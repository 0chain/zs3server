# ZS3Server Deployment Guide — S3+NFS Accelerator Cache for Züs Blobbers

A copy-paste guide for a devops engineer to mount **zs3server** as an S3+NFS gateway in front of a Züs allocation, with optional upstream S3 fallback. Positions zs3server as a drop-in alternative to AWS S3 Files / EFS.

---

## 1. Architecture

zs3server is a **stateless regional gateway** that runs on one VM per allocation. It exposes both S3 and NFS on a single node, keeps a hot tmpfs cache of the working set, and streams the rest from Züs blobbers on demand. Optionally it can fall through to an upstream S3 endpoint for objects not yet in Züs, and automatically **cache back** those objects into the blobbers so subsequent reads are served from Züs.

```
┌─────────────────────────┐      ┌──────────────────────────┐      ┌─────────────────────┐
│ Client node(s)          │      │ zs3server GATEWAY node   │      │ Blobber nodes (N≥5) │
│ anywhere on the internet│      │ one per region/allocation│      │ eblobber × 5/7/...  │
│                         │      │                          │      │ running on their    │
│ - Linux, Mac, Windows   │ ───▶ │ - NFS-Ganesha (:2049)    │ ───▶ │ own instances       │
│ - S3 client (boto3, mc) │      │ - S3 gateway (:9100)     │      │ - WriteMarker       │
│ - NFS client (mount)    │      │ - Console (:9101)        │      │   consensus         │
│                         │      │ - tmpfs cache (/nfs_     │      │ - Erasure coded     │
│                         │      │   export, e.g. 32 GB)    │      │   3+2 or 4+1        │
│                         │      │ - WAL (wal.go)           │      │                     │
│                         │      │ - FSAL_ZUS plugin        │      │                     │
│                         │      │ - Router / fallback_s3   │      │                     │
└─────────────────────────┘      └────────────┬─────────────┘      └─────────────────────┘
                                              │ optional fallback on miss
                                              ▼
                                 ┌──────────────────────────┐
                                 │ Upstream S3 (AWS, MinIO) │
                                 │ used as read-through for │
                                 │ objects not yet in Züs   │
                                 └──────────────────────────┘
```

**Ports:** S3 API `9100`, MinIO Console `9101`, NFS `2049` (TCP).

---

## 2. Prerequisites

### 2a. Client node
Any Linux / macOS / Windows host. Install one of:
- **NFS v4 client** — Linux kernel built-in (`mount -t nfs4`), macOS built-in, Windows 10+ has `NFS-Client` feature.
- **S3 client** — `awscli`, `boto3`, `mc`, or `rclone`.

### 2b. Gateway node (zs3server)
- Linux x86_64, kernel ≥ 5.4 (for `nconnect=16` NFS support).
- Docker or a native binary build.
- **16 GB RAM minimum** (8 GB tmpfs cache + MinIO overhead).
- **NVMe SSD** recommended for the spillover tier and WAL (`/mcache`).
- `nfs-ganesha`, `nfs-ganesha-vfs` packages (for the NFS front-end).
- Install steps: see `CLAUDE.md` (build) and `BENCHMARK_GUIDE.md` (runtime dependencies and tmpfs setup).

### 2c. Blobber nodes
5 or more already-provisioned Züs enterprise blobbers. Setup: the `eblobber` repo. Blobbers must be registered to the same 0dns / miner network the gateway connects to.

### 2d. Upstream S3 (optional)
Any S3-compatible endpoint: AWS S3, MinIO, Ceph RGW, Backblaze B2, Wasabi. You'll need access/secret keys and (optionally) a per-bucket map from zs3server-bucket → upstream-bucket.

---

## 3. Step-by-step setup

### 3a. Create a Züs allocation (from any machine with `zbox`)

```bash
zbox newallocation \
  --size 1099511627776 \                 # 1 TiB
  --data 3 --parity 2 \
  --read_price 0.1-1 --write_price 0.1-1 \
  --lock 10

# prints the allocation ID — save it
export ALLOCATION_ID=<64-hex-chars>
```

### 3b. Configure the gateway node

```bash
ssh root@GATEWAY_IP
mkdir -p /root/.zcn-zs3 /nfs_export /mcache /var/log/ganesha /var/run/ganesha
mount -t tmpfs -o size=32G tmpfs /nfs_export
```

Drop in the Züs network config:

```bash
cat > /root/.zcn-zs3/config.yaml <<'EOF'
block_worker: https://mainnet.zus.network/dns
signature_scheme: bls0chain
min_submit: 50
min_confirmation: 50
confirmation_chain_length: 3
max_txn_query: 5
query_sleep_time: 5
sharder_consensous: 3
EOF

# copy your existing wallet and set the allocation
cp ~/.zcn/wallet.json /root/.zcn-zs3/wallet.json
echo -n "$ALLOCATION_ID" > /root/.zcn-zs3/allocation.txt
```

Server tuning file — fields map to `serverOptions` in `cmd/gateway/zcn/initSDK.go`:

```bash
cat > /root/.zcn-zs3/zs3server.json <<'EOF'
{
  "max_batch_size": 200,
  "batch_wait_time": 50,
  "batch_workers": 20,
  "upload_workers": 20,
  "download_workers": 20,
  "max_concurrent_requests": 2000,
  "locked_blobbers_cap": 0,

  "enable_wal": true,
  "wal_dir": "/nfs_export/.wal",
  "wal_commit_workers": 8,

  "nfs_ganesha_export_dir": "/nfs_export",
  "nfs_tmpfs_cache_enabled": true,
  "nfs_spillover_cache_enabled": false,
  "nfs_spillover_max_bytes": 0,
  "nfs_sync_enabled": true,
  "nfs_sync_workers": 8,
  "nfs_direct_threshold": 2097152,
  "nfs_cache_evict": true,
  "nfs_cacheback_enabled": true,

  "fallback_s3_enabled": false,
  "fallback_s3_write_through": "",
  "fallback_s3_write_max_bytes": 0
}
EOF
```

**Recommended default: ONE tmpfs cache, no spillover.** `/nfs_export` is the single cache tier for both S3 and NFS paths; WAL lives inside it at `.wal`. Do NOT set `MINIO_CACHE_DRIVES` — that opt-in layer creates a separate `/mcache` tmpfs with MinIO's hash-sharded format, which doubles memory overhead without helping the S3 GET path (the gateway's own `TryCacheRead` already checks `/nfs_export` first). See §10 for when to deviate.

### 3c. Start the gateway

```bash
export MINIO_ROOT_USER=zusadmin
export MINIO_ROOT_PASSWORD=$(openssl rand -hex 24)

nohup minio gateway zcn \
  --configDir /root/.zcn-zs3 \
  --address :9100 \
  --console-address :9101 \
  zcn://$ALLOCATION_ID \
  > /var/log/zs3server.log 2>&1 &
disown
```

Verify: `curl -s http://127.0.0.1:9100/minio/health/ready` should return HTTP 200.

### 3d. Start NFS-Ganesha with FSAL_ZUS

Config (`/etc/ganesha/ganesha.conf`) — FSAL_ZUS stacked over FSAL_VFS, tmpfs backend at `/nfs_export`:

```
NFS_CORE_PARAM { NFS_Protocols = 3, 4; }
NFSV4 { Grace_Period = 0; }

EXPORT {
    Export_Id   = 1;
    Path        = /nfs_export;
    Pseudo      = /;
    Access_Type = RW;
    Squash      = No_Root_Squash;
    SecType     = sys;
    Delegations = None;
    FSAL {
        Name = ZUS;
        FSAL {
            Name = VFS;
        }
    }
}
LOG { Default_Log_Level = EVENT; }
```

Launch:

```bash
nohup ganesha.nfsd -L /var/log/ganesha/ganesha.log \
  -f /etc/ganesha/ganesha.conf -N NIV_EVENT </dev/null >/dev/null 2>&1 & disown
echo 2 > /proc/sys/sunrpc/tcp_slot_table_entries   # tune sunrpc
```

### 3e. Connect a client

**S3 (boto3 / aws-cli / mc):**

```bash
# aws-cli
aws configure set aws_access_key_id     zusadmin
aws configure set aws_secret_access_key <your-password>
aws --endpoint-url http://GATEWAY_IP:9100 s3 mb s3://bucket
aws --endpoint-url http://GATEWAY_IP:9100 s3 cp foo.parquet s3://bucket/

# mc
mc alias set zus http://GATEWAY_IP:9100 zusadmin <password> --api S3v4
mc cp foo.parquet zus/bucket/

# boto3
python3 - <<'PY'
import boto3
s3 = boto3.client("s3",
    endpoint_url="http://GATEWAY_IP:9100",
    aws_access_key_id="zusadmin",
    aws_secret_access_key="<password>")
s3.upload_file("foo.parquet", "bucket", "foo.parquet")
PY
```

**NFS v4.2 (recommended mount line):**

```bash
apt-get install -y nfs-common
mount -t nfs4 -o vers=4.2,nconnect=16,rsize=1048576,wsize=1048576,\
lookupcache=positive,actimeo=0,hard,timeo=600,retrans=2 \
  GATEWAY_IP:/ /mnt/zus

cp -r data/ /mnt/zus/bucket/
```

### 3f. Upstream S3 fallback (optional)

Edit `/root/.zcn-zs3/zs3server.json`:

```json
"fallback_s3_enabled":     true,
"fallback_s3_endpoint":    "https://s3.us-east-1.amazonaws.com",
"fallback_s3_region":      "us-east-1",
"fallback_s3_access_key":  "AKIA...",
"fallback_s3_secret_key":  "wJ...",
"fallback_s3_use_ssl":     true,
"fallback_bucket_map":     { "bucket": "legacy-aws-bucket" },

"fallback_s3_write_through":  "async",
"fallback_s3_write_max_bytes": 0
```

Restart the gateway. Three things turn on:

1. **Read-through** — a `GetObject` that's not in Züs (404 or consensus-fail) falls through to upstream S3, the reply is teed into an async cache-back upload to Züs, and subsequent reads serve from Züs. See `tryFallbackFetch` at `cmd/gateway/zcn/fallback_s3.go:76`.
2. **Write-through (S3)** — every successful S3 PUT to zs3server is replicated to the upstream bucket after the Züs commit. In `"async"` mode the replication is fire-and-forget with retry+backoff (up to 4 attempts, exponential); in `"mirror"` mode the PUT reply waits for both Züs and upstream to succeed. See `fallback_s3_write.go:syncPutStreamToUpstream`.
3. **Write-through (NFS)** — the same replication is invoked from the NFS commit path in `nfs_blobber_sync.go:commitBatch`, so files written through the NFS mount also land on the upstream. Same for DELETE via `commitDeleteBatch`.

The WAL covers writes ≤ 1 MiB even when upstream replication is in flight — crash before replication is fine, the next recovery pass re-commits and re-replicates. Size-gate via `fallback_s3_write_max_bytes` (0 = replicate all; set to e.g. 100 MiB to skip huge objects).

**Observability**: `curl http://GATEWAY_IP:9100/internal/cache_stats` shows write-through `{attempts, successes, failures, retries, bytes_replicated, last_failure}`.

---

## 4. Data flow diagrams

### 4a. Hot read — cache hit (sub-millisecond)

```
client ─► NFS/S3 :2049/:9100 ─► TryCacheRead(/nfs_export) ─► tmpfs read ─► reply
```
Code: `cmd/gateway/zcn/cache_router.go:TryCacheRead`.

### 4b. Cold read — cache miss, object is in Züs

```
client ─► NFS open2 / S3 GET ─► TryCacheRead miss ─► /internal/prewarm
  ─► gosdk getFileReader ─► blobber stream ─► tee into /nfs_export ─► reply
```
Code: `cmd/gateway/zcn/prewarm_router.go:prewarmHandler`.

### 4c. Cold read — cache miss, object NOT in Züs (fallback_s3)

```
client ─► S3 GET ─► TryCacheRead miss ─► Züs 404/consensus-fail
  ─► tryFallbackFetch(upstream S3) ─► io.TeeReader
                                    ├─► reply to client
                                    └─► async Zus Upload(cache-back)
```
Code: `cmd/gateway/zcn/fallback_s3.go:76` (fetch) and `fallback_s3.go:106` (tee). Dedup via singleflight so concurrent GETs for the same missing key share one upstream fetch.

---

## 5. Durability and consistency

- **Writes ≤ 1 MiB:** intent is appended to a group-commit WAL on `/mcache` and fsync'd before the client gets a reply. Durable across gateway crash. Source: `wal.go:ShouldUseWAL`.
- **Writes > 1 MiB:** buffered in the MinIO writeback cache, committed to blobbers asynchronously by BlobberSync. If the gateway crashes between client ACK and blobber commit, the write can be lost. Use WAL-eligible small-file paths where per-object durability matters, or front with a durable queue.
- **Reads:** strong read-your-writes for a single gateway instance (cache is authoritative until eviction). Eventual consistency if you write through one gateway and read through a different gateway — see §7.
- **Linearizability:** close-to-open semantics (same as AWS EFS / S3 Files). Porcupine linearizability harness in `tests/` passes both fs and s3 backends at 8 workers / 4000 ops.

---

## 6. Pricing model

- **Blobber storage** — you pay the blobber operators a GB-month rent in ZCN tokens. Inspect with `zbox getreadpoolinfo` / `zbox getwritepoolinfo`.
- **Gateway compute** — your own VM, your own NVMe. No per-GB cache surcharge (unlike AWS S3 Files where the EFS tier is metered).
- **Upstream S3** (only if fallback is enabled) — provider's pay-as-you-go rate for GET + egress on misses.

---

## 7. Multi-client / multi-region

- **Today:** one gateway per allocation is the supported pattern. Multiple clients through the same gateway are fine. Running multiple gateway instances against the same allocation works for reads but writes are not cache-coherent across gateways.
- **Near-future:** multi-gateway coordination using WriteMarker consensus plus blobber-as-source-of-truth (cache invalidation via blobber write-epoch).

---

## 7b. Deployment topologies

Three ways to run this, any of which works:

### A. Apps on the same host as zs3server (co-located)

Simplest. App container + zs3server on the same VM; app points at `http://127.0.0.1:9100` (S3) or mounts `localhost:/` (NFS). No network hop.

```bash
# as a systemd service + a Spark/PyTorch/etc. app side by side
mc alias set zus http://127.0.0.1:9100 zusadmin <password>
mc cp ./dataset zus/bucket/ --recursive
python train.py   # reads from /mnt/zus or via boto3 against 127.0.0.1:9100
```

Best for: single-GPU ML training, TPC-DS Spark, ad-hoc benchmarking.

### B. Apps on a separate compute node (shared gateway)

App node(s) mount the zs3server over the network. One gateway per region/allocation; many clients.

```bash
# On the app node:
mount -t nfs4 -o vers=4.2,nconnect=16,rsize=1048576,wsize=1048576 \
  GATEWAY_IP:/ /mnt/zus
# or: point your S3 client at http://GATEWAY_IP:9100
```

Best for: multi-client S3/NFS access, GPU training clusters where storage is centralised, dev/staging with one team-shared gateway.

### C. Containerised app + sidecar gateway

Everything in Docker / Kubernetes. Gateway runs as a sidecar or a `DaemonSet`; apps mount `/mnt/zus` from the gateway container's hostPath.

```yaml
# Pod spec fragment (k8s)
spec:
  containers:
  - name: zs3server
    image: 0chain/zs3server:latest
    args: ["gateway", "zcn", "--configDir", "/cfg", "--address", ":9100",
           "zcn://$(ALLOCATION_ID)"]
    env:
    - name: MINIO_ROOT_USER
      valueFrom: { secretKeyRef: { name: zs3, key: access } }
    - name: MINIO_ROOT_PASSWORD
      valueFrom: { secretKeyRef: { name: zs3, key: secret } }
    volumeMounts:
    - name: nfs-export
      mountPath: /nfs_export
    - name: cfg
      mountPath: /cfg
    ports:
    - containerPort: 9100
    - containerPort: 2049  # only if NFS is exposed
  - name: app
    image: my-app:latest
    env:
    - name: AWS_ENDPOINT_URL
      value: "http://localhost:9100"
    - name: AWS_ACCESS_KEY_ID
      valueFrom: { secretKeyRef: { name: zs3, key: access } }
    - name: AWS_SECRET_ACCESS_KEY
      valueFrom: { secretKeyRef: { name: zs3, key: secret } }
  volumes:
  - name: nfs-export
    emptyDir: { medium: Memory, sizeLimit: 32Gi }   # tmpfs via emptyDir
  - name: cfg
    secret: { secretName: zs3-config }
```

For Ganesha-in-container you need `CAP_SYS_ADMIN` (to mount tmpfs) and a running `rpcbind`. Production-ready images live under `0chain/zs3server:latest`.

Best for: CI pipelines, short-lived training jobs, serverless-style inference that wants a POSIX cache in front of Züs.

## 8. Limits and gotchas

- **Firewall:** open TCP `2049` (NFS) and `9100` (S3) to clients. Keep `9101` (Console) admin-only.
- **Auth:** NFS export is `SecType=sys` — fine for private networks. For hostile networks, put zs3server behind a VPN/WireGuard or configure Kerberos (`SecType=krb5p`).
- **tmpfs sizing:** `/nfs_export` should be ≥ your hot working set. Enable `nfs_spillover_cache_enabled=true` with `nfs_spillover_dir=/mcache/spill` when the hot set is larger than RAM.
- **WAL:** `enable_wal: true` is the default.
- **Chain-down operation:** zs3server needs sharders reachable on startup to fetch allocation state. If the chain is stopped, pre-seed the allocation cache before stopping (see `rclone_zus_chain_down.md` playbook).
- **FSAL_ZUS direct-write fix (2026-04-19):** one-line fix in `fsal-zus/file.c` so plain tmpfs files with no ZUS xattrs are served instead of returning EIO. Rebuild `libfsalzus.so` and restart Ganesha.
- **One tmpfs vs two:** by default we run one tmpfs at `/nfs_export` and the WAL inside it. If you want MinIO's built-in CacheObjectLayer (GET-side disk cache, separate from ours), set `MINIO_CACHE_DRIVES=/mcache` and mount a second tmpfs at `/mcache`. This only helps for S3-heavy workloads where our `TryCacheRead` is missing and the miss cost is network-bound; on most deployments it doubles RAM usage for minimal gain.
- **Write-through latency:** in `mirror` mode the PUT latency is `max(Züs_commit, upstream_put)` — plan for ~100 ms + upstream RTT. In `async` mode the client sees Züs latency only; upstream replication runs after the reply. Pick `mirror` only when you need both copies durable before acknowledging the client.

---

## 9. Regression / self-test

Self-test harnesses live in `tests/`:

- `run_regression_suite.sh` — full regression runner (env-driven; skips any target that's unset)
- `s3_mlperf_harness.py` — PUT+GET sweep over the 5 MLPerf workloads + Llama3 checkpoints
- `porcupine_harness.go` — linearizability harness with `-backend fs|s3`
- `dual_access_test.sh` — write via one protocol, read via the other, assert byte equality
- `imagenet_bench.sh` — ResNet/ImageNet-style small-file read throughput
- `cleanup_bench.sh` — drop scratch buckets cleanly after a benchmark run

Against your deployment:

```bash
export ZS3_ENDPOINT=http://GATEWAY_IP:9100
export ZS3_ACCESS_KEY=zusadmin
export ZS3_SECRET_KEY=<password>
export UPSTREAM_S3_ENDPOINT=http://upstream-minio:9200
export UPSTREAM_S3_ACCESS_KEY=upstream
export UPSTREAM_S3_SECRET_KEY=upstream123
export NFS_MOUNT=/mnt/zus
bash tests/run_regression_suite.sh
```

Pass criteria:
- All MLPerf workloads complete with `errors=0` in PUT and GET phases.
- Porcupine fs and s3 backends both print `RESULT: LINEARIZABLE`.
- Router fallback probe reports `ROUTER_FALLBACK: PASS` (byte-identical tee-through).

Output: `/tmp/zs3_regression_<timestamp>/` with JSON + per-test logs.

---

**References (relative to this repo):**
- `cmd/gateway/zcn/initSDK.go` — `serverOptions` struct
- `cmd/gateway/zcn/wal.go` — `ShouldUseWAL`, `directThreshold`
- `cmd/gateway/zcn/fallback_s3.go` — `tryFallbackFetch`, tee cache-back
- `cmd/gateway/zcn/cache_router.go` — `TryCacheRead`
- `cmd/gateway/zcn/prewarm_router.go` — `/internal/prewarm`
- `CLAUDE.md`, `BENCHMARK_GUIDE.md`
