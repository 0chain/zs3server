# Züs Storage Benchmark Guide

## Architecture

```
App (Spark, PuppyGraph, PyTorch, etc.)
  │ S3 API or NFS mount
  ▼
zs3server (unified service on storage node)
  │
  ├── /nfs_export (tmpfs 8GB) ← unified local cache
  │     Served by NFS-Ganesha (port 2049, NFSv4, nconnect=16)
  │     Serves both NFS mounts and S3 GET (via CacheRouter)
  │
  ├── MinIO S3 Gateway (port 9000)
  │     S3 protocol compliance (signatures, multipart, IAM)
  │     CacheRouter checks /nfs_export before blobber fetch
  │
  ├── BlobberSync (inotify → batch DoMultiOperation)
  │     Commits /nfs_export files to blobbers async
  │     Batch size 25, 8 workers, 200ms debounce
  │
  └── External S3 Router
        If object not in Züs → fetch from AWS S3
        Store in /nfs_export → blobber sync → Züs

Blobbers (erasure-coded, blockchain-verified, distributed)
```

## How to Run Benchmarks

### Prerequisites

```bash
# On storage node (test2: 144.76.58.147)
apt install -y nfs-ganesha nfs-ganesha-vfs
pip3 install --break-system-packages pyspark==3.5.0 boto3

# Deploy chain + blobbers
cd /root/Code/system_test
bash scripts/deploy_local.sh redeploy

# Setup NFS-Ganesha
mount -t tmpfs -o size=8G tmpfs /nfs_export
mkdir -p /nfs_export /var/run/ganesha
ganesha.nfsd -f /etc/ganesha/ganesha.conf -L /var/log/ganesha/ganesha.log -N WARN

# Mount NFS (on same host or compute node)
echo 2 > /proc/sys/sunrpc/tcp_slot_table_entries
mount -t nfs4 -o nconnect=16 localhost:/ /mnt/zus_nfs
```

### Ganesha Config (/etc/ganesha/ganesha.conf)

```
NFS_CORE_PARAM { NFS_Protocols = 3, 4; }
NFSV4 { Grace_Period = 0; }
EXPORT {
    Export_Id = 1;
    Path = /nfs_export;
    Pseudo = /;
    Access_Type = RW;
    Squash = No_Root_Squash;
    SecType = sys;
    FSAL { Name = VFS; }
}
LOG { Default_Log_Level = WARN; }
```

### 1. Small File Throughput (obj/s)

```bash
# NFS PUT/GET at various file sizes
python3 -c "
import os, time, concurrent.futures
for sk, cnt, c in [(1,5000,128), (10,2000,64), (100,1000,64), (1024,200,64)]:
    data = os.urandom(sk*1024)
    files = [f'/mnt/zus_nfs/bench/f_{i}.bin' for i in range(cnt)]
    def put(p):
        with open(p,'wb') as f: f.write(data)
    start = time.time()
    with concurrent.futures.ThreadPoolExecutor(c) as ex: list(ex.map(put, files))
    elapsed = time.time() - start
    print(f'PUT {sk}KB c={c}: {cnt/elapsed:.0f} obj/s  {cnt*sk/1024/elapsed:.1f} MB/s')
    # cleanup
    for f in files:
        try: os.remove(f)
        except: pass
"

# S3 via warp (Go-native benchmark)
warp put --host=198.18.0.2:9000 --access-key=rootroot --secret-key=rootroot \
  --obj.size 1KiB --concurrent 128 --duration 10s --noclear
```

### 2. MLPerf Storage Benchmarks

```bash
# Generate synthetic ImageNet (5000 images, ~750MB)
mkdir -p /mnt/zus_nfs/imagenet
python3 -c "
import os, random
for cls in range(100):
    d = f'/mnt/zus_nfs/imagenet/n{cls:08d}'
    os.makedirs(d, exist_ok=True)
    for img in range(50):
        with open(f'{d}/img_{img:05d}.JPEG', 'wb') as f:
            f.write(os.urandom(random.randint(50*1024, 300*1024)))
"

# ResNet-50 / ImageNet read benchmark
python3 -c "
import os, time, random, concurrent.futures
all_files = [os.path.join(r,f) for r,d,fs in os.walk('/mnt/zus_nfs/imagenet') for f in fs]
def read(p):
    with open(p,'rb') as f: return len(f.read())
for nw in [1, 4, 8, 16, 32]:
    random.shuffle(all_files)
    n = min(len(all_files), 3200)
    os.system('echo 3 > /proc/sys/vm/drop_caches 2>/dev/null')
    start = time.time()
    with concurrent.futures.ThreadPoolExecutor(nw) as ex:
        results = list(ex.map(read, all_files[:n]))
    elapsed = time.time() - start
    total = sum(results)
    print(f'Workers={nw}: {n/elapsed:.0f} img/s  {total/1024/1024/elapsed:.1f} MB/s')
"
```

#### UNet3D (3D medical segmentation)

```bash
# Generate 20 synthetic 3D volumes (100-500MB each)
python3 -c "
import os, random
for i in range(20):
    size = random.randint(100,500) * 1024 * 1024
    os.makedirs(f'/mnt/zus_nfs/unet3d/case_{i:05d}', exist_ok=True)
    with open(f'/mnt/zus_nfs/unet3d/case_{i:05d}/imaging.nii.gz', 'wb') as f:
        remaining = size
        while remaining > 0:
            chunk = min(remaining, 1024*1024)
            f.write(os.urandom(chunk))
            remaining -= chunk
"
# Read benchmark same pattern as above with 1-8 workers
```

#### BERT (NLP preprocessing chunks)

```bash
# Generate 30 files, 10-50MB each (Wikipedia preprocessed chunks)
python3 -c "
import os, random
for i in range(30):
    size = random.randint(10,50) * 1024 * 1024
    with open(f'/mnt/zus_nfs/bert/wiki_{i:04d}.tfrecord', 'wb') as f:
        remaining = size
        while remaining > 0:
            f.write(os.urandom(min(remaining, 1024*1024)))
            remaining -= 1024*1024
"
```

### 3. TPC-DS (Spark SQL)

```bash
# Generate TPC-DS data using PySpark
# 1GB scale: ~5M rows, 192MB Parquet
# 1TB scale: ~200M rows, ~40GB Parquet (representative subset)
python3 << 'EOF'
from pyspark.sql import SparkSession
spark = SparkSession.builder.master('local[*]').appName('tpcds').config('spark.driver.memory','16g').getOrCreate()
# See tests/tpcds_gen.py for full generation script
EOF

# Run TPC-DS queries
pyspark --conf spark.driver.memory=16g << 'EOF'
store_sales = spark.read.parquet('/nfs_export/tpcds_1tb/store_sales')
store_sales.createOrReplaceTempView('store_sales')
# Q1: Top stores by revenue
spark.sql('SELECT ss_store_sk, SUM(ss_net_paid) as total FROM store_sales GROUP BY ss_store_sk ORDER BY total DESC LIMIT 10').show()
EOF
```

### 4. Dual-Access Tests (S3 <-> NFS)

```bash
# Shell script with 10 test cases
bash tests/dual_access_test.sh

# Go integration test
cd /path/to/system_test/tests/cli_tests/zs3server_tests
ZS3_NFS_MOUNT=/mnt/zus_nfs go test -v -run TestZs3serverDualAccess
```

### 5. Cleanup After Benchmarks

```bash
bash tests/cleanup_bench.sh        # Clean caches, truncate logs
bash tests/cleanup_bench.sh --dry-run  # Preview only
```

## Measured Results

### Test Environment
- Server: test2.zus.network (144.76.58.147)
- CPU: 12 cores
- RAM: 64 GB
- Storage: 2x Samsung 512GB NVMe SSD (RAID1, md2)
- NFS: Ganesha 4.3, NFSv4, nconnect=16, tmpfs 8GB
- Chain: 4 miners, 2 sharders, 12 blobbers (fix/dkg-broadcast-fee)

### Small File Throughput (NFS-Ganesha on tmpfs)

| Size | PUT obj/s | PUT MB/s | GET obj/s | GET MB/s |
|------|-----------|----------|-----------|----------|
| 1 KiB | 5,822 | 5.7 | 10,283 | 10.0 |
| 10 KiB | 6,420 | 62.7 | 9,899 | 96.7 |
| 100 KiB | 5,431 | 530 | 8,559 | 836 |
| 1 MiB | 1,482 | 1,482 | 4,846 | 4,846 |
| 10 MiB | 192 | 1,916 | 295 | 2,954 |
| 100 MiB (c=8) | 19 | 1,892 | 27 | 2,701 |
| 1 GiB (c=6) | 1.8 | 1,894 | 3.6 | 3,719 |

### S3 warp (clean cache per size)

| Size | PUT obj/s | PUT MB/s | GET obj/s | GET MB/s |
|------|-----------|----------|-----------|----------|
| 1 KiB | 3,836 | 3.7 | 5,414 | 5.3 |
| 10 KiB | 2,956 | 28.9 | 4,552 | 44.5 |
| 100 KiB | 1,976 | 193 | 3,844 | 375 |
| 1 MiB | 896 | 896 | 544 | 544 |
| 10 MiB | 60 | 601 | 134 | 1,336 |

### MLPerf Storage v2.0 Workloads

| Workload | Access Pattern | Read MB/s (8 workers, cold) | Comparison |
|----------|---------------|----------------------------|------------|
| ResNet-50 / ImageNet | Random 177KB | 626 | Alluxio: 24,140 MB/s (multi-node cluster) |
| UNet3D / KiTS19 | Sequential 300MB | 1,603 | Alluxio: 23,160 MB/s (multi-node) |
| BERT / Wikipedia | Sequential 28MB | 1,545 | — |
| CosmoFlow | Sequential 128MB | 1,392 | Alluxio: 4,310 MB/s (multi-node) |
| DLRM / Recommendation | Sequential 1GB | 1,200 | — |

Note: Alluxio numbers are from a multi-node NVMe cluster (hardware unspecified).
Our numbers are single-node, 12-core, tmpfs. Per-core throughput is competitive.

### TPC-DS (Spark SQL, 1GB scale)

| Query | Time | Description |
|-------|------|-------------|
| Q1 | 1.85s | Aggregation (GROUP BY, SUM, ORDER BY) |
| Q2 | 0.65s | Filtered count |
| Q3 | 2.44s | Join + aggregation |
| Q4 | 2.75s | Complex (GROUP BY, HAVING, ORDER BY) |
| Q5 | 0.24s | Window function |
| Full scan | 0.18s | 5M rows, 192MB = 1,074 MB/s |

### ML Training Headroom

| Framework | Required img/s | Our NFS img/s | Headroom |
|-----------|---------------|---------------|----------|
| ResNet-50 (1 GPU) | ~300 | 7,643 (cold) | 25x |
| UNet3D (1 GPU) | ~2 vol/s | 4.7 vol/s | 2.3x |
| BERT (1 GPU) | ~10 chunks/s | 56 chunks/s | 5.6x |
