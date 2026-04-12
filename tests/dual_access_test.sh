#!/bin/bash
# Dual-access test: verify S3 writes are visible via NFS and vice versa.
#
# Prerequisites:
#   - zs3server running with enable_nfs=true
#   - NFS mounted at $NFS_MOUNT
#   - S3 endpoint accessible at $S3_ENDPOINT
#   - mc configured with alias "zs3"
#
# Usage:
#   export S3_ENDPOINT="http://localhost:9000"
#   export S3_ACCESS_KEY="rootroot"
#   export S3_SECRET_KEY="rootroot"
#   export NFS_MOUNT="/mnt/zus_nfs"
#   bash tests/dual_access_test.sh

set -euo pipefail

S3_ENDPOINT="${S3_ENDPOINT:-http://localhost:9000}"
S3_ACCESS_KEY="${S3_ACCESS_KEY:-rootroot}"
S3_SECRET_KEY="${S3_SECRET_KEY:-rootroot}"
NFS_MOUNT="${NFS_MOUNT:-/mnt/zus_nfs}"
MC="${MC:-mc}"
BUCKET="dual-access-test-$(date +%s)"
PASS=0
FAIL=0

red()   { echo -e "\033[31m$*\033[0m"; }
green() { echo -e "\033[32m$*\033[0m"; }

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        green "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        red "  FAIL: $desc (expected='$expected', actual='$actual')"
        FAIL=$((FAIL + 1))
    fi
}

assert_file_exists() {
    local desc="$1" path="$2"
    if [ -e "$path" ]; then
        green "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        red "  FAIL: $desc (file not found: $path)"
        FAIL=$((FAIL + 1))
    fi
}

assert_file_not_exists() {
    local desc="$1" path="$2"
    if [ ! -e "$path" ]; then
        green "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        red "  FAIL: $desc (file still exists: $path)"
        FAIL=$((FAIL + 1))
    fi
}

# --- Setup ---
echo "=== Dual-Access Test: S3 <-> NFS ==="
echo "S3 endpoint: $S3_ENDPOINT"
echo "NFS mount:   $NFS_MOUNT"
echo "Bucket:      $BUCKET"
echo ""

# Check prerequisites
if [ ! -d "$NFS_MOUNT" ]; then
    red "ERROR: NFS mount not found at $NFS_MOUNT"
    exit 1
fi

# Configure mc alias
$MC alias set zs3test "$S3_ENDPOINT" "$S3_ACCESS_KEY" "$S3_SECRET_KEY" --api S3v4 >/dev/null 2>&1

# Create bucket via S3
$MC mb "zs3test/$BUCKET" >/dev/null 2>&1
sleep 1

# Generate test data
SMALL_DATA="hello-dual-access-$(date +%s)"
MEDIUM_FILE=$(mktemp)
dd if=/dev/urandom of="$MEDIUM_FILE" bs=1024 count=100 2>/dev/null
MEDIUM_MD5=$(md5sum "$MEDIUM_FILE" | awk '{print $1}')

# ============================================================
echo "--- Test 1: S3 PUT -> NFS READ ---"
# ============================================================

# Write via S3
echo -n "$SMALL_DATA" | $MC pipe "zs3test/$BUCKET/s3-to-nfs.txt" >/dev/null 2>&1
sleep 2  # writeback cache flush

# Read via NFS
NFS_CONTENT=$(cat "$NFS_MOUNT/$BUCKET/s3-to-nfs.txt" 2>/dev/null || echo "READ_FAILED")
assert_eq "S3 write -> NFS read (content match)" "$SMALL_DATA" "$NFS_CONTENT"

# ============================================================
echo "--- Test 2: NFS WRITE -> S3 READ ---"
# ============================================================

# Write via NFS
echo -n "$SMALL_DATA" > "$NFS_MOUNT/$BUCKET/nfs-to-s3.txt"
sleep 2

# Read via S3
S3_CONTENT=$($MC cat "zs3test/$BUCKET/nfs-to-s3.txt" 2>/dev/null || echo "READ_FAILED")
assert_eq "NFS write -> S3 read (content match)" "$SMALL_DATA" "$S3_CONTENT"

# ============================================================
echo "--- Test 3: S3 PUT (100KB) -> NFS READ (checksum) ---"
# ============================================================

$MC cp "$MEDIUM_FILE" "zs3test/$BUCKET/medium-s3.bin" >/dev/null 2>&1
sleep 2

NFS_MD5=$(md5sum "$NFS_MOUNT/$BUCKET/medium-s3.bin" 2>/dev/null | awk '{print $1}' || echo "MD5_FAILED")
assert_eq "S3 PUT 100KB -> NFS READ checksum" "$MEDIUM_MD5" "$NFS_MD5"

# ============================================================
echo "--- Test 4: NFS WRITE (100KB) -> S3 READ (checksum) ---"
# ============================================================

cp "$MEDIUM_FILE" "$NFS_MOUNT/$BUCKET/medium-nfs.bin"
sleep 2

S3_TMP=$(mktemp)
$MC cp "zs3test/$BUCKET/medium-nfs.bin" "$S3_TMP" >/dev/null 2>&1
S3_MD5=$(md5sum "$S3_TMP" | awk '{print $1}')
assert_eq "NFS WRITE 100KB -> S3 READ checksum" "$MEDIUM_MD5" "$S3_MD5"
rm -f "$S3_TMP"

# ============================================================
echo "--- Test 5: S3 LIST sees NFS-written files ---"
# ============================================================

S3_LIST=$($MC ls "zs3test/$BUCKET/" 2>/dev/null | awk '{print $NF}' | sort)
echo "$S3_LIST" | grep -q "nfs-to-s3.txt" && assert_eq "S3 LIST sees NFS file (nfs-to-s3.txt)" "found" "found" \
    || assert_eq "S3 LIST sees NFS file (nfs-to-s3.txt)" "found" "not_found"
echo "$S3_LIST" | grep -q "medium-nfs.bin" && assert_eq "S3 LIST sees NFS file (medium-nfs.bin)" "found" "found" \
    || assert_eq "S3 LIST sees NFS file (medium-nfs.bin)" "found" "not_found"

# ============================================================
echo "--- Test 6: NFS LIST sees S3-written files ---"
# ============================================================

NFS_LIST=$(ls "$NFS_MOUNT/$BUCKET/" 2>/dev/null | sort)
echo "$NFS_LIST" | grep -q "s3-to-nfs.txt" && assert_eq "NFS LIST sees S3 file (s3-to-nfs.txt)" "found" "found" \
    || assert_eq "NFS LIST sees S3 file (s3-to-nfs.txt)" "found" "not_found"
echo "$NFS_LIST" | grep -q "medium-s3.bin" && assert_eq "NFS LIST sees S3 file (medium-s3.bin)" "found" "found" \
    || assert_eq "NFS LIST sees S3 file (medium-s3.bin)" "found" "not_found"

# ============================================================
echo "--- Test 7: S3 DELETE -> NFS verify gone ---"
# ============================================================

$MC rm "zs3test/$BUCKET/s3-to-nfs.txt" >/dev/null 2>&1
sleep 2
assert_file_not_exists "S3 DELETE -> NFS file gone" "$NFS_MOUNT/$BUCKET/s3-to-nfs.txt"

# ============================================================
echo "--- Test 8: NFS DELETE -> S3 verify gone ---"
# ============================================================

rm -f "$NFS_MOUNT/$BUCKET/nfs-to-s3.txt" 2>/dev/null
sleep 2
S3_CHECK=$($MC stat "zs3test/$BUCKET/nfs-to-s3.txt" 2>&1 || true)
echo "$S3_CHECK" | grep -qi "does not exist\|not found\|Object does not exist" \
    && assert_eq "NFS DELETE -> S3 file gone" "gone" "gone" \
    || assert_eq "NFS DELETE -> S3 file gone" "gone" "still_exists"

# ============================================================
echo "--- Test 9: S3 overwrite -> NFS sees new content ---"
# ============================================================

echo -n "version1" | $MC pipe "zs3test/$BUCKET/overwrite.txt" >/dev/null 2>&1
sleep 2
echo -n "version2" | $MC pipe "zs3test/$BUCKET/overwrite.txt" >/dev/null 2>&1
sleep 2
NFS_VER=$(cat "$NFS_MOUNT/$BUCKET/overwrite.txt" 2>/dev/null || echo "READ_FAILED")
assert_eq "S3 overwrite -> NFS sees new content" "version2" "$NFS_VER"

# ============================================================
echo "--- Test 10: NFS overwrite -> S3 sees new content ---"
# ============================================================

echo -n "nfs-v1" > "$NFS_MOUNT/$BUCKET/nfs-overwrite.txt"
sleep 2
echo -n "nfs-v2" > "$NFS_MOUNT/$BUCKET/nfs-overwrite.txt"
sleep 2
S3_VER=$($MC cat "zs3test/$BUCKET/nfs-overwrite.txt" 2>/dev/null || echo "READ_FAILED")
assert_eq "NFS overwrite -> S3 sees new content" "nfs-v2" "$S3_VER"

# --- Cleanup ---
rm -f "$MEDIUM_FILE"
$MC rb --force "zs3test/$BUCKET" >/dev/null 2>&1 || true
$MC alias rm zs3test >/dev/null 2>&1 || true

# --- Summary ---
echo ""
echo "================================"
echo "Results: $PASS passed, $FAIL failed"
echo "================================"

if [ "$FAIL" -gt 0 ]; then
    red "DUAL-ACCESS TEST FAILED"
    exit 1
else
    green "ALL DUAL-ACCESS TESTS PASSED"
    exit 0
fi
