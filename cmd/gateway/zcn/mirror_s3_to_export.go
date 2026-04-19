// Copyright (c) 2015-2021 MinIO, Inc.
//
// mirror_s3_to_export: reflect S3-handler commits into /nfs_export so the NFS
// namespace (served by NFS-Ganesha + FSAL_ZUS) sees objects written via S3.
// Symmetric to BlobberSync (NFS -> Züs). The placeholder is a sparse file of
// the correct size with user.zus.stub set; FSAL_ZUS open2 triggers
// /internal/prewarm which overwrites with real bytes on first NFS read.
//
// Called from the happy-path of PutObject / PutMultipleObjects / CopyObject /
// CompleteMultipartUpload / MakeBucketWithLocation / Delete* after the Züs
// commit has succeeded. No-op when NFSGaneshaExportDir is empty.
package zcn

import (
	"context"
	"os"
	"path/filepath"
	"syscall"

	"github.com/minio/minio/internal/logger"
)

func exportRelPath(bucket, object string) string {
	if bucket == "" || bucket == rootBucketName {
		return object
	}
	return filepath.Join(bucket, object)
}

// mirrorS3PutToExport creates a sparse stub placeholder in /nfs_export with
// the committed size and user.zus.stub xattr. MarkCommitted first so the
// inotify Create is ignored by BlobberSync; the xattr is second-line defense.
func mirrorS3PutToExport(bucket, object string, size int64) {
	if serverConfig.NFSGaneshaExportDir == "" || object == "" {
		return
	}
	rel := exportRelPath(bucket, object)
	if rel == "" {
		return
	}
	target := filepath.Join(serverConfig.NFSGaneshaExportDir, rel)

	if currentBS != nil {
		currentBS.MarkCommitted(rel)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		logger.LogIf(context.Background(), err)
		return
	}
	// If a real (non-stub) file with matching size already exists, leave it —
	// the NFS side may have written authoritative bytes already.
	if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
		var buf [4]byte
		n, _ := syscall.Getxattr(target, "user.zus.stub", buf[:])
		if n == 0 && fi.Size() == size {
			return
		}
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		logger.LogIf(context.Background(), err)
		return
	}
	f.Close()
	if size > 0 {
		if err := os.Truncate(target, size); err != nil {
			logger.LogIf(context.Background(), err)
		}
	}
	if err := syscall.Setxattr(target, "user.zus.stub", []byte{'1'}, 0); err != nil {
		logger.LogIf(context.Background(), err)
	}
	var st syscall.Stat_t
	if syscall.Lstat(target, &st) == nil {
		inodeRelSet(st.Ino, rel)
	}
}

// mirrorS3MakeDirToExport creates a directory entry in /nfs_export (used for
// S3 PUT of an object key ending in "/" which denotes a directory).
func mirrorS3MakeDirToExport(bucket, object string) {
	if serverConfig.NFSGaneshaExportDir == "" {
		return
	}
	rel := exportRelPath(bucket, object)
	if rel == "" {
		return
	}
	if err := os.MkdirAll(filepath.Join(serverConfig.NFSGaneshaExportDir, rel), 0o755); err != nil {
		logger.LogIf(context.Background(), err)
	}
}

// mirrorS3MakeBucketToExport creates /nfs_export/<bucket> so NFS opendir works
// immediately after an S3 mb.
func mirrorS3MakeBucketToExport(bucket string) {
	if serverConfig.NFSGaneshaExportDir == "" || bucket == "" || bucket == rootBucketName {
		return
	}
	if err := os.MkdirAll(filepath.Join(serverConfig.NFSGaneshaExportDir, bucket), 0o755); err != nil {
		logger.LogIf(context.Background(), err)
	}
}

// mirrorS3DeleteObjectToExport removes the placeholder and suppresses the
// resulting inotify Remove so BlobberSync does not re-issue a delete op
// against Züs (which already ran at the S3 handler).
func mirrorS3DeleteObjectToExport(bucket, object string) {
	if serverConfig.NFSGaneshaExportDir == "" || object == "" {
		return
	}
	rel := exportRelPath(bucket, object)
	if rel == "" {
		return
	}
	target := filepath.Join(serverConfig.NFSGaneshaExportDir, rel)
	if currentBS != nil {
		currentBS.SkipNextRemove(rel)
	}
	_ = os.Remove(target)
}

// mirrorS3DeleteBucketToExport removes /nfs_export/<bucket> tree and suppresses
// inotify Remove events for every child so BlobberSync does not replay the
// deletes against Züs.
func mirrorS3DeleteBucketToExport(bucket string) {
	if serverConfig.NFSGaneshaExportDir == "" || bucket == "" || bucket == rootBucketName {
		return
	}
	if currentBS != nil {
		currentBS.SkipRemoveSubtree(bucket)
	}
	_ = os.RemoveAll(filepath.Join(serverConfig.NFSGaneshaExportDir, bucket))
}
