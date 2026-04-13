package zcn

import (
	"fmt"
	"io"
	"log"
	"mime"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	minio "github.com/minio/minio/cmd"
)

// CacheRouter checks local caches (NFS export, MinIO writeback) before
// falling back to blobber downloads. This avoids a round-trip to blobbers
// for objects that were recently written via NFS or S3 but not yet committed.
//
// Lookup order:
//   1. /nfs_export/{bucket}/{key}  — NFS-written files visible to S3
//   2. /mcache/{bucket}/{key}      — S3-written files in MinIO writeback cache
//   3. nil → caller falls through to getFileReader (blobber download)

// CacheRouterStats tracks hit/miss counters for monitoring.
type CacheRouterStats struct {
	NFSHits    atomic.Int64
	S3Hits     atomic.Int64
	Misses     atomic.Int64
}

var cacheStats CacheRouterStats

// CacheRouterResult is returned when a cached file is found locally.
type CacheRouterResult struct {
	Reader     io.ReadCloser
	ObjectInfo *minio.ObjectInfo
	Source     string // "nfs_export" or "mcache"
}

// TryCacheRead checks local caches for the requested object.
// Returns nil if the object is not found in any cache (caller should
// fall through to blobber download via getFileReader).
//
// rangeStart/rangeEnd are used to seek into the file if a range request
// is made. If rangeEnd < rangeStart, the entire file is returned.
func TryCacheRead(bucket, object string, rangeStart, rangeEnd int64) *CacheRouterResult {
	nfsDir := serverConfig.NFSGaneshaExportDir
	if nfsDir == "" {
		nfsDir = "/nfs_export"
	}

	// 1. Check NFS export directory
	nfsPath := filepath.Join(nfsDir, bucket, object)
	if result := tryLocalFile(nfsPath, bucket, object, "nfs_export", rangeStart, rangeEnd); result != nil {
		cacheStats.NFSHits.Add(1)
		log.Printf("[CacheRouter] NFS hit: %s/%s", bucket, object)
		return result
	}

	// 2. Check MinIO writeback cache
	mcachePath := filepath.Join("/mcache", bucket, object)
	if result := tryLocalFile(mcachePath, bucket, object, "mcache", rangeStart, rangeEnd); result != nil {
		cacheStats.S3Hits.Add(1)
		log.Printf("[CacheRouter] S3 cache hit: %s/%s", bucket, object)
		return result
	}

	cacheStats.Misses.Add(1)
	return nil
}

// tryLocalFile opens a local file and returns a CacheRouterResult if it exists
// and is a regular file. Returns nil if the file doesn't exist or can't be read.
func tryLocalFile(localPath, bucket, object, source string, rangeStart, rangeEnd int64) *CacheRouterResult {
	fi, err := os.Stat(localPath)
	if err != nil || fi.IsDir() {
		return nil
	}

	f, err := os.Open(localPath)
	if err != nil {
		return nil
	}

	fileSize := fi.Size()

	// Handle range requests
	if rangeStart > 0 && rangeStart < fileSize {
		if _, err := f.Seek(rangeStart, io.SeekStart); err != nil {
			f.Close()
			return nil
		}
	}

	var reader io.ReadCloser = f
	returnSize := fileSize
	if rangeEnd >= rangeStart && rangeEnd < fileSize {
		returnSize = rangeEnd - rangeStart + 1
		reader = &limitedReadCloser{
			R: io.LimitReader(f, returnSize),
			C: f,
		}
	}

	contentType := mime.TypeByExtension(filepath.Ext(object))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	objInfo := &minio.ObjectInfo{
		Bucket:      bucket,
		Name:        object,
		ModTime:     fi.ModTime(),
		Size:        returnSize,
		ContentType: contentType,
		ETag:        fmt.Sprintf("%x-%d", fi.ModTime().UnixNano(), fi.Size()),
	}

	return &CacheRouterResult{
		Reader:     reader,
		ObjectInfo: objInfo,
		Source:     source,
	}
}

// limitedReadCloser wraps a LimitReader with a Closer from the underlying file.
type limitedReadCloser struct {
	R io.Reader
	C io.Closer
}

func (l *limitedReadCloser) Read(p []byte) (int, error) { return l.R.Read(p) }
func (l *limitedReadCloser) Close() error               { return l.C.Close() }

// CacheStatsSnapshot returns a point-in-time snapshot for monitoring.
func CacheStatsSnapshot() map[string]int64 {
	return map[string]int64{
		"nfs_hits":  cacheStats.NFSHits.Load(),
		"s3_hits":   cacheStats.S3Hits.Load(),
		"misses":    cacheStats.Misses.Load(),
	}
}

// ResetCacheStats zeroes the counters (useful for benchmarks).
func ResetCacheStats() {
	cacheStats.NFSHits.Store(0)
	cacheStats.S3Hits.Store(0)
	cacheStats.Misses.Store(0)
}

// CacheStatsReport returns a formatted string for logging.
func CacheStatsReport() string {
	s := CacheStatsSnapshot()
	total := s["nfs_hits"] + s["s3_hits"] + s["misses"]
	if total == 0 {
		return "[CacheRouter] no requests"
	}
	hitPct := float64(s["nfs_hits"]+s["s3_hits"]) / float64(total) * 100
	return fmt.Sprintf("[CacheRouter] hits=%d (nfs=%d s3=%d) misses=%d hit_rate=%.1f%%",
		s["nfs_hits"]+s["s3_hits"], s["nfs_hits"], s["s3_hits"], s["misses"], hitPct)
}

func init() {
	// Log cache stats every 60 seconds if there's any activity
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		var lastTotal int64
		for range ticker.C {
			s := CacheStatsSnapshot()
			total := s["nfs_hits"] + s["s3_hits"] + s["misses"]
			if total > lastTotal {
				log.Println(CacheStatsReport())
				lastTotal = total
			}
		}
	}()
}
