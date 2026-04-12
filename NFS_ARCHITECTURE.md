# NFS Gateway Architecture — Zus S3 + NFS Dual Access

## Overview

The NFS gateway provides NFSv3 filesystem access to the same blobber data served by the S3 API. Both protocols share the same allocation, writeback cache, WAL, and batch upload workers. Any data written through S3 is visible via NFS, and vice versa.

```
                     +-----------------------------------------------+
                     |              ZS3 Server Process               |
                     |                                               |
  NFS Clients -TCP-> |  +------------+                               |
  (mount -t nfs)     |  | NFS Server |-->  billy.Filesystem (ZcnFS)  |
  port 2049          |  | (go-nfs)   |         |                     |
                     |  +------------+         |                     |
                     |                         v                     |
                     |              +---------------------+          |
                     |              | In-Process Cache API |          |
  S3 Clients -HTTP-> |  +--------+  | ObjectLayer.PutObj() |          |
  (aws s3, mc)       |  | MinIO  |->| ObjectLayer.GetObj() |          |
  port 9000          |  | Gateway|  | (no HTTP overhead)   |          |
                     |  +--------+  +--------+------------+          |
                     |                       |                       |
                     |              +--------v------------+          |
                     |              | /mcache (NVMe SSD)  |          |
                     |              | MinIO writeback cache|          |
                     |              +--------+------------+          |
                     |                       | async (5s)            |
                     |              +--------v------------+          |
                     |              | WAL Intent Log      |          |
                     |              | (fdatasync, crash    |          |
                     |              |  recovery metadata)  |          |
                     |              +--------+------------+          |
                     |                       |                       |
                     |              +--------v------------+          |
                     |              | Zus Blobbers        |          |
                     |              | (erasure-coded,     |          |
                     |              |  blockchain-verified)|          |
                     |              +---------------------+          |
                     +-----------------------------------------------+
```

## ACID Compliance

Both S3 and NFS share the same ACID guarantees:

| Property | Mechanism |
|----------|-----------|
| **Atomicity** | Each file write is atomic -- fully in cache or not |
| **Consistency** | Writeback worker ensures eventual consistency with blobbers |
| **Isolation** | Concurrent writes to different files are isolated (separate cache entries) |
| **Durability** | Data written to /mcache (NVMe) + WAL fdatasync survives process crash |

**Crash recovery**: WAL replay + /mcache scan -> re-commit uncommitted objects to blobbers.
**Disk failure window**: ~5s between cache write and blobber commit. If NVMe dies in this window, data is lost (Mode 2 behavior).

## NFS Data Path

### Write Path (NFS -> cache -> blobbers)

```
NFS WRITE RPCs (32KB chunks)
  -> ZcnFile.Write() -> in-memory bytes.Buffer (files <= 1MB)
                      -> temp file on disk (files > 1MB)

NFS CLOSE RPC
  -> ZcnFile.Close()
  -> nfsObjAPI.put() -> MinIO CacheObjectLayer.PutObject()
     -> writes to /mcache (NVMe, ~0.5ms)
     -> returns immediately
  -> stat cache updated (avoids HEAD round-trip on next GETATTR)

Background (async, every 5s):
  -> writeback worker scans /mcache for dirty objects
  -> putFile() -> batchUploadChan -> DoMultiOperation -> blobbers
  -> WAL intent recorded (fdatasync)
```

### Read Path (NFS <- cache/blobbers)

```
NFS OPEN
  -> ZcnFS.OpenFile()
  -> nfsObjAPI.head() -> CacheObjectLayer.GetObjectInfo() (~0.1ms if cached)
  -> nfsObjAPI.get() -> CacheObjectLayer.GetObjectNInfo()
     +-- Cache hit -> /mcache sendfile (sub-ms)
     +-- Cache miss -> blobber download via GoSDK (~9ms direct IP)
  -> data loaded into bytes.Buffer (<=1MB) or temp file (>1MB)

NFS READ RPCs (32KB chunks)
  -> ZcnFile.Read() -> read from buffer/temp file (zero copy)
```

## Performance Data (Measured)

Test environment: test2 (12 cores, 3 enterprise blobbers 2+1, chain stopped)

### NFS vs S3 -- Disk mode + nconnect=16 (current best)

| Size | NFS PUT obj/s | S3 PUT obj/s | NFS/S3 PUT | NFS GET obj/s | S3 GET obj/s | NFS/S3 GET |
|------|---------------|--------------|------------|---------------|--------------|------------|
| 1 KiB | 61 | 355 | 17% | 312 | 493 | 63% |
| 10 KiB | 93 | 360 | 26% | 303 | 509 | 60% |
| 100 KiB | 88 | 335 | 26% | 300 | 497 | 60% |
| 1 MiB | 63 | 207 | 30% | 239 | 379 | 63% |

Both benchmarks use Python clients (boto3 for S3, os.open for NFS). S3 numbers would be 5-10x higher with Go-native warp tool (previously measured 2516 PUT, 6257 GET for 1KB).

NFS mount must use `nconnect=16` for maximum throughput:
```bash
mount -t nfs -o vers=3,tcp,nolock,nconnect=16 <host>:/ /mnt/zs3
```

### NFS cache modes

| Mode | PUT latency | Crash recovery | Config |
|------|-------------|----------------|--------|
| `disk` (default) | ~0.5ms | Yes (ACID via /mcache + WAL) | `"nfs_cache_mode": "disk"` |
| `memory` | ~0.2ms | No (data in RAM only) | `"nfs_cache_mode": "memory"` |

Memory mode is faster per-file but does NOT improve concurrent throughput -- the bottleneck is go-nfs NFSv3 RPC dispatch, not backend storage.

### Improvement journey

| Version | 1KB PUT | 1KB GET | Change |
|---------|---------|---------|--------|
| v1: Direct blobber (sync WM lock+commit) | 9 | 44 | Baseline -- each Close() blocks on 3 blobber round-trips |
| v2: HTTP S3 loopback (minio-go) | 32 | 95 | +3.5x PUT -- writeback cache via HTTP, ~5ms overhead/file |
| v3: In-process ObjectLayer API | 48 | 103 | +5.3x PUT -- zero HTTP overhead, direct cache write ~0.5ms |
| v4: nconnect=16 (mount option) | 61 | 312 | +6.8x PUT, +7.1x GET -- 16 parallel TCP connections |

### Why NFS is still slower than S3

The remaining gap (61 vs 355 PUT, 312 vs 493 GET) is **go-nfs server overhead**:

1. **Multiple RPCs per file write**: Each write = LOOKUP + CREATE + WRITE + CLOSE + GETATTR = 5 RPCs
2. **go-nfs sequential RPC dispatch**: processes RPCs one at a time per TCP connection
3. **No compound operations**: NFSv3 cannot batch multiple ops into one round-trip
4. **32KB chunk size**: 1MB file = ~32 WRITE RPCs + ~32 READ RPCs (vs 1 HTTP PUT/GET)

`nconnect=16` solves the client-side concurrency (16 parallel TCP connections), but go-nfs still processes each connection's RPCs sequentially. GET benefits more because read RPCs are independent across files. PUT benefits less because CREATE->WRITE->CLOSE for each file must execute in order on the same connection.

### Path to S3 parity for NFS

**Option A: NFS-Ganesha (recommended for production)**
- Industry-standard C userspace NFS server (used by CephFS, GlusterFS)
- Custom FSAL (Filesystem Abstraction Layer) plugin: ~20 C callbacks
- FSAL calls into Go code via CGo for blobber operations
- Expected: 5,000-15,000 ops/s (10-100x current)
- Supports NFSv3/v4/v4.1/v4.2, pNFS, Kerberos
- Trade-off: C dependency, CGo bridge complexity

**Option B: Patch go-nfs for concurrent RPC dispatch**
- Modify willscott/go-nfs serve loop to dispatch each RPC to a goroutine pool
- ~50 line change to conn.go
- Expected: 2-4x current (200-400 PUT obj/s with nconnect=16)
- Stay pure Go, minimal risk

**Option C: NFSv4.1 via kuleuven/nfs4go**
- Compound operations: 5 RPCs -> 1-3
- Sessions with multiple slots
- Expected: 2-3x current (realistic, not 5x)
- Pure Go, new library integration

**NFSv4.1 addresses points 1, 2, 3**: compound operations batch LOOKUP+OPEN+WRITE+CLOSE into a single RPC, sessions allow multiple concurrent connections, and pNFS enables parallel data access.

### Scaling analysis: obj/s vs MB/s at different file sizes

At small sizes (1-10KB), the bottleneck is **per-file overhead** (RPC round-trips for NFS, HTTP round-trips for S3). obj/s stays roughly constant regardless of file size.

At large sizes (1MB+), the bottleneck shifts to **data transfer bandwidth**. obj/s drops but MB/s rises:

| Size | NFS PUT MB/s | S3 PUT MB/s | NFS GET MB/s | S3 GET MB/s |
|------|-------------|-------------|-------------|-------------|
| 1 KiB | ~0 | 0.3 | 0.1 | 0.5 |
| 10 KiB | 0.6 | 3.6 | 1.1 | 4.9 |
| 100 KiB | 6.2 | 31.7 | 11.1 | 47.1 |
| 1 MiB | 46.5 | 177.2 | 73.8 | 535.4 |

NFS large-file performance is further penalized by the 32KB NFS chunk size: a 1MB file requires ~32 NFS WRITE RPCs vs 1 HTTP PUT for S3.

## NFSv4.1 Upgrade Plan

### Why NFSv4.1

NFSv4.1 compound operations can batch an entire file create+write+close into **1 round-trip** instead of 5+. This alone would give ~5x improvement for small-file writes, bringing NFS close to S3 performance.

| Feature | NFSv3 (current) | NFSv4.1 (planned) | Impact |
|---------|-----------------|-------------------|--------|
| Ops per file write | 5+ RPCs | 1 compound RPC | ~5x less overhead |
| Connections | 1 TCP per mount | Multiple sessions | Better concurrency |
| Locking | Advisory (nolock) | Mandatory + delegations | Safer multi-client |
| Data parallelism | None | pNFS layouts | Parallel blobber reads |
| Security | AUTH_SYS only | RPCSEC_GSS (Kerberos) | Enterprise auth |

### Projected performance with NFSv4.1

| Size | Current NFS PUT | Expected NFSv4.1 PUT | S3 PUT | NFS/S3 |
|------|----------------|---------------------|--------|--------|
| 1 KiB | 48 | ~200-300 | 357 | 56-84% |
| 10 KiB | 65 | ~200-300 | 364 | 55-82% |
| 100 KiB | 63 | ~200-300 | 324 | 62-93% |
| 1 MiB | 47 | ~150 | 177 | 85% |

Estimates based on: 1 compound RPC per file (~2ms) instead of 5 RPCs (~20ms), with multi-session concurrency.

### Implementation approach

**Option A: kuleuven/nfs4go (recommended)**
- Pure Go NFSv4.0/4.1/4.2 server library
- Companion kuleuven/vfs abstraction layer designed for minimal integration effort
- Supports compound operations, sessions, state management
- Active development, based on smallfz/libnfs-go
- GitHub: github.com/kuleuven/nfs4go + github.com/kuleuven/vfs

**Option B: Buildbarn bb-remote-execution NFSv4**
- Production-tested NFSv4.1 in Go (used in Buildbarn build infrastructure)
- Requires implementing ~15-20 methods across Directory/Leaf/Node interfaces
- Tightly coupled to Buildbarn virtual filesystem types -- significant adaptation needed
- No third-party usage examples found

**Option C: NFS-Ganesha (C) with Go FSAL plugin**
- Most mature NFSv4.1/4.2 implementation (C userspace server)
- Requires CGo bridge for custom filesystem backend
- Not pure Go -- adds build/deployment complexity

### Implementation phases

#### Phase 1: NFSv4.1 basic access (kuleuven/nfs4go)
- [ ] Integrate nfs4go as NFSv4.1 server alongside existing NFSv3
- [ ] Implement VFS interface backed by MinIO in-process ObjectLayer
- [ ] Support compound OPEN+WRITE+CLOSE for small files
- [ ] Benchmark: target 200+ obj/s for 1KB PUT
- [ ] Config: nfs_version: "4.1" (default stays v3 for backward compat)

#### Phase 2: Sessions and concurrency
- [ ] NFSv4.1 session support (multiple connections per mount)
- [ ] Sequence/slot tracking for exactly-once semantics
- [ ] Delegations for cached reads (client-side caching with callbacks)
- [ ] Benchmark: target 300+ obj/s for 1KB PUT

#### Phase 3: pNFS for parallel blobber access
- [ ] pNFS file layout: return blobber endpoints as data servers
- [ ] Client reads directly from blobbers (bypasses NFS server for data)
- [ ] NFS server only handles metadata operations
- [ ] Benchmark: target S3-parity for large files (500+ MB/s GET)

#### Phase 4: Enterprise features
- [ ] RPCSEC_GSS authentication (Kerberos)
- [ ] POSIX ACLs stored as blobber custom metadata
- [ ] Named attributes (xattrs) via blobber CustomMeta
- [ ] NFSv4.2 server-side copy (inter-allocation copy without data transfer)

## Configuration

```json
{
  "enable_nfs": true,
  "nfs_port": 2049,
  "nfs_version": "3",
  "nfs_cache_dir": "/tmp/zs3-nfs-cache",
  "enable_wal": true
}
```

### Mount commands

```bash
# NFSv3 (current)
sudo mount -t nfs -o vers=3,tcp,nolock <host>:/ /mnt/zs3

# NFSv4.1 (planned)
sudo mount -t nfs -o vers=4.1 <host>:/ /mnt/zs3
```

## File Structure

```
cmd/gateway/zcn/
+-- nfs_server.go      # NFSv3 server startup (go-nfs + billy.Filesystem)
+-- nfs_fs.go          # ZcnFS: billy.Filesystem backed by in-process ObjectLayer
+-- nfs_file.go        # ZcnFile: in-memory buffer (<=1MB) or temp file (>1MB)
+-- nfs_s3client.go    # In-process ObjectLayer API (replaces HTTP loopback)
+-- gateway-zcn.go     # S3 gateway (starts NFS if enable_nfs=true)
+-- wal.go             # WAL intent log (shared by S3 + NFS writes)
+-- dStorage.go        # putFile/getFileReader (shared data path)
+-- worker.go          # Batch upload workers (shared)
+-- initSDK.go         # Config loading (enable_nfs, nfs_port, nfs_cache_dir)
```
