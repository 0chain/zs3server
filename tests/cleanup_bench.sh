#!/bin/bash
# cleanup_bench.sh - Clean benchmark artifacts from NFS export, S3 cache, WAL, and docker logs.
#
# Usage:
#   bash tests/cleanup_bench.sh             # execute cleanup
#   bash tests/cleanup_bench.sh --dry-run   # show what would be cleaned

set -euo pipefail

DRY_RUN=false
if [[ "${1:-}" == "--dry-run" ]]; then
    DRY_RUN=true
    echo "=== DRY RUN - no changes will be made ==="
fi

NFS_EXPORT="${NFS_EXPORT:-/nfs_export}"
MCACHE="${MCACHE:-/mcache}"
WAL_DIR="${WAL_DIR:-$HOME/.zcn/wal}"
BENCH_BUCKET="${BENCH_BUCKET:-benchtest}"
DOCKER_LOG_MAX=$((10 * 1024 * 1024))  # 10MB

run_or_show() {
    if $DRY_RUN; then
        echo "  [dry-run] $*"
    else
        eval "$@"
    fi
}

echo ""
echo "=== Disk usage BEFORE cleanup ==="
echo "--- NFS export ---"
du -sh "$NFS_EXPORT" 2>/dev/null || echo "  (not found: $NFS_EXPORT)"
echo "--- S3 cache ---"
du -sh "$MCACHE" 2>/dev/null || echo "  (not found: $MCACHE)"
echo "--- WAL ---"
du -sh "$WAL_DIR" 2>/dev/null || echo "  (not found: $WAL_DIR)"
echo "--- Docker logs ---"
total_log_size=0
for logfile in /var/lib/docker/containers/*/*-json.log; do
    if [[ -f "$logfile" ]]; then
        sz=$(stat -f%z "$logfile" 2>/dev/null || stat -c%s "$logfile" 2>/dev/null || echo 0)
        total_log_size=$((total_log_size + sz))
    fi
done 2>/dev/null
echo "  Total docker log size: $((total_log_size / 1024 / 1024))MB"
echo ""

# 1. Clean NFS export benchmark data
echo "=== Step 1: Clean NFS export ($NFS_EXPORT/$BENCH_BUCKET) ==="
if [[ -d "$NFS_EXPORT/$BENCH_BUCKET" ]]; then
    count=$(find "$NFS_EXPORT/$BENCH_BUCKET" -type f 2>/dev/null | wc -l | tr -d ' ')
    echo "  Found $count files"
    run_or_show "rm -rf '$NFS_EXPORT/$BENCH_BUCKET'/*"
else
    echo "  Directory not found, skipping"
fi
echo ""

# 2. Clean S3 writeback cache - stop minioserver, remount tmpfs, restart
echo "=== Step 2: Clean S3 cache ($MCACHE) ==="
if mountpoint -q "$MCACHE" 2>/dev/null; then
    echo "  $MCACHE is a tmpfs mount"
    if $DRY_RUN; then
        echo "  [dry-run] would stop minioserver, remount tmpfs at $MCACHE, restart"
    else
        echo "  Stopping minioserver..."
        pkill -f minioserver 2>/dev/null || true
        sleep 1
        echo "  Remounting tmpfs..."
        umount "$MCACHE" 2>/dev/null || true
        mount -t tmpfs -o size=4G tmpfs "$MCACHE"
        echo "  Restarting minioserver..."
        # Find and restart the server. Caller may need to adjust this.
        if command -v systemctl &>/dev/null && systemctl is-active --quiet minioserver 2>/dev/null; then
            systemctl restart minioserver
        else
            echo "  WARNING: minioserver not managed by systemd. Restart manually."
        fi
    fi
elif [[ -d "$MCACHE" ]]; then
    echo "  $MCACHE is a regular directory (not tmpfs)"
    count=$(find "$MCACHE" -type f 2>/dev/null | wc -l | tr -d ' ')
    echo "  Found $count files"
    run_or_show "rm -rf '$MCACHE'/*"
else
    echo "  Directory not found, skipping"
fi
echo ""

# 3. Clear WAL
echo "=== Step 3: Clear WAL ($WAL_DIR) ==="
if [[ -d "$WAL_DIR" ]]; then
    count=$(find "$WAL_DIR" -type f 2>/dev/null | wc -l | tr -d ' ')
    echo "  Found $count WAL files"
    run_or_show "rm -rf '$WAL_DIR'/*"
else
    echo "  WAL directory not found, skipping"
fi
echo ""

# 4. Truncate large docker container logs
echo "=== Step 4: Truncate docker logs > 10MB ==="
truncated=0
for logfile in /var/lib/docker/containers/*/*-json.log; do
    if [[ -f "$logfile" ]]; then
        sz=$(stat -f%z "$logfile" 2>/dev/null || stat -c%s "$logfile" 2>/dev/null || echo 0)
        if [[ "$sz" -gt "$DOCKER_LOG_MAX" ]]; then
            container_id=$(basename "$(dirname "$logfile")")
            echo "  ${container_id:0:12}: $((sz / 1024 / 1024))MB"
            run_or_show "truncate -s 0 '$logfile'"
            truncated=$((truncated + 1))
        fi
    fi
done 2>/dev/null
if [[ $truncated -eq 0 ]]; then
    echo "  No logs exceed 10MB"
fi
echo ""

# Report disk usage after cleanup
if ! $DRY_RUN; then
    echo "=== Disk usage AFTER cleanup ==="
    echo "--- NFS export ---"
    du -sh "$NFS_EXPORT" 2>/dev/null || echo "  (not found)"
    echo "--- S3 cache ---"
    du -sh "$MCACHE" 2>/dev/null || echo "  (not found)"
    echo "--- WAL ---"
    du -sh "$WAL_DIR" 2>/dev/null || echo "  (not found)"
    echo ""
fi

echo "=== Done ==="
