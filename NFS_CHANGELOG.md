# NFS Gateway — Changes Summary

## Performance Results

NFS-Ganesha on tmpfs, NFSv4, nconnect=16. Same durability as S3 (eventual ACID via async blobber commit).

| Size | NFS PUT obj/s | NFS PUT MB/s | NFS GET obj/s | NFS GET MB/s |
|------|---------------|-------------|---------------|-------------|
| 1 KiB | 5,822 | 5.7 | 10,283 | 10.0 |
| 10 KiB | 6,420 | 62.7 | 9,899 | 96.7 |
| 100 KiB | 5,431 | 530 | 8,559 | 836 |
| 1 MiB | 1,482 | 1,482 | 4,846 | 4,846 |
| 10 MiB | 192 | 1,916 | 295 | 2,954 |
| 100 MiB | 12 | 1,227 | 14 | 1,423 |
| 1 GiB | 1.1 | 1,174 | 2.4 | 2,509 |

vs S3 warp (also on tmpfs):

| Size | NFS PUT | S3 warp PUT | NFS/S3 |
|------|---------|-------------|--------|
| 1 KiB | 5,822 | 2,516 | 2.3x faster |
| 1 MiB | 1,482 | 516 | 2.9x faster |
| 10 MiB | 192 | 88 | 2.2x faster |

## Journey (1KB PUT obj/s)

| Step | obj/s | Change |
|------|-------|--------|
| v1: go-nfs + direct blobber | 9 | Baseline |
| v2: go-nfs + HTTP S3 loopback | 32 | Writeback cache via minio-go |
| v3: go-nfs + in-process ObjectLayer | 48 | Eliminated HTTP overhead |
| v4: go-nfs + nconnect=16 | 61 | 16 parallel TCP connections |
| v5: go-nfs + concurrent dispatch + write cache | 60 | go-nfs is fundamentally limited |
| v6: NFS-Ganesha + FSAL_VFS + tmpfs | 5,822 | C NFS server, 647x improvement |

## Changes Made

### 1. NFS-Ganesha replacement (biggest impact: 60 → 5,822 obj/s)
- Replaced go-nfs (Go, NFSv3, 60 obj/s ceiling) with NFS-Ganesha (C, NFSv4, 5,822 obj/s)
- NFS-Ganesha uses FSAL_VFS pointing at tmpfs export directory
- Installed via `apt install nfs-ganesha nfs-ganesha-vfs`
- Config at `/etc/ganesha/ganesha.conf`
- Mount: `mount -t nfs4 -o nconnect=16 localhost:/ /mnt/zs3`

### 2. Blobber sync via inotify (ACID layer)
- `nfs_blobber_sync.go`: watches `/nfs_export` for file changes
- Non-blocking batch commits: collects files into batches of 25, calls `DoMultiOperation` directly
- Previous approach used `putFile()` which blocked on `batchUploadChan` (500ms per file)
- New approach: batch workers drain file channel, commit 25 files per WM lock acquisition
- Debounced inotify: 200ms quiet period before queueing commit
- 2s backoff on failure, 10K file channel buffer
- Expected blobber commit: ~250 files/s (5 workers x 25 files / 500ms)

### 3. Dynamic direct-to-blobber threshold
- `nfs_direct_threshold`: files >2MB bypass batch, commit to blobbers immediately
- `s3_direct_threshold`: same for S3 PUT path (bypass batchUploadChan)
- Small files batch for throughput, large files commit immediately to free cache

### 4. Cache management
- `nfs_cache_evict`: delete files from export dir after blobber commit (frees tmpfs)
- `nfs_spillover_dir`: NVMe directory for overflow when tmpfs >80% full
- Steady-state tmpfs usage with eviction: ~25MB for small-file workloads

### 5. Three NFS cache modes
- `tmpfs`: fastest (5,822 PUT obj/s), no fsync, data in RAM, lost on power failure
- `nvme`: crash-safe, data on NVMe, survives power failure (but slower on full RAID)
- `direct`: synchronous blobber commit, slowest (60 obj/s), zero data loss risk

### 6. go-nfs patches (superseded by Ganesha, still in code as fallback)
- Concurrent RPC dispatch: goroutine per RPC instead of sequential
- Thread-safe CachingHandler: RWMutex on reverseHandles map
- Write cache: buffer WRITE RPCs in memory, flush on COMMIT
- In-memory file buffers: bytes.Buffer for files <=1MB (no temp file I/O)
- In-process ObjectLayer API: direct CacheObjectLayer calls, no HTTP loopback
- Stat cache: skip S3 HEAD after writes

### 7. gosdk changes (perf/small-file-throughput branch)
- `LockedBlobbersCap`: configurable per-blobber WM lock channel capacity
- Parallel WM lock: fan out all blobbers simultaneously (was sequential lead-first)
- Impact: +63% PUT, +18-93% GET for S3 path

### 8. Config additions (zs3server.json)
```json
{
  "nfs_cache_mode": "tmpfs",
  "nfs_ganesha_export_dir": "/nfs_export",
  "nfs_sync_workers": 8,
  "nfs_direct_threshold": 2097152,
  "s3_direct_threshold": 0,
  "nfs_spillover_dir": "",
  "nfs_cache_evict": true
}
```

### 9. Tests
- `tests/dual_access_test.sh`: 10 test cases (S3<->NFS read/write/list/delete/overwrite)
- `system_test/.../5_dual_access_test.go`: Go integration test

### 10. Documentation
- `NFS_ARCHITECTURE.md`: full architecture, measured data, NFSv4.1 plan
- `NFS_CHANGELOG.md`: this file

## Why go-nfs was slow (root cause analysis)

Every NFS WRITE RPC in go-nfs triggered: `Stat → OpenFile → download → Write → Close(upload) → Stat`.
That's 2 stats + 1 download + 1 upload PER WRITE RPC. For a 1KB file (1 WRITE RPC), this meant
2 round-trips to the cache layer just for one file.

NFS-Ganesha does a single `pwrite()` syscall per WRITE RPC — no open/close cycle.

## Branches
- zs3server: `feat/nfs-gateway`
- gosdk: `perf/small-file-throughput`
- go-nfs fork: `/Users/saswatabasu/Code/go-nfs` (branch: concurrent-dispatch)
