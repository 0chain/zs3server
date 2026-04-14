# Recommended NFS Mount Options for FSAL_ZUS

The FSAL_ZUS Ganesha plugin exports a tmpfs-backed cache (/nfs_export) and
transparently prewarms missing objects from Zus blobbers on lookup miss. The
following mount options are recommended for production clients.

## Recommended mount line

```
mount -t nfs4 -o vers=4.2,nconnect=16,rsize=1048576,wsize=1048576,\
  lookupcache=positive,actimeo=0,hard,timeo=600,retrans=2 \
  <server>:/ /mnt/zus_nfs
```

## Option rationale

### nconnect=16
Opens 16 parallel TCP streams from client to server. Matches the
BlobberSync parallelism (nfs_sync_workers=8 plus prewarm callbacks) so a
single mount can saturate the prewarm pipeline with concurrent cold reads.
Without nconnect, a single TCP connection serializes all RPCs.

### lookupcache=positive (or actimeo=0)
Disables the client-side NEGATIVE dentry cache. Critical for FSAL_ZUS
correctness: if the kernel caches a miss for an object that is later
prewarmed into /nfs_export, a second lookup would keep returning ENOENT
instead of letting Ganesha call our lookup hook. `actimeo=0` achieves the
same by expiring all attribute caches immediately; prefer
`lookupcache=positive` in production since `actimeo=0` also forces
re-GETATTR on every access (higher overhead).

### vers=4.2
Matches the Ganesha export protocol. NFSv4.2 enables server-side copy
(COPY op), sparse-file SEEK, and ALLOCATE/DEALLOCATE — useful for
parquet/ORC workloads. v3 would disable compound ops and NFSv4 state.

### hard,timeo=600,retrans=2
`hard` — client retries indefinitely on server RPC timeout instead of
returning EIO to userspace. Prewarming from blobbers can take seconds
(consensus + erasure decode + network); a soft mount would abort reads
mid-prewarm.
`timeo=600` (60s) + `retrans=2` — allow a single RPC up to ~180s (with
exponential backoff) before the client gives up and retries from scratch.
Covers blobber consensus delay without thrashing retries.

### rsize=1048576,wsize=1048576
1 MiB I/O size matches Ganesha's default chunk size and the tmpfs page
behavior. Smaller sizes fragment reads into more RPCs; larger than 1 MiB
is capped server-side anyway.

## Not recommended

- `noac` — too aggressive, disables all attribute caching including
  during active reads. Use `actimeo=0` or `lookupcache=positive` instead.
- `soft` — will surface EIO to apps during prewarm latency spikes.
- `proto=udp` — NFSv4 requires TCP; ignored anyway.
- `sync` — forces O_SYNC semantics; not needed, hurts throughput.
