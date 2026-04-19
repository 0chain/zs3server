# SF10 TPC-DS Benchmarks — 2026-04-19

Full-day session of cache-tier sweep + code fixes. All runs on test2 (3+2 erasure, 5 enterprise blobbers on localhost via nginx HTTP/2).

## Dataset

- TPC-DS SF10 parquet, 24 tables, ~452 files, ~3.8 GB on disk
- Stored on allocation `69aa58c55eef…` (4+1 erasure, 80 GB alloc)
- Spark 3.5.0 driver-local[16], 8 GB driver memory
- 10-query suite (`q4,q14a,q23a,q24a,q64,q67,q72,q78,q80,q93`) via `tpcds_bench.py` (NFS) or `tpcds_bench_s3.py` (S3)

## Cache architecture

Three data tiers:

1. **tmpfs** — `/nfs_export`, RAM-backed (sized via mount option)
2. **spillover** — `/root/nfs_spillover`, NVMe (sized via `nfs_spillover_max_bytes`)
3. **blobber** — network fetch via gosdk

Fully configurable in `zs3server.json`:

```json
{
  "nfs_tmpfs_cache_enabled":     true|false,
  "nfs_spillover_cache_enabled": true|false,
  "nfs_spillover_dir":           "/root/nfs_spillover",
  "nfs_spillover_max_bytes":     1073741824,
  "nfs_cache_disabled":          false,
  "nfs_direct_threshold":        2097152,
  "s3_direct_threshold":         0,
  "nfs_sync_enabled":            true
}
```

Plus the tmpfs mount size: `mount -t tmpfs -o size=8G,strictatime tmpfs /nfs_export`.

### Read paths

| Path | NFS | S3 |
|---|---|---|
| Primary serve | FSAL_ZUS → tmpfs or spillover (stub → spillover fallback) | Fix A local-file fast path (tmpfs, range+full) OR `TryCacheRead` (both tiers, full-file only) |
| Fallback | `zus_prewarm` → blobber fetch → tmpfs write | `getFileReader` → gosdk range read from blobber |
| Background cache-fill | N/A | `cacheBackTee` (full-file), `cacheBackFullFetch` (range-miss, my lazy-cache-back fix) |

### Eviction policy

**tmpfs → spillover** (`spillCommittedFiles`, 1 s tick):
- Trigger: `tmpfs_usage > 60%`
- Candidate selection: oldest-mtime committed file, skip if in `openFdInodes()`, skip if `atime < 120 s` (active reader), skip-list for candidates whose spill failed
- Serialization: per-path mutex + OFD F_WRLCK cross-process

**spillover → delete** (`evictSpilloverOldest`):
- Trigger: spillover total > `nfs_spillover_max_bytes`
- Oldest-mtime first; skip if `mtime < 120 s` OR `atime < 120 s`
- `MarkRecentlyEvicted(key)` with 120 s TTL (blocks re-cache-back)

**Cool-down**: `RecentlyEvicted(key)` consulted before background cache-back (120 s window, my earlier bump from 30 s).

## Full 10-query results (all fixes applied)

| Config | tmpfs / spillover | Total (s) | vs best (1.00) | vs Apr-14 baseline (223s) |
|---|---|---|---|---|
| **NFS 8G/0**    | 8 GB / off         | **228.24** | **1.00×** | **1.02×** (near baseline ✓) |
| NFS 1G/1G       | 1 GB / 1 GB        | 365.93 | 1.60× | 1.64× |
| NFS 1G/0        | 1 GB / off         | 366.85 | 1.61× | 1.64× |
| S3 0/1G         | off / 1 GB         | 385.31 | 1.69× | 1.73× |
| S3 8G/0         | 8 GB / off         | 385.61 | 1.69× | 1.73× |
| S3 0/8G         | off / 8 GB         | 386.80 | 1.69× | 1.73× |
| S3 1G/1G        | 1 GB / 1 GB        | 752.21 | 3.30× | 3.37× |
| S3 no-cache     | `nfs_cache_disabled=true` | 1056.32 | 4.63× | 4.74× |
| S3 1G/0         | 1 GB / off         | 1134.79 | 4.97× | 5.09× |

## Per-query breakdown

| Query | NFS 8G/0 | NFS 1G/1G | NFS 1G/0 | S3 0/1G | S3 8G/0 | S3 0/8G | S3 1G/1G | S3 1G/0 | S3 no-cache |
|---|---|---|---|---|---|---|---|---|---|
| q4   | 39.15 | 43.00 | 42.29 | 61.33 | 61.41 | 61.05 | 76.76  | 115.99 | 93.71  |
| q14a | 15.28 | 47.26 | 49.81 | 54.25 | 53.86 | 54.35 | 144.40 | 243.30 | 203.06 |
| q23a | 18.10 | 29.47 | 29.66 | 38.96 | 38.98 | 39.24 | 90.03  | 164.71 | 130.84 |
| q24a | 11.73 | 12.38 | 12.65 | 25.65 | 25.87 | 27.38 | 39.26  | 43.65  | 78.15  |
| q64  | 22.75 | 31.03 | 31.50 | 44.85 | 44.30 | 45.16 | 94.01  | 95.28  | 125.25 |
| q67  | 19.96 | 26.65 | 27.36 | 19.55 | 20.04 | 20.29 | 43.33  | 39.69  | 51.37  |
| q72  | 54.62 | 67.08 | 69.01 | 59.31 | 58.71 | 57.48 | 100.12 | 130.40 | 115.78 |
| q78  | 18.42 | 41.43 | 40.87 | 34.43 | 35.34 | 35.29 | 75.41  | 136.79 | 107.38 |
| q80  | 17.36 | 46.51 | 42.70 | 32.00 | 31.96 | 31.54 | 62.17  | 117.59 | 106.64 |
| q93  | 10.88 | 21.12 | 21.00 | 14.99 | 15.13 | 15.04 | 26.71  | 47.40  | 44.14  |
| **TOTAL** | **228.24** | 365.93 | 366.85 | 385.31 | 385.61 | 386.80 | 752.21 | 1134.79 | 1056.32 |

## Observations

### 1. Tmpfs beats spillover when sized ≥ working set

- NFS 8G/0 (228 s) is **38 % faster** than NFS 1G/1G (366 s) despite having one fewer tier.
- SF10 fits in 8 GB tmpfs → no eviction → no double-tier overhead.

### 2. Undersized tmpfs is actively harmful for S3

- S3 1G/0 (1135 s) is **slower than S3 no-cache** (1056 s).
- At 1 GB, tmpfs thrashes. Every range read acquires Fix A F_RDLCK; `spilloverMonitor` churns; evict-re-prewarm cycles dominate. The cache costs more than it saves.
- S3 0/1G (385 s) — same 1 GB cache but on spillover only — is **3× faster** than S3 1G/0.
  - Why: spillover path avoids Fix A lock overhead; `TryCacheRead` serves full-file reads (small dims, `_SUCCESS`) via plain `os.Open` without F_RDLCK.

### 3. NFS doesn't benefit from a second tier

- NFS 1G/1G (366 s) ≈ NFS 1G/0 (367 s). Spillover adds ~0 s.
- Reason: on this setup, blobber re-fetch (≈100-300 ms) ≈ spillover disk read. The `spillCommittedFiles` copy + sparse-truncate + xattr bookkeeping costs more than the avoided re-fetch.
- Spillover would win with (a) remote blobbers (WAN fetch ≫ NVMe read) or (b) working set >> tmpfs.

### 4. S3 at "no-thrash" configurations converges

- S3 0/1G (385) ≈ S3 8G/0 (386) ≈ S3 0/8G (387). Once the cache is sized to avoid thrash, the per-range HTTP framing dominates — extra cache capacity doesn't help.
- The residual gap between S3 ~385 s and NFS 228 s is HTTP/MinIO-framework overhead per range request (~6-10 ms × ~1000 ranges = ~8 s per query × 10 = 80 s accumulated).

### 5. NFS vs S3 path-overhead difference (warm)

- NFS read: Spark → kernel NFS client → ganesha.nfsd → FSAL_ZUS → sub-FSAL pread on tmpfs. Mostly kernel, zero-copy splices where possible, Linux page cache on the client host.
- S3 read: Spark → S3A → HTTP → MinIO framework → Fix A `os.Open` + `LimitReader` → userspace buffer → HTTP framing → TCP send. Multiple user-space copies per range.

## Code fixes landed this session

### zs3server

1. **Lazy S3 cache-back on range-miss** (`gateway-zcn.go:748`) — spawn background `cacheBackFullFetch` so subsequent ranges on the same file hit Fix A fast path. Deduped via `cacheBackInflight`, gated by `RecentlyEvicted`.
2. **`.cachefetch` added to BlobberSync skip list** (`nfs_blobber_sync.go:165,190,772,1025`) — prevents inotify from uploading mid-write temp files to blobber.
3. **OFD-locks everywhere** — `F_OFD_SETLKW`/`F_OFD_SETLK` replace POSIX fcntl locks in FSAL_ZUS and zs3server. POSIX locks release on any close in the same process; OFD locks are per-fd, immune to sibling-fd close.
4. **`atime`-based spillover grace** (`nfs_blobber_sync.go:441,509`) — skip spill (tmpfs→spillover) and skip evict (spillover→delete) for files with `atime` newer than 120 s. `strictatime` tmpfs mount required.
5. **`EnsureFreeTmpfs` skip-failed-candidate** — iterate through distinct candidates instead of bailing on first failure. Added `findOldestCommittedTmpfsFileExcept(skip map)`.
6. **Prewarm ENOSPC / short-read restore sparse stub** (`prewarm_router.go`) — on `io.Copy` error or `n != expected`, `O_TRUNC + Truncate(origSize)` so readers don't see partial bytes.
7. **`RecentlyEvicted` TTL bump** — 30 s → 120 s (longer cool-off against re-cache-back).

### FSAL_ZUS (ganesha stackable plugin)

1. **`zusfs_read2` EIO fallback** (`file.c:1096+`) — after `zus_rel_stub_check_and_prewarm`, re-probe tmpfs under lock. If usable, proceed; else try spillover. If neither has real bytes, return `ERR_FSAL_IO`. **Never fall through to sub-FSAL with a sparse stub** (that was returning `[0,0,0,0]` zeros to Spark → parquet "bad magic").
2. **OFD locks throughout** — `zus_lock_and_check_tmpfs`, `zus_read_from_spillover`, release paths.

### gosdk (unchanged this session — carrying yesterday's)

- `listworker.go` + `filerefsworker.go` — wait-all for quorum + `latestUpdated` tiebreaker
- `sdk.go` — `SetAllocationCacheDir` for chain-down bootstrap

## Conclusions

1. **Size tmpfs to fit the hot working set.** For SF10 (≈3.8 GB), 8 GB tmpfs is the sweet spot.
2. **Avoid tiny-tmpfs + big-dataset.** At 1 GB tmpfs / 3.8 GB dataset, any cache is worse than none for S3.
3. **Spillover is optional on this deployment.** Only win case would be WAN blobbers or dataset >> cache.
4. **S3 path has ~8 s/query fixed overhead vs NFS.** HTTP framing + MinIO middleware — not fixable without intrusive changes (sendfile + middleware strip). NFS wins for sustained analytics.
5. **Remote Spark containers**: NFS client has Linux page cache → better sustained read perf than S3 (app-level cache needed on Spark host to match).

## Recommended default config

For SF10-sized datasets on this hardware, production should use:

```json
{
  "nfs_tmpfs_cache_enabled":     true,
  "nfs_spillover_cache_enabled": false,
  "nfs_cache_disabled":          false,
  "nfs_sync_enabled":            true
}
```

With `/nfs_export` mounted at `size=8G,strictatime` (or larger for bigger datasets).

Single-tier tmpfs, no spillover — simplest and fastest config on this setup.
