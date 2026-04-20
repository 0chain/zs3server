# LogCache: Log-Structured ACID Cache for zs3server

## Problem

Enterprise blobber small-file throughput is limited by:
1. **Per-object overhead**: 6 sequential HTTP round-trips per PUT (connection → upload → lock → checkAlloc → commit → unlock)
2. **MinIO S3 handler overhead**: ~0.25ms per request through middleware pipeline
3. **batch_wait_time**: default 500ms sleep before firing partial batches (was 94% of PUT latency)
4. **nginx TLS proxy**: adds ~20ms per hop when blobbers are accessed via reverse proxy

Historical baseline: PUT 0.58 obj/s, GET 125 obj/s (1KiB, 4 concurrent)

## Solution: LogCache

A log-structured ACID cache that sits between the S3 API and the blobber commit layer.

### Data flow

```
S3 PUT (≤1MB) → Read data into memory
               → Append [header|data] to cache file (append-only)
               → Group fdatasync (one sync per batch of entries)
               → Update in-memory index
               → Return 200 to S3 client
               → Background: drain to blobbers via DoMultiOperation

S3 PUT (>1MB) → Direct to blobbers via batch upload (cache adds no benefit)

S3 GET → Check in-memory index
        → HIT: pread() from cache file at indexed offset → return data
        → MISS: fetch from blobbers via gosdk → return data

S3 HEAD → Check in-memory index → return metadata or fall through to blobbers
S3 LIST → Scan index for prefix matches + merge with blobber listing  
S3 DELETE → Mark deleted in index + send delete to blobbers
S3 COPY → Read source from cache/blobber → PUT dest through same path
```

### Cache file format

```
┌─────────────────────────────────────────────────────────────┐
│ Single append-only file on NVMe SSD                         │
├───────────────┬───────────────┬───────────────┬─────────────┤
│   Entry 1     │   Entry 2     │   Entry 3     │    ...      │
│ [hdrLen:4]    │ [hdrLen:4]    │ [hdrLen:4]    │             │
│ [header:JSON] │ [header:JSON] │ [header:JSON] │             │
│ [dataLen:4]   │ [dataLen:4]   │ [dataLen:4]   │             │
│ [data:bytes]  │ [data:bytes]  │ [data:bytes]  │             │
└───────────────┴───────────────┴───────────────┴─────────────┘

Header JSON: {"b":"bucket","k":"key","z":1024,"m":"application/octet-stream","t":1234567890,"s":0}
  b = bucket name
  k = object key
  z = data size
  m = MIME type
  t = timestamp (nanoseconds)
  s = status (0=pending, 1=committed, 2=deleted)
```

### In-memory index

```go
map[string]*lcIndexEntry  // key = "bucket/key"

type lcIndexEntry struct {
    Bucket, Key, MimeType string
    Size       int64
    Timestamp  int64
    Status     lcEntryStatus  // pending, committed, deleted
    DataOffset int64          // byte offset in cache file
    DataLen    int64          // data length in cache file
}
```

### Group commit (amortized fdatasync)

Multiple concurrent PUTs are batched into a single fdatasync call:

```
goroutine 1: PUT → writeCh ──┐
goroutine 2: PUT → writeCh ──┤
goroutine 3: PUT → writeCh ──┼──→ groupCommitWriter: write all + 1 fdatasync → signal all done
goroutine 4: PUT → writeCh ──┤
goroutine N: PUT → writeCh ──┘
```

- **Per-entry cost at conc=256**: 5ms fdatasync / 256 entries = **0.02ms per entry**
- **Volume limit**: max 4MB data per fdatasync batch (prevents one large file from delaying small ones)
- **Uses fdatasync** (not fsync): skips inode metadata update, ~30% faster

### Background blobber commit

```
pending channel → commitWorker(s) → batch 25 entries → DoMultiOperation → mark committed
```

- 5 commit workers (configurable)
- Each batch: 25 entries → 6 RPCs × 3 blobbers ≈ 30ms per batch
- Drain rate: 5 × 25 / 0.030 = ~4,200 obj/s
- Committed entries STAY in cache index (continue serving GETs)

### ACID guarantees

| Property | How |
|----------|-----|
| **Atomicity** | Entry is either fully written to cache file (with fdatasync) or not at all |
| **Consistency** | In-memory index always matches cache file state |
| **Isolation** | Per-entry updates are atomic; concurrent readers see consistent state via RWMutex |
| **Durability** | fdatasync ensures data survives power loss; crash recovery replays cache file |

### Crash recovery

On startup:
1. Open cache file
2. Read entries sequentially from offset 0
3. For each entry: rebuild in-memory index (offset, length, status)
4. Queue pending entries for re-commit to blobbers
5. Set write offset to end of file (ready for appends)

### Adaptive routing by file size

| File size | Route | Rationale |
|-----------|-------|-----------|
| ≤64 KB | LogCache inline | Data fits in one fdatasync batch. Max benefit. |
| 64 KB - 1 MB | LogCache | Worth caching. Background commit handles blobber upload. |
| > 1 MB | Direct to blobbers | Cache file grows too fast. NVMe write ≈ blobber write. |

### Performance targets

#### obj/s (1KiB, 10 dedicated cores, 3 enterprise blobbers):

| Operation | Current (no cache) | LogCache target | Bottleneck |
|-----------|-------------------|-----------------|------------|
| PUT conc=1 | 29 | 100+ | fdatasync latency (5ms) |
| PUT conc=64 | 280 | 3,000+ | group commit amortization |
| PUT conc=256 | — | 5,000+ | CPU (Go HTTP handler) |
| GET conc=1 (cache hit) | 67 | 500+ | pread latency |
| GET conc=64 (cache hit) | 413 | 6,000+ | CPU + pread |
| GET conc=256 (cache hit) | — | 10,000+ | CPU limit (~15K on this HW) |

#### MB/s (large files):

| File size | PUT MB/s target | GET MB/s target | Limit |
|-----------|----------------|-----------------|-------|
| 1 KB | 5 | 10 | obj/s limited |
| 10 KB | 50 | 100 | obj/s limited |
| 100 KB | 500 | 1,000 | NVMe approaching |
| 1 MB (direct) | 200 | 3,000 | NVMe write / read |
| 10 MB (direct) | 500 | 3,500 | NVMe bandwidth |

#### Sustained performance:

- Sustained PUT ≤ drain rate (~4,200 obj/s for small files)
- Burst: cache absorbs spike, background catches up
- Cache growth at 5,000 obj/s × 10KB = 50 MB/s → 180 GB/hour
- **Cache sizing**: 10-100 GB recommended for typical workloads

### Cache compaction

When committed entries accumulate (wasting space), compact by:
1. Create new cache file
2. Copy only pending + recently-committed entries
3. Rebuild index with new offsets
4. Swap files atomically (rename)
5. Delete old file

Trigger: when >50% of file is committed/deleted entries.

### Configuration (zs3server.json)

```json
{
  "download_workers": 64,         // gosdk: SetDownloadWorkerCount()
  "upload_workers": 8,            // gosdk: SetHighModeWorkers()
  "sdk_batch_size": 32,           // gosdk: sdk.BatchSize
  "locked_blobbers_cap": 5,       // gosdk: sdk.LockedBlobbersCap
  "max_batch_size": 25,           // zs3server: ops per DoMultiOperation
  "batch_wait_time": 10,          // zs3server: ms before firing partial batch
  "batch_workers": 5,             // zs3server: concurrent DoMultiOperation workers
  "enable_wal": true,             // enable LogCache
  "wal_dir": "/data/logcache",    // cache file directory (should be on fast NVMe)
  "wal_commit_workers": 5         // background commit workers
}
```

### gosdk changes required

**1 line** (mandatory):
```go
// writemarker_mutex.go
var LockedBlobbersCap = 1  // set via zs3server.json locked_blobbers_cap
```

**Optional** (parallel WM lock, saves ~2-5ms per PUT):
```go
// writemarker_mutex.go: Lock()
// Replace sequential lead-blobber lock with parallel fan-out
```

**Optional** (fileCache size, helps non-cached GET):
```go
// fileref/fileref.go
var fileCache, _ = lru.New[string, FileRef](50000) // was 100
```

### Files

| File | Purpose |
|------|---------|
| `cmd/gateway/zcn/logcache.go` | Log-structured ACID cache implementation |
| `cmd/gateway/zcn/wal.go` | Lightweight WAL intent log (alternative to LogCache for writeback cache mode) |
| `cmd/gateway/zcn/initSDK.go` | Config struct with LogCache fields |
| `cmd/gateway/zcn/gateway-zcn.go` | S3 handler integration (GET/HEAD/DELETE/LIST/COPY) |
| `cmd/gateway/zcn/dStorage.go` | PUT handler with size-adaptive routing |

### Branches

- gosdk: `perf/small-file-throughput` (1b822c35)
- zs3server: `perf/logcache-acid` (1be6f34da)

### Measured results (test2, 2026-04-10/11)

| Configuration | PUT 1KiB (conc=64) | GET 1KiB (conc=64) | ACID |
|--------------|-------------------|--------------------|------|
| Historical baseline | 0.58 obj/s | 125 obj/s | Yes |
| + gosdk patches + config tuning | 280 obj/s | 4535 obj/s* | Yes |
| + WAL inline (no cache) | 1889 obj/s | 3900 obj/s | Yes |
| + WAL inline (conc=256) | 3672 obj/s | — | Yes |
| Writeback cache (not ACID) | 2498 obj/s | 8734 obj/s | **No** |
| **LogCache target** | **5,000 obj/s** | **10,000 obj/s** | **Yes** |

*writethrough cache for GET
