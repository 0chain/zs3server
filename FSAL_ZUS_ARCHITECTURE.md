# NFS Gateway — FSAL_ZUS + zs3server integration

## Problem

Züs (0chain) stores data as erasure-coded shards across blobbers behind a consensus
protocol. Analytics and ML workloads need both S3-API *and* POSIX filesystem access
to the same bucket without copying data twice.

The previous `feat/enterprise-timings` stack used **NFS-Ganesha + FSAL_VFS** as a
pass-through to a tmpfs-backed cache (`/nfs_export`), with `BlobberSync` (inotify
watcher) asynchronously pushing writes into Züs. This worked only for writes —
**NFS reads that missed `/nfs_export` returned `ENOENT`** because FSAL_VFS has no
hook to fetch from blobbers on miss.

## Architecture

```
  +--------------------------------------------------------+
  | Client (Spark / ClickHouse / PyTorch / rsync / fio)    |
  +--------------------------------------------------------+
         |                                            |
         |  POSIX mount                               |  S3 API
         v                                            v
  +-------------------+                       +---------------------+
  | NFS client        |                       | mc / aws-cli / warp |
  +---------+---------+                       +----------+----------+
            |                                            |
            | NFSv4.2 (nconnect=16, rsize/wsize=1M)      |
            v                                            v
  +-------------------+                       +---------------------+
  | NFS-Ganesha       |     /etc/ganesha      | zs3server           |
  | + FSAL_ZUS (new)  | <----> ganesha.conf   | MinIO Gateway (zcn) |
  | stacked on VFS    |                       | :9100               |
  +---------+---------+                       +----------+----------+
            |                                            |
            | POSIX passthrough                          | gosdk
            v                                            v
  +-------------------+ <-- /internal/prewarm --- +------+----------+
  | /nfs_export       |     /internal/list        | Züs blobbers    |
  | (tmpfs 8 GB)      | <-- BlobberSync inotify   | (erasure-coded) |
  +-------------------+      (writes)             +-----------------+
            ^
            |  S3 fallback on Züs 404 (optional, config-gated)
            |
  +-------------------+
  | Upstream S3       |
  | (AWS / MinIO)     |
  +-------------------+
```

### Data flow — read (new in FSAL_ZUS)

1. Client calls `open(/mnt/zus_nfs/tpcds10/foo.parquet)`.
2. NFS-Ganesha dispatches to `FSAL_ZUS.open2`.
3. FSAL_ZUS delegates to the stacked `FSAL_VFS.open2` (the underlying export).
4. If VFS returns `ERR_FSAL_NOENT` (file not in `/nfs_export` cache):
   - FSAL_ZUS `POST`s to `http://localhost:9100/internal/prewarm` with `{bucket, key}`.
   - zs3server calls the existing `getFileReader(bucket, key)` path via gosdk.
   - Stream from blobbers goes into `/nfs_export/<bucket>/.prewarm-<uuid>` and is
     `os.Rename`d to `/nfs_export/<bucket>/<key>`.
   - `BlobberSync.MarkCommitted(relPath)` is called before rename so the inotify
     `Create` event is suppressed (no re-upload loop).
5. FSAL_ZUS retries `FSAL_VFS.open2` — now succeeds.

### Data flow — readdir (new in FSAL_ZUS)

`ls` / `glob` / `listStatus` need a *union* of local `/nfs_export` listing and the
blobber-side listing, otherwise apps that discover files via directory scan miss
anything not yet cached.

1. Client calls `readdir(/mnt/zus_nfs/tpcds10/store_sales/)`.
2. FSAL_ZUS delegates to `FSAL_VFS.readdir` (returns files currently in `/nfs_export`).
3. FSAL_ZUS `GET`s `http://localhost:9100/internal/list?bucket=X&prefix=Y&stub=1`.
4. zs3server calls `alloc.ListDir("/" + bucket + "/" + prefix)` and for each entry
   not already present in `/nfs_export`, creates a **sparse placeholder** with the
   real file size via `truncate(path, ActualSize)` and sets the xattr
   `user.zus.stub=1`.
5. FSAL_ZUS returns the union. The stub files show up in NFS listings with correct
   sizes but zero disk blocks.
6. When a client `open`s a stub, FSAL_ZUS's `open2` pre-check reads the xattr,
   detects stub, calls `/internal/prewarm` which fetches real content and
   overwrites the stub, then removes the xattr and retries the open.

### Data flow — write (unchanged from feat/enterprise-timings)

Client write → NFS → Ganesha → FSAL_VFS → `/nfs_export` → inotify → BlobberSync
→ Züs blobbers. Writes above `nfs_direct_threshold` bypass the cache.

## Hardening

### BlobberSync stub-skip xattr check
A sparse placeholder created by `/internal/list?stub=1` must never be uploaded to
blobbers; otherwise it would overwrite real allocation content as a 32-byte
(metadata-size-only) file.

`nfs_blobber_sync.go:processEvents` now does:
```go
var stubbuf [2]byte
if n, _ := syscall.Getxattr(event.Name, "user.zus.stub", stubbuf[:]); n > 0 {
    continue // never commit a stub
}
```
Defense in depth — `MarkCommitted` alone was insufficient because the fsnotify
`Create` event fires before the stub-creating caller has populated the `committed`
map.

### GatewayExtraRouters → before STS
Previously: `registerSTSRouter(router)` ran first. STS's subrouter uses
`PathPrefix("/")` + `MatcherFunc(POST && Content-Type: form-urlencoded && no queries)`.
gorilla/mux evaluates routes in registration order; the STS catch-all was hijacking
`POST /internal/prewarm` whenever the client omitted `Content-Type: application/json`.
Fixed by moving `for _, h := range GatewayExtraRouters { h(router) }` **before**
`registerSTSRouter` in `cmd/gateway-main.go`.

### Singleflight dedup
Both `/internal/prewarm` and the S3-upstream fallback use
`golang.org/x/sync/singleflight` keyed on `bucket/key`. Concurrent cold-miss reads
produce exactly one blobber/upstream fetch; followers share the result.

## Measured results (SF=10 TPC-DS, 3.8 GB parquet, 452 files, 4+1 allocation)

### TPC-DS (10 queries: Q4, Q14a, Q23a, Q24a, Q64, Q67, Q72, Q78, Q80, Q93)

| Path | Total time | Overhead vs NVMe |
|---|---|---|
| NVMe local (baseline)         | 180.05s | — |
| S3-API cold (`s3a://…`)        | 223.08s | +24% |
| S3-API warm (/mcache)          | 221.43s | +23% |
| NFS via FSAL_ZUS (prewarmed)   | 222.24s | +23% |

Warm ≈ cold because the workload is CPU-bound at SF=10 — Spark's JVM buffer pool
already covers the hot footer reads. The 24% overhead is the S3/HTTP/consensus
constant, not blobber bandwidth.

### fio — NFS (FSAL_ZUS → tmpfs) vs NVMe direct

| Workload | NFS | NVMe direct | NFS / NVMe |
|---|---|---|---|
| seq_read 4k        | 18 MB/s | 1061 MB/s | 1.7 % |
| seq_read 64k       | 188 MB/s | 1271 MB/s | 15 % |
| seq_read 1m        | **577 MB/s** | 1294 MB/s | 45 % |
| rand_read 4k qd=1  | 15.6 MB/s / 250 µs | 73 MB/s / 52 µs | 21 % |
| rand_read 4k qd=32 | 43.8 MB/s / 2.8 ms | 74 MB/s / 1.7 ms | 59 % |

NFS per-call latency dominates at small block sizes (~250 µs floor vs 52 µs direct).
Large-block sequential reaches ~45 % of NVMe — acceptable for ML/analytics.

### Prewarm throughput
`curl -X POST /internal/prewarm` × 452 files at `xargs -P 16` → **547-624 MB/s
sustained** Züs → `/nfs_export` (3.8 GiB in 6-7 s). Represents the
zs3server → gosdk → blobbers end-to-end bandwidth under 16-way concurrency.

### Cold single-file NFS read (stub → prewarm → retry)
1.9 MB file, 389 ms end-to-end. Breakdown:
- NFS → Ganesha → FSAL dispatch: ~1 ms
- libcurl POST to zs3server: ~1 ms
- zs3server → gosdk → blobbers (consensus + erasure decode): **~380 ms**
- rename + retry: ~5 ms

Blobber round-trip dominates; FSAL overhead is negligible.

## Endpoints added

### `POST /internal/prewarm`
Request: `{"bucket":"X","key":"Y"}`
Response 200: `{"path":"/nfs_export/X/Y","size":N}`
Response 404: `{"error":"not found"}`
Response 503: `{"error":"..."}`

Idempotent fast path when file already exists and has no stub xattr.

### `GET /internal/list?bucket=X&prefix=Y[&stub=1]`
Returns JSON `{"entries":[{"name","size","is_dir","mtime"},...],"stubbed":N}`.
With `stub=1`, creates sparse placeholders in `/nfs_export` + sets
`user.zus.stub` xattr. 30 s TTL cache keyed on `remotePath|stub=<bool>`.

## S3-upstream fallback (config-gated)

When zs3server returns 404 from Züs, optionally fetch from a configured upstream
(AWS S3, another MinIO) and tee the bytes into Züs for subsequent hits.

`zcnconfig/zs3server.json`:
```json
{
  "fallback_s3_enabled": true,
  "fallback_s3_endpoint": "https://s3.amazonaws.com",
  "fallback_s3_region": "us-east-1",
  "fallback_s3_access_key": "...",
  "fallback_s3_secret_key": "...",
  "fallback_s3_use_ssl": true,
  "fallback_bucket_map": {"pubdata": "commoncrawl"}
}
```

Disabled by default — zero regression on pure-Züs installs. Uses minio-go client
with `io.Pipe` + `TeeReader` so the client streams upstream bytes immediately
while a background goroutine uploads to Züs via `putFile`. Singleflight
deduplicates concurrent fallback requests for the same key.

This replaces the hypothetical `/router` repo's fallback role in the original
architecture. When/if a compute-side Router is built, its upstream should be
zs3server, which becomes the single point of Züs + external-S3 dedupe.

## Known issues (filed for future fixes)

1. **mc mirror data corruption at high concurrency** — default concurrency in
   `mc mirror` produces corrupt/swapped content; `-P 4` via xargs is clean.
   Likely race in zs3server's concurrent multipart-upload path. Reproducer TBD.

2. **FSAL_ZUS `open2` xattr pre-check** — works in happy-path tests but didn't
   fire across a full Spark workload in one run. Probably `handle->rel_path` not
   set in all open-by-handle code paths. The ENOENT-on-lookup prewarm path is
   unaffected.

3. **gosdk `alloc.ListDir` field naming** — `Size` is per-shard metadata (32 B);
   `ActualSize` is the real content size. `list_router.go` already prefers
   `ActualSize` with a fallback. Worth documenting in gosdk.

## FSAL_ZUS plugin

The Ganesha plugin lives separately (it patches the upstream Ganesha source tree).
Planned location: **new repo `0chain/fsal-zus`** — standalone, fetches ganesha
headers at build time. Not in zs3server subtree.

For now the source is at
`/root/nfs-ganesha-source/nfs-ganesha/src/FSAL/Stackable_FSALs/FSAL_ZUS/` on the
test2 deployment. ~270 LOC of new C on top of a copied FSAL_NULL skeleton.
Builds as `libfsalzus.so`, drops into `/usr/lib/x86_64-linux-gnu/ganesha/`.

## Recommended NFS mount options

See `FSAL_ZUS_MOUNT.md` in this repo.
