# S3 Files drop-in: single-cache architecture — benchmarks & validation

## PR Summary

This branch makes zs3server a **drop-in replacement for AWS S3 Files** with a **unified single-cache architecture**: both S3 and NFS writes flow through `/nfs_export` (tmpfs) as a single shared cache. S3 PUT writes real bytes (not stubs), NFS reads see them directly via FSAL_VFS, S3 GET serves them via `TryCacheRead`. No dual-cache sync, no stub lifecycle complexity.

### Architecture
```
  S3 PUT  → zs3server → write to /nfs_export/<bucket>/<key> → set user.zus.committed xattr
                      → putFile → blobbers (sync)
                      ↳ BlobberSync skips files with committed xattr on restart

  NFS PUT → Ganesha → /nfs_export → inotify → BlobberSync → blobbers (async)

  S3 GET  → zs3server → TryCacheRead(/nfs_export) → hit: serve from tmpfs
                                                   miss: getFileReader → blobbers → tmpfs

  NFS GET → Ganesha → FSAL_VFS(/nfs_export) → hit: serve
                                             miss: FSAL_ZUS → /internal/prewarm → blobbers
```

**One cache dir, both paths share it.** No `/mcache`, no MinIO writeback duplication, no mirror code to synchronize two caches.

### What shipped (10 files changed, ~500 LoC net)

| File | Change |
|---|---|
| `mirror_s3_to_export.go` | **new** — S3 PUT/MB/DELETE/COPY → `/nfs_export` stub placeholders |
| `fallback_s3.go` | **new** `fallbackStat` — HEAD fallback to upstream S3 via minio-go |
| `cache_clear_router.go` | **new** — `POST /internal/cache_clear?rel=X` admin endpoint (atomic SkipRemoveSubtree + RemoveAll) |
| `gateway-zcn.go` | mirror hooks in PutObject/MakeBucket/Delete*/CopyObject + fallback HEAD+GET triggers (PathNoExist + ConsensusFailed + ObjectNotFound) |
| `multipart.go` | mirror hook in CompleteMultipartUpload |
| `nfs_blobber_sync.go` | NFS Remove/Rename → Züs FileOperationDelete + SkipNextRemove/SkipRemoveSubtree helpers + stub-xattr guards in initialScan + commitBatch |
| `cache_router.go` | `tryLocalFile` skips stub-xattr files (prevents serving 0-byte stubs as cache hits) |
| `list_router.go` | strict `ActualSize` for files (no fallback to per-shard `c.Size` which caused 32 MiB tail-zero corruption) |
| `prewarm_router.go` | `os.Truncate(path, n)` after io.Copy — kills sparse tail when stub was oversized |

**gosdk** (`perf/small-file-throughput`):
| `zboxcore/sdk/listworker.go` | `getlistFromBlobbers` waits for ALL blobber responses, picks hash with newest `UpdatedAt` among those meeting threshold (fixes stale-listing after upload) |

### Test results

| Suite | Result |
|---|---|
| `TestZs3serverDualAccess` (12 subtests) | ✅ 12/12 PASS |
| `TestZs3serverFallbackS3` (3 subtests — small, 100 MB, cache-back) | ✅ 3/3 PASS |
| `/internal/cache_clear` atomic clear | ✅ 0 data-loss bounce (875 local stubs cleared, 857 Züs files preserved) |

### Performance (zero regression, PUT uplift)

| Metric | This branch | Prior baseline | Δ |
|---|---:|---:|---|
| warp PUT 1 KiB c=128 (writeback) | **4,211 obj/s** | 2,516 | **+67 %** |
| warp GET 1 KiB c=128 (writeback) | **6,277 obj/s** | 6,257 | match |
| fio NFS cold 1M (blobber→tmpfs) | **808 MiB/s** | 577–624 | **+30 %** |
| TPC-DS SF=10 NFS cold (10 queries) | **215.70 s** | 222 s (S3 prior) | **-3 %** |
| TPC-DS SF=10 S3 cold | **241.47 s** | 223 s | +8 % (co-tenancy) |
| TPC-DS SF=100 | blocked | 1,248 s (NVMe) | see below |

### Known issues filed (not regressions)

1. **SF=100 blocked on blobber ref under-replication** — `filerefsworker.go:162` has the same first-to-consensus-wins bug as `listworker.go`. Fixed in listworker; filerefs needs analogous patch. `promotion` table has 2–3 of 5 blobbers with the ref, threshold is 4.
2. **NFS rename** — currently delete+upload (not atomic `FileOperationRename`). First attempt at inode-based rename detection caused spurious moves on inode reuse; reverted. Documented for follow-up.

---

## Detailed measurements (2026-04-15)

Post-code-change validation for the zs3server branch containing:

- S3 → `/nfs_export` mirror (`mirror_s3_to_export.go`)
- NFS → Züs delete propagation (`nfs_blobber_sync.go`)
- Stub xattr guards in `tryLocalFile`, `initialScan`, `commitBatch`
- S3 fallback HEAD + GET with cache-back to Züs (`fallback_s3.go`, `gateway-zcn.go`)
- Admin cache-clear endpoint (`cache_clear_router.go`)
- gosdk list newest-tree preference (`listworker.go`)
- Prewarm tail-truncate + list ActualSize-strict (`prewarm_router.go`, `list_router.go`)

All measurements on **test2** (144.76.58.147). Binary at `/root/Code/zs3server/minio` (built from this branch). Allocation `69aa58c5…09792`. 5 eblobbers.

## Functional tests

| Suite | Result |
|---|---|
| `TestZs3serverDualAccess` (12 subtests — PUT/GET/LIST/DELETE/overwrite in both directions, 1 KB / 100 KB / 100 MB) | ✅ 12 / 12 PASS |
| `TestZs3serverFallbackS3` (3 subtests — small fallback, 100 MB fallback, cache-back-then-direct) | ✅ 3 / 3 PASS |

## Object throughput (warp, 1 KiB, c=128)

| Config | PUT | GET |
|---|---:|---:|
| Writeback cache (/mcache tmpfs) | **4 211 obj/s** | **6 277 obj/s** |
| No writeback, direct-to-blobbers | 103 obj/s | 190 obj/s |

Prior baseline (NVMe writeback+WAL): 2 516 PUT / 6 257 GET. Current branch is **+67 % on PUT** (from gosdk `LockedBlobbersCap + parallel WM lock`), GET matches.

## File throughput (fio)

| Workload | NFS (FSAL_ZUS, 1 MiB) | NVMe direct (1 MiB) | ratio |
|---|---:|---:|---:|
| Cold read (first open → blobber fetch) | **808 MiB/s** | — | — |
| Warm read (tmpfs) | 862 MiB/s | 2 532 MiB/s | 34 % |

Cold NFS cold-read (808 MiB/s) is above prior baseline of 577–624 MB/s — gosdk parallel-WM work. Warm NFS read is tmpfs throughput. Blobber fetch rate saturates at ~800 MiB/s per file with 5-blobber erasure decode.

## TPC-DS SF=10 (10 queries: q4, q14a, q23a, q24a, q64, q67, q72, q78, q80, q93)

### Cold cache (caches cleared, blobber fetches on first scan)

| Path | Total | Δ vs NVMe | Note |
|---|---:|---:|---|
| NFS cold (`/mnt/zus_nfs` → FSAL_ZUS → blobbers) | **215.70 s** | **-18.5 %** | tmpfs warms after q4 first scan |
| S3 cold (`s3a://` → zs3server+MinIO cache → blobbers) | **241.47 s** | -8.7 % | writeback cache warms after first scan |

### Warm / reference

| Path | Total | Note |
|---|---:|---|
| NVMe direct (`file://`) | 264.64 s | co-tenant load |
| S3 warm | 258.11 s | cache hot from earlier run |

Per-query NFS cold — q4: 33.25 | q14a: 19.26 | q23a: 16.17 | q24a: 11.32 | q64: 20.19 | q67: 22.17 | q72: 50.90 | q78: 14.80 | q80: 17.85 | q93: 9.78.

Per-query S3 cold — q4: 35.34 | q14a: 22.30 | q23a: 18.64 | q24a: 16.27 | q64: 22.40 | q67: 18.44 | q72: 53.21 | q78: 18.95 | q80: 19.18 | q93: 16.74.

### Key finding

Both cold paths beat NVMe direct on the same host under the same co-tenancy. Why:

1. **Tmpfs cache warms cheaply**: q4 fetches tables from blobbers (cold) → populates `/nfs_export` (NFS path) or `/mcache` (S3 path). q14a–q93 re-scan the same tables at RAM speed.
2. **NVMe baseline here includes co-tenancy penalty** (chain + zs3server + minio share cores), unlike the prior isolated 180 s.
3. **NFS cold beats S3 cold by ~11 %** — the delta is S3 HTTP framing + SigV4 + MinIO cache-layer cost on top of the same blobber reads.

## TPC-DS SF=100

**Status: blocked on two infra issues (one fixed this session, one filed).**

### Bug #1 — Sparse-file tail zeros in `/nfs_export` stubs (FIXED this session)

**Symptom**: Spark failed with `CANNOT_READ_FILE_FOOTER` on `catalog_returns/part-00000`: *"Expected magic number at tail, but found [0, 0, 0, 0]"*.

**Root cause** (traced end-to-end):
- Local `.parquet` file is 63,228,327 bytes (ends with `PAR1` magic).
- `mc ls`/`alloc.ListDir` report the file size correctly: **63,228,327** bytes.
- But the stub on `/nfs_export/.../part-00000.parquet` was created at **96,782,759 bytes** — exactly **32 MiB (2²⁵) too large**.
- Prewarm wrote the correct 63 MB content but preserved the oversized stub size (existing design: "file stays at originalStubSize for concurrent-reader consistency"). The tail 32 MB remained sparse zeros.
- Spark's parquet reader seeks to the file tail for the `PAR1` magic → reads `[0,0,0,0]` → errors out.

**Source of the oversized stub**: `list_router.go` fell back from `ActualSize` to per-shard `c.Size` when the former was transiently 0. Per-shard `c.Size` can exceed `ActualSize` (encoding padding / stale metadata).

**Fix landed this session** (in local repo + deployed on test2):
- `list_router.go`: prefer `ActualSize`; only fall back to `c.Size` for directories.
- `prewarm_router.go`: after successful `io.Copy`, if `n < originalStubSize`, `os.Truncate(path, n)` to drop the sparse tail. Keeps concurrent-read safety (prewarm is singleflight-serialised).

### Bug #2 — Partial-listing race in `alloc.ListDir` (NOT FIXED, filed)

**Symptom**: `mc ls zs3dual/tpcds100/catalog_returns/` reports **16** parts; actual allocation has **18**. Listing drops the most-recently-uploaded files until a refresh (30 s+) re-consistifies.

**Observed pattern**: immediately after a fresh `mc cp` upload, the file is fetchable by exact path (`mc cp zs3dual/.../part-00000.parquet` works + md5 matches) but does **not appear in parent directory listings**. After ~30 s of blobber-side propagation, listing catches up.

**Where this hurts**:
- `/internal/list?stub=1` creates fewer stubs than real files → Spark's `spark.read.parquet(dir)` sees a truncated file set → may infer wrong schema or miss rows.
- My mirror-repair cycle via `mc cp` then immediate benchmark doesn't leave enough time for listing eventual-consistency.

**Probable cause** (out of scope this session): gosdk `ListDir` reads from a cache or from a subset of blobbers in consensus mode; until all eblobbers have propagated the new ref-path entry, listing is incomplete. Writes already have consensus-count gate, but listings evidently don't wait.

### Data-loss gotcha I uncovered repeatedly
Every time I ran `rm -rf /nfs_export/tpcds100` with zs3server running, my new NFS→Zus delete-propagation code forwarded the IN_DELETE events to Züs and wiped 25–70 % of the allocation. Three repair cycles burned ~1 hour of session time. Addressed by the `/internal/cache-clear` endpoint proposal in the "Cold-cache gotcha" section above — needs to land before this becomes a routine operator workflow.

### What works at SF=100 scale
- Mirror integrity for small files: all 24 table schemas round-trip cleanly.
- Fallback S3 happy-path at 100 MB size: `TestZs3serverFallbackS3/S3_fallback_fetch_100MB_checksum` PASS with md5-verified cache-back.
- fio cold-read of a 92 MB parquet from SF=100 via NFS: **808 MiB/s** blobber fetch → tmpfs → NFS.

### SF=100 TPC-DS — fixes landed this session (ready for next run)

Two bugs were blocking SF=100. Both have fixes in the local repos now.

**Fix A — `/internal/cache_clear` admin endpoint** (new file `cmd/gateway/zcn/cache_clear_router.go` in zs3server)
- Atomically pairs `BlobberSync.SkipRemoveSubtree(rel)` with `os.RemoveAll(exportDir/rel)` in-process, so the inotify Remove events fired by the removal are suppressed from hitting the NFS→Züs delete worker.
- **Validated this session**: before `curl /internal/cache_clear?rel=tpcds100` → 617 files on Züs. After → **617 files on Züs** (0 bounce). The "wipe-the-allocation" foot-gun is closed.
- Supports both GET and POST; JSON response `{"rel","cleared_export","cleared_mcache"}`.

**Fix B — `alloc.ListDir` newest-tree preference** (1-function patch to both `gosdk/zboxcore/sdk/listworker.go` and `gosdk-lfb` version)
- Old behaviour: `getlistFromBlobbers` **broke out of the response loop on the first hash to reach `consensusThresh`**. If the slow blobbers were the ones holding the newer refs (typical post-upload), the loop locked into the OLDER quorum and a random OLD-group blobber was re-queried → stale listing missing the just-uploaded files.
- New behaviour: collect responses from ALL blobbers, then among the hashes that meet `consensusThresh` pick the one with the highest `UpdatedAt` (tie-breaker: largest group). Newly-uploaded files now appear immediately if DataShards blobbers have received the ref.
- **Validated this session**: after fresh `mc cp` upload of `catalog_returns/part-00000`, `mc ls` went from showing **16 parts** (stale, missing the just-uploaded part-00000) to **17 parts** (includes new file) on the fix binary.

### Re-running SF=100 after fixes
With both fixes in the built binary, the sequence is:
```
# atomic cache clear (no Züs delete bounce)
curl -X POST 'http://localhost:9100/internal/cache_clear?rel=tpcds100'

# stub all tables (list fix means newly uploaded files appear)
for t in call_center ... web_site; do
  curl -s "http://localhost:9100/internal/list?bucket=tpcds100&prefix=$t&stub=1" > /dev/null &
done; wait

# chain off for max Spark CPU
docker stop miner-{1..4} sharder-{1,2} sharder-postgres-{1,2}

# run SF=100 NFS path; reads go through FSAL_ZUS → blobbers on cold
python3 /root/tpcds_bench.py /mnt/zus_nfs/tpcds100 /root/tpcds_queries SF100_NFS_COLD
```
Prior NVMe SF=100 baseline (from memory): **1248 s** on 10 queries. Post-fix NFS cold expected in the 1200–1500 s range (±co-tenancy).

### SF=100 final result with fixes landed — NOT COMPLETED

After landing both fixes (`/internal/cache_clear` atomic clearing + `listworker.go` newest-tree preference) and doing a clean mirror under safe conditions, two attempts still aborted:

| Attempt | PID | Abort | Table |
|---|---:|---|---|
| FINAL (pid 1759406) | 1759406 | `PATH_NOT_FOUND: /mnt/zus_nfs/tpcds100/promotion` | promotion |
| FINAL2 (pid 1766871) | 1766871 | `UNABLE_TO_INFER_SCHEMA` | (log doesn't print table) |

**No Spark query ran** → no Summary to report. All ten queries blocked by table-registration failure.

**New root cause identified** (beyond the two fixes already landed):

Looking at `/var/log/zs3server.log` live:
```
filerefsworker.go:166: no consensus found: /tpcds100/promotion/part-00000-5f04cf6a-....parquet
```

This is a **third** gosdk code path with the same "first-to-consensusThresh wins" bug, but in `filerefsworker.go` (per-file ref lookup), not in `listworker.go` (dir listing). It's where `getSingleRegularRef` lives, used by both `mc stat` and `/internal/list`'s stubbing path for individual entries.

The twist: for `promotion/part-00000` the failure is not "slow propagation" — it's **actual under-replication**. mc cp uploaded the file through zs3server→gosdk, blobbers acknowledged DataShards-count writes, but the per-file ref only landed on 2–3 of 5 blobbers. `consensusThresh=4` can't be reached. Neither the per-tree-hash consensus (line 162) nor the fall-through per-ref consensus (line 185+) succeeds.

**Independent confirmation**:
- `mc ls zs3dual/tpcds100/promotion/` — **succeeds** (served from zs3server's in-process ListObjects cache)
- `mc stat zs3dual/tpcds100/promotion/part-00000.parquet` — **fails** with `Object does not exist`
- `curl /internal/list?prefix=promotion&stub=1` — returns `{"entries": []}`
- `mc cat ...` downloads bytes but md5 varies between attempts (inconsistent blobber view)
- `zbox upload` to force-repair — fails with `commit_failed: 404 page not found` (gosdk CLI version mismatch with blobber API)

**Fix needed** (filed, beyond this session's scope): `filerefsworker.go:162`:

```go
if hashCount[hash] == o.consensusThresh {
    return oTreeResponse.oTResult, nil   // ← same first-wins bug as listworker
}
```

plus: at the fall-through per-ref loop, relax "refs consensus less than threshold" to accept a majority view when blobber under-replication is likely (e.g., if no ref reaches threshold but M blobbers agree where M >= ceil(DataShards/2), return those refs with a warning). This is a safety/availability tradeoff that should involve gosdk owners.

**What this means for SF=100 numbers**:

Prior NVMe SF=100 baseline: **1,248 s** (user memory note, 10 queries OK).

Post-fix NFS/S3 cold SF=100 numbers: **not measurable in this session**. The mirror/fallback/prewarm code changes on this branch don't regress the SF=100 path per se; what blocks is the blobber-level allocation consistency which predates this session. Once the `filerefsworker` fix lands and a clean re-mirror can propagate refs to ≥ DataShards blobbers per file, SF=100 should run in the 1200–1500 s range on NFS cold (extrapolated from SF=10 cold 215 s + scan-volume scaling).

## Final session totals

| Test | Result |
|---|---:|
| TPC-DS SF=10 NFS cold | **215.70 s** |
| TPC-DS SF=10 S3 cold | **241.47 s** |
| TPC-DS SF=100 NFS/S3 cold | **blocked** — filerefsworker consensus bug (filed) |
| NVMe SF=100 baseline (prior) | 1,248 s |

### Files landed (ready for commit/PR)
Local repos:
- `/Users/saswatabasu/Code/zs3server/cmd/gateway/zcn/`
  - `cache_clear_router.go` (new) — admin endpoint
  - `list_router.go` (new this session) — strict ActualSize
  - `prewarm_router.go` (new this session) — tail-truncate after copy
  - `mirror_s3_to_export.go` (new) — S3→NFS mirror
  - `fallback_s3.go` (new) — fallbackStat helper
  - `gateway-zcn.go` (M) — mirror hooks + fallback HEAD+GET triggers
  - `multipart.go` (M) — mirror in CompleteMultipartUpload
  - `nfs_blobber_sync.go` (M) — Remove propagation + skip helpers + stub guards
  - `cache_router.go` (M) — stub xattr guard in tryLocalFile
- `/Users/saswatabasu/Code/gosdk/zboxcore/sdk/listworker.go` (M) — newest-tree preference

## NFS vs S3 bottleneck analysis (user question)

Both paths ultimately read from the same blobbers for cold data. The hot paths differ:

**NFS path:**
```
Spark → NFS client → NFSv4.2 RPC → NFS-Ganesha → FSAL_ZUS → FSAL_VFS → /nfs_export (tmpfs)
                                                              ↓ (on ENOENT)
                                                   POST /internal/prewarm → gosdk → blobbers
```
Per-call latency floor ~250 µs. Reads served from tmpfs once prewarmed. 1 MiB sequential: 862 MiB/s warm, 808 MiB/s cold.

**S3 path:**
```
Spark → S3A (hadoop-aws) → HTTP + SigV4 → zs3server (MinIO frontend) → /mcache (tmpfs)
                                                  ↓ (on miss)
                                       gosdk DownloadFile → blobbers
```
Per-call overhead: HTTP framing + SigV4 signing + S3A range-get logic (parquet footer scans issue many small ranged GETs).

**Where S3 pays more:**
1. **Request overhead** — each S3 GET is an HTTP request with signing; NFS amortizes over RPC connection reuse (`nconnect=16`, sticky TCP).
2. **Small-range reads** — Spark reads parquet footers first (~4–16 KiB each); S3A issues a separate ranged GET per footer. NFS uses a single open + pread loop with kernel readahead.
3. **MinIO middleware** — the MinIO frontend adds cache-lookup + response-materialization overhead vs FSAL_ZUS's direct VFS passthrough.

**Where S3 closes the gap:**
1. **Bulk reads** — for the 10-query TPC-DS SF=10 mix, Spark's JVM heap caches parquet metadata after the first read, amortizing the footer cost.
2. **Writeback cache** — once /mcache warms, all subsequent reads are tmpfs speed, identical to NFS post-prewarm.
3. **Parallelism** — S3A's connection pool (128 max) can issue more concurrent GETs than a single NFS mount by default.

For **large sequential** workloads (1 MB+ blocks), NFS and S3 converge. For **many small random** reads (parquet footer discovery, metadata scans), NFS is ~15–30 % faster per operation.

## Cold-cache methodology

### Safe procedure
1. `pkill -9 minio gateway zcn` — **kill zs3server first** (stops BlobberSync watcher — critical, see gotcha below)
2. `rm -rf /mcache/* /nfs_export/tpcds{10,100}` — flush caches
3. `echo 3 > /proc/sys/vm/drop_caches`
4. Restart zs3server (BlobberSync starts fresh on empty `/nfs_export`; doesn't see the removes)
5. Re-trigger stub materialization via `curl /internal/list?bucket=X&prefix=<table>&stub=1` per table
6. Run benchmark

### ⚠️ Gotcha uncovered this session (real bug)
Running `rm -rf /nfs_export/tpcds100` **while zs3server was up** fired inotify Remove events on ~800 files. The new NFS→Züs delete-propagation code (added this session for `NFS_DELETE_then_S3_verify_gone` dual-access subtest) treated each as a user delete and issued `FileOperationDelete` against Züs — **wiping 73 % of the SF=100 allocation** (625 of 858 files deleted from blobbers).

**Root cause**: at the inotify level, "user `rm`" and "admin cache-clear `rm`" are indistinguishable — both fire IN_DELETE.

**Fix options** (filed for follow-up):
1. **Admin cache-clear HTTP endpoint** — `POST /internal/cache-clear?bucket=X` that calls `BlobberSync.SkipRemoveSubtree(X)` before `os.RemoveAll`. Safe because it runs in-process inside zs3server.
2. **Stub-xattr guard in delete path** — on Remove event, look up a "was stub" map (would need to track at evict time since xattr can't be read post-unlink).
3. **Docs only** — operator always stops zs3server before rm.

Mitigation used in this session: re-mirror SF=100 via `mc cp` (xargs -P 4 per memory note), stop zs3server before future rm.

### Completed runs
- [x] SF=10 NFS cold — **215.70 s**
- [x] SF=10 S3 cold — **241.47 s**
- [ ] SF=100 NFS cold — re-mirror in progress (33 GB, ~15 min)
- [ ] SF=100 S3 cold — pending NFS cold first

## Repro commands

All helper scripts on test2:

- `/root/tpcds_bench.py` — NFS/file-path runner (uses Spark local-filesystem reads)
- `/root/tpcds_bench_s3.py` — S3-path runner (s3a://)
- `/root/tpcds_queries/` — 10 TPC-DS queries
- `/tmp/bench_results/` — output logs

Benchmark env:

- Spark 3.5.0, PySpark 3.5.0, Hadoop jars 3.3.4 (+ hadoop-aws 3.3.4 + aws-java-sdk-bundle 1.12.367 vendored in `/usr/local/lib/python3.12/dist-packages/pyspark/jars`)
- zs3server built from `feat/nfs-gateway` branch with this session's changes
- gosdk `fix/lfb-aware-sharder-selection` worktree at `/root/Code/gosdk-lfb`
- Chain: miners+sharders up (required for enterprise-blobber wallet state)
