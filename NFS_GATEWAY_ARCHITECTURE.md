# NFS Gateway Architecture — Züs S3 Files

## Overview

The NFS gateway adds NFSv3 filesystem access to the same blobber data served by the S3 API. Both protocols share the same allocation, WAL, writeback cache, and batch upload workers. This provides the same dual-access pattern as AWS S3 Files — any data written through S3 is visible via NFS, and vice versa.

```
                     ┌───────────────────────────────────────────────┐
                     │              ZS3 Server Process               │
                     │                                               │
  NFS Clients ─TCP─→ │  ┌────────────┐                               │
  (mount -t nfs)     │  │ NFS Server │──→ billy.Filesystem (ZcnFS)  │
  port 2049          │  │ (go-nfs)   │         │                     │
                     │  └────────────┘         │                     │
                     │                         ↓                     │
                     │              ┌─────────────────────┐          │
                     │              │ Shared Data Path    │          │
  S3 Clients ─HTTP─→ │  ┌────────┐  │ • putFile()         │          │
  (aws s3, mc)       │  │ MinIO  │──→ • getFileReader()   │          │
  port 9000          │  │ Gateway│  │ • getRegularRefs()  │          │
                     │  └────────┘  │ • WAL intent log    │          │
                     │              │ • Batch workers      │          │
                     │              └────────┬────────────┘          │
                     │                       │                       │
                     │              ┌────────▼────────────┐          │
                     │              │ /mcache (NVMe SSD)  │          │
                     │              │ MinIO writeback cache│          │
                     │              └────────┬────────────┘          │
                     │                       │                       │
                     │              ┌────────▼────────────┐          │
                     │              │ Züs Blobbers        │          │
                     │              │ (erasure-coded,     │          │
                     │              │  blockchain-verified)│          │
                     │              └─────────────────────┘          │
                     └───────────────────────────────────────────────┘
```

## S3 Files Feature Parity

| AWS S3 Files Feature | Züs NFS Gateway | Status |
|---------------------|-----------------|--------|
| NFS v4.1/4.2 mount | NFSv3 mount (go-nfs) | ✅ Phase 1 (v3), Phase 4 (v4) |
| Dual access (S3 + NFS) | Same blobber data via both protocols | ✅ Implemented |
| Hot data cache (sub-ms reads) | MinIO writeback cache on /mcache | ✅ Existing |
| Small file fast path (< 128KB) | WAL inline + group fdatasync | ✅ Existing |
| Large file streaming (> 1MB) | Direct blobber download | ✅ Existing |
| Write-back with async sync | Write to /mcache, async commit | ✅ Existing |
| POSIX permissions (uid/gid) | Stored as blobber custom metadata | 🔜 Phase 3 |
| NFS v4.2 file locking | Advisory locks (NFS clients only) | 🔜 Phase 3 |
| Read-after-write consistency | Cache write-through for metadata | ✅ Existing |
| VPC mount targets | Network listener on configurable port | ✅ Implemented |

## Operation Mapping

### NFS → Züs Data Path

| NFS Operation | Implementation | Züs Function |
|--------------|----------------|--------------|
| `open + read` | Download to local temp, serve from temp | `getFileReader()` → blobber/cache |
| `write + close` | Buffer to local temp, upload on close | `putFile()` → WAL + batch workers |
| `readdir` | List blobber refs | `getRegularRefs()` |
| `stat / getattr` | Get single ref metadata | `getSingleRegularRef()` |
| `mkdir` | Create directory on blobbers | `alloc.DoMultiOperation(createdir)` |
| `unlink / rmdir` | Delete from blobbers + WAL cleanup | `alloc.DeleteFile()` + `walWriter.Delete()` |
| `rename` | Move + rename via multi-operation | `alloc.DoMultiOperation(move, rename)` |
| `create + write` | New temp file, upload on close | `putFile()` |

### Read Path (Detail)

```
NFS READ request
  → ZcnFile.Read() / ReadAt()
  → reads from local temp file (already downloaded on Open)

NFS OPEN (first access):
  → getSingleRegularRef() to verify file exists
  → getFileReader() to download content
     ├─ MinIO writeback cache hit → /mcache sendfile (sub-ms)
     └─ Cache miss → blobber download via GoSDK (~9ms direct IP)
  → write to temp file in nfs_cache_dir
  → all subsequent reads served from temp (random access, seek)
```

### Write Path (Detail)

```
NFS WRITE request
  → ZcnFile.Write() → local temp file (NVMe fast)
  → marked dirty

NFS CLOSE (flush to blobbers):
  → ZcnFile.Close()
  → uploadFromFile():
     ├─ putFile(ctx, alloc, remotePath, reader, size)
     │   → MinIO writeback cache accepts PUT (~1ms)
     │   → batch worker commits to blobbers asynchronously
     └─ walWriter.RecordIntent() (metadata-only, fdatasync)
         → crash recovery: WAL replay + /mcache retry
  → temp file cleaned up
```

## Configuration

Add to `~/.zcn/zs3server.json`:

```json
{
  "enable_nfs": true,
  "nfs_port": 2049,
  "nfs_cache_dir": "/tmp/zs3-nfs-cache",

  "enable_wal": true,
  "max_batch_size": 25,
  "batch_wait_time": 10,
  "batch_workers": 5,
  "upload_workers": 8,
  "download_workers": 64
}
```

### Mount Commands

```bash
# Linux
sudo mount -t nfs -o vers=3,tcp,nolock <zs3-host>:/ /mnt/zs3

# macOS
sudo mount -t nfs -o nfsvers=3,tcp,nolock,-P <zs3-host>:/ /mnt/zs3

# fstab entry
<zs3-host>:/ /mnt/zs3 nfs vers=3,tcp,nolock 0 0
```

## File Structure

```
cmd/gateway/zcn/
├── nfs_server.go    # NFSv3 server startup (go-nfs + billy.Filesystem)
├── nfs_fs.go        # ZcnFS: billy.Filesystem backed by blobbers
├── nfs_file.go      # ZcnFile: billy.File with temp staging + blobber flush
├── gateway-zcn.go   # S3 gateway (starts NFS if enable_nfs=true)
├── wal.go           # WAL intent log (shared by S3 + NFS writes)
├── dStorage.go      # putFile/getFileReader (shared data path)
├── worker.go        # Batch upload workers (shared)
└── initSDK.go       # Config loading (enable_nfs, nfs_port, nfs_cache_dir)
```

## Implementation Phases

### Phase 1 — Basic NFS access (current)
- [x] NFSv3 server via go-nfs library
- [x] billy.Filesystem implementation (ZcnFS)
- [x] Read files (download → temp → serve)
- [x] Write files (buffer → temp → upload on close)
- [x] Directory listing, stat, mkdir, remove
- [x] WAL integration for write crash recovery
- [x] Config: enable_nfs, nfs_port, nfs_cache_dir

### Phase 2 — Read cache optimization
- [ ] Persistent read cache (skip re-download for recently read files)
- [ ] Cache eviction (LRU, max size limit)
- [ ] Large file streaming (bypass temp file for sequential reads > 1MB)
- [ ] Prefetch for sequential access patterns

### Phase 3 — POSIX semantics
- [ ] uid/gid/mode stored as blobber CustomMeta
- [ ] NFS advisory file locking (Lock/Unlock on ZcnFile)
- [ ] Symlink support (small metadata objects on blobbers)
- [ ] Extended attributes via blobber CustomMeta

### Phase 4 — Production hardening
- [ ] Upgrade to NFSv4.1 (for better locking, delegations)
- [ ] Cross-view sync notifications (S3 write → NFS cache invalidation)
- [ ] Multi-gateway HA (shared lock state via etcd/Redis)
- [ ] Metrics: open files, cache hit ratio, read/write latency
- [ ] Kubernetes DaemonSet deployment for on-prem

## Performance Expectations

Based on existing benchmarks (test2, 12 cores, 3 enterprise blobbers):

| Operation | Expected NFS Throughput | Notes |
|-----------|----------------------|-------|
| Small file read (cached) | 5,000-9,000 obj/s | MinIO sendfile from /mcache |
| Small file read (blobber) | 500-700 obj/s | Direct blobber download |
| Small file write | 1,500-3,000 obj/s | WAL + writeback cache |
| Large file read | 1,700+ MB/s | Streaming from blobbers |
| Directory listing | ~500 entries/call | getRegularRefs pageLimit |

NFS adds ~0.5ms overhead per operation (protocol parsing, temp file I/O) on top of the underlying data path latency.

## Comparison with AWS S3 Files

| Dimension | AWS S3 Files | Züs NFS Gateway |
|-----------|-------------|-----------------|
| Protocol | NFS v4.1/4.2 | NFS v3 (Phase 1), v4 (Phase 4) |
| Data durability | S3 (single provider) | Erasure-coded across independent blobbers |
| Vendor lock-in | AWS only | Any cloud or on-prem |
| Data integrity | AWS-managed | Blockchain-verified (0chain) |
| Pricing | $0.30/GB cache + access fees | Blobber staking + allocation cost |
| Encryption | SSE-S3 or SSE-KMS | End-to-end (GoSDK encrypt flag) |
| File locking | NFS v4.2 (S3 bypasses locks) | Advisory (same: S3 bypasses locks) |
| Sync latency | Minutes (FS→S3), seconds (S3→FS) | Same (writeback flush interval) |
