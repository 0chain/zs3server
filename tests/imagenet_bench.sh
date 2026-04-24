#!/bin/bash
# ImageNet benchmark for NFS + S3 storage performance
#
# Downloads ImageNet-mini (~1.2GB, 38K images) or full ImageNet subset,
# uploads to Züs via NFS, then benchmarks read throughput simulating
# ML training data loading.
#
# Usage:
#   bash tests/imagenet_bench.sh [--size mini|full] [--nfs-mount /mnt/zs3] [--s3-endpoint http://localhost:9000]

set -euo pipefail

NFS_MOUNT="${NFS_MOUNT:-/mnt/zus_nfs}"
S3_ENDPOINT="${S3_ENDPOINT:-http://198.18.0.2:9000}"
S3_ACCESS_KEY="${S3_ACCESS_KEY:-rootroot}"
S3_SECRET_KEY="${S3_SECRET_KEY:-rootroot}"
DATASET_SIZE="${1:-mini}"  # mini (~1.2GB) or full (~14GB subset)
BUCKET="imagenet"
WORKDIR="/tmp/imagenet_bench"

echo "=== ImageNet Storage Benchmark ==="
echo "NFS mount: $NFS_MOUNT"
echo "S3 endpoint: $S3_ENDPOINT"
echo "Dataset: $DATASET_SIZE"
echo ""

mkdir -p "$WORKDIR"

# ============================================================
# Step 1: Generate synthetic ImageNet-like dataset
# (Avoids downloading real ImageNet which needs credentials)
# ============================================================
echo "--- Step 1: Generating synthetic ImageNet dataset ---"

python3 << 'PYGEN'
import os, sys, struct, random, time

workdir = "/tmp/imagenet_bench/dataset"
size = os.environ.get("DATASET_SIZE", "mini")

if size == "mini":
    num_classes = 100
    images_per_class = 50
    img_size_range = (50*1024, 300*1024)  # 50-300KB (JPEG-like)
    # Total: 100 * 50 = 5,000 images, ~750MB
elif size == "full":
    num_classes = 1000
    images_per_class = 50
    img_size_range = (50*1024, 300*1024)
    # Total: 1000 * 50 = 50,000 images, ~7.5GB
else:
    num_classes = 100
    images_per_class = 50
    img_size_range = (50*1024, 300*1024)

os.makedirs(workdir, exist_ok=True)
total_files = 0
total_bytes = 0
start = time.time()

for cls in range(num_classes):
    cls_dir = os.path.join(workdir, f"n{cls:08d}")
    os.makedirs(cls_dir, exist_ok=True)
    for img in range(images_per_class):
        fsize = random.randint(*img_size_range)
        fpath = os.path.join(cls_dir, f"img_{img:05d}.JPEG")
        if not os.path.exists(fpath):
            with open(fpath, 'wb') as f:
                f.write(os.urandom(fsize))
        total_files += 1
        total_bytes += fsize

elapsed = time.time() - start
print(f"Generated {total_files} files ({total_bytes/1024/1024:.0f} MB) in {elapsed:.1f}s")
PYGEN

DATASET_DIR="$WORKDIR/dataset"
FILE_COUNT=$(find "$DATASET_DIR" -name '*.JPEG' | wc -l)
DATASET_MB=$(du -sm "$DATASET_DIR" | awk '{print $1}')
echo "Dataset: $FILE_COUNT files, ${DATASET_MB}MB"

# ============================================================
# Step 2: Upload to Züs via NFS (measures write throughput)
# ============================================================
echo ""
echo "--- Step 2: Upload to Züs via NFS ---"

NFS_DEST="$NFS_MOUNT/$BUCKET"
mkdir -p "$NFS_DEST" 2>/dev/null || true

START=$(date +%s%N)
cp -r "$DATASET_DIR"/* "$NFS_DEST/" 2>/dev/null
END=$(date +%s%N)
ELAPSED_MS=$(( (END - START) / 1000000 ))
WRITE_MBS=$(echo "scale=1; $DATASET_MB * 1000 / $ELAPSED_MS" | bc)
WRITE_OPS=$(echo "scale=0; $FILE_COUNT * 1000 / $ELAPSED_MS" | bc)
echo "NFS WRITE: $FILE_COUNT files (${DATASET_MB}MB) in ${ELAPSED_MS}ms = ${WRITE_OPS} files/s, ${WRITE_MBS} MB/s"

# ============================================================
# Step 3: Read benchmark — simulate ML DataLoader
# ============================================================
echo ""
echo "--- Step 3: Read benchmark (simulating ML DataLoader) ---"

# Drop caches for cold read
sync; echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || true

python3 << 'PYREAD'
import os, time, random, concurrent.futures

nfs_mount = os.environ.get("NFS_MOUNT", "/mnt/zus_nfs")
bucket = "imagenet"
dataset_dir = os.path.join(nfs_mount, bucket)

# Collect all image files
all_files = []
for root, dirs, files in os.walk(dataset_dir):
    for f in files:
        if f.endswith('.JPEG'):
            all_files.append(os.path.join(root, f))

if not all_files:
    print("ERROR: No files found in", dataset_dir)
    exit(1)

print(f"Found {len(all_files)} images in {dataset_dir}")

# Simulate ML DataLoader: random reads with multiple workers
batch_size = 64
num_workers_list = [1, 4, 8, 16]

for num_workers in num_workers_list:
    # Shuffle for random access (like real training)
    random.shuffle(all_files)
    num_batches = min(100, len(all_files) // batch_size)
    total_files = num_batches * batch_size
    total_bytes = 0

    def read_file(path):
        with open(path, 'rb') as f:
            data = f.read()
        return len(data)

    start = time.time()
    with concurrent.futures.ThreadPoolExecutor(max_workers=num_workers) as ex:
        results = list(ex.map(read_file, all_files[:total_files]))
    elapsed = time.time() - start
    total_bytes = sum(results)

    files_per_sec = total_files / elapsed
    mb_per_sec = total_bytes / 1024 / 1024 / elapsed
    batches_per_sec = num_batches / elapsed

    print(f"  Workers={num_workers:>2d}: {files_per_sec:>7.0f} img/s, {mb_per_sec:>6.1f} MB/s, "
          f"{batches_per_sec:>5.1f} batches/s (batch_size={batch_size})")

# Warm read (cached)
print("\n  --- Warm reads (cached) ---")
for num_workers in [8, 16]:
    random.shuffle(all_files)
    total_files = min(len(all_files), num_batches * batch_size)
    start = time.time()
    with concurrent.futures.ThreadPoolExecutor(max_workers=num_workers) as ex:
        results = list(ex.map(read_file, all_files[:total_files]))
    elapsed = time.time() - start
    total_bytes = sum(results)
    files_per_sec = total_files / elapsed
    mb_per_sec = total_bytes / 1024 / 1024 / elapsed
    print(f"  Workers={num_workers:>2d}: {files_per_sec:>7.0f} img/s, {mb_per_sec:>6.1f} MB/s (warm)")
PYREAD

# ============================================================
# Step 4: S3 read benchmark (for comparison)
# ============================================================
echo ""
echo "--- Step 4: S3 read benchmark (boto3) ---"

python3 << 'PYS3'
import os, time, random, concurrent.futures
import boto3
from botocore.config import Config

endpoint = os.environ.get("S3_ENDPOINT", "http://198.18.0.2:9000")
access_key = os.environ.get("S3_ACCESS_KEY", "rootroot")
secret_key = os.environ.get("S3_SECRET_KEY", "rootroot")
bucket = "imagenet"

s3 = boto3.client("s3", endpoint_url=endpoint,
                   aws_access_key_id=access_key,
                   aws_secret_access_key=secret_key,
                   config=Config(max_pool_connections=50))

# List all objects
paginator = s3.get_paginator('list_objects_v2')
all_keys = []
for page in paginator.paginate(Bucket=bucket, MaxKeys=1000):
    for obj in page.get('Contents', []):
        all_keys.append(obj['Key'])
    if len(all_keys) >= 5000:
        break

if not all_keys:
    print("No objects found in S3 bucket", bucket)
    exit(0)

print(f"Found {len(all_keys)} objects in S3 bucket {bucket}")

batch_size = 64
num_batches = min(50, len(all_keys) // batch_size)
total_files = num_batches * batch_size

def read_s3(key):
    resp = s3.get_object(Bucket=bucket, Key=key)
    data = resp['Body'].read()
    return len(data)

for num_workers in [8, 16]:
    random.shuffle(all_keys)
    start = time.time()
    with concurrent.futures.ThreadPoolExecutor(max_workers=num_workers) as ex:
        results = list(ex.map(read_s3, all_keys[:total_files]))
    elapsed = time.time() - start
    total_bytes = sum(results)
    files_per_sec = total_files / elapsed
    mb_per_sec = total_bytes / 1024 / 1024 / elapsed
    print(f"  S3 Workers={num_workers:>2d}: {files_per_sec:>6.0f} img/s, {mb_per_sec:>6.1f} MB/s")
PYS3

echo ""
echo "=== Benchmark Complete ==="
echo ""
echo "For ML training, the key metric is batches/sec at batch_size=64."
echo "PyTorch ResNet-50 training needs ~300 img/s on 1 GPU."
echo "Our NFS path should provide 5,000+ img/s (16x headroom)."
