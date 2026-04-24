// Write-through / write-back from zs3server to the configured upstream S3.
//
// Called after a successful Züs commit (PUT / DELETE / CompleteMultipartUpload)
// when serverConfig.FallbackS3WriteThrough != "". Two modes:
//
//   "async"  — fire-and-forget goroutine; bounded concurrency; retries up to
//              fallbackWriteMaxAttempts times with exponential backoff; on
//              final failure, logs and records to an in-memory miss counter
//              for observability. This is the S3 Files "EFS primary, S3
//              asynchronously synced" analog.
//   "mirror" — synchronous: the caller blocks until upstream succeeds (or
//              all retries fail, in which case the caller receives an error
//              and should roll back the Züs write). Dual-write durability.
//
// Inbound PUT payload is passed as []byte (data already buffered locally in
// /nfs_export during the Züs commit), so retries don't need to re-read from
// blobbers. For CompleteMultipartUpload the caller passes the assembled
// file path and we stream it.

package zcn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio/internal/logger"
)

const (
	fallbackWriteMaxAttempts = 4
	fallbackWriteBaseBackoff = 500 * time.Millisecond
	fallbackWriteMaxConcur   = 16
)

var (
	fallbackWriteSem      = make(chan struct{}, fallbackWriteMaxConcur)
	fallbackWriteInFlight sync.WaitGroup

	fallbackWriteStats struct {
		Attempts        atomic.Int64
		Successes       atomic.Int64
		Failures        atomic.Int64
		Retries         atomic.Int64
		BytesReplicated atomic.Int64
		LastFailure     atomic.Value // holds string
	}

	ErrWriteThroughDisabled = errors.New("fallback_s3: write-through disabled")
	ErrWriteThroughSkipSize = errors.New("fallback_s3: file above size threshold")
)

// writeThroughEnabled returns the active mode, or "" when disabled.
func writeThroughEnabled() string {
	if !serverConfig.FallbackS3Enabled {
		return ""
	}
	switch serverConfig.FallbackS3WriteThrough {
	case "async", "mirror":
		return serverConfig.FallbackS3WriteThrough
	}
	return ""
}

// withinSizeLimit gates by FallbackS3WriteMaxBytes (0 = no limit).
func withinSizeLimit(size int64) bool {
	lim := serverConfig.FallbackS3WriteMaxBytes
	return lim == 0 || size <= lim
}

// syncPutToUpstream replicates a successful Züs PUT to the upstream bucket.
// Call AFTER Züs commit succeeds. Safe to call with any mode; skips on empty.
//
// Inputs:
//
//	bucket, key   — local bucket/key; remapped via FallbackBucketMap
//	payload       — full object body (must be retriable — i.e., buffered, not
//	                a one-shot pipe). For large objects, prefer syncPutStream.
//	contentType   — content-type header; fall back to octet-stream if empty
func syncPutToUpstream(ctx context.Context, bucket, key string, payload []byte, contentType string) error {
	mode := writeThroughEnabled()
	if mode == "" {
		return ErrWriteThroughDisabled
	}
	if !withinSizeLimit(int64(len(payload))) {
		return ErrWriteThroughSkipSize
	}
	if mode == "async" {
		fallbackWriteInFlight.Add(1)
		go func() {
			defer fallbackWriteInFlight.Done()
			fallbackWriteSem <- struct{}{}
			defer func() { <-fallbackWriteSem }()
			_ = doUpstreamPut(context.Background(), bucket, key, payload, contentType)
		}()
		return nil
	}
	// mirror mode: caller waits for the result
	return doUpstreamPut(ctx, bucket, key, payload, contentType)
}

// syncPutStreamToUpstream replicates an on-disk file (e.g. the local
// /nfs_export copy or a staged multipart-upload file). Used when the object
// is too large to buffer in RAM. The path must remain valid for the full
// retry window in async mode — typically pass the /nfs_export path, which
// survives until eviction.
func syncPutStreamToUpstream(ctx context.Context, bucket, key, filePath string, size int64, contentType string) error {
	mode := writeThroughEnabled()
	if mode == "" {
		return ErrWriteThroughDisabled
	}
	if !withinSizeLimit(size) {
		return ErrWriteThroughSkipSize
	}
	doFile := func(ctx context.Context) error {
		f, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("open %s: %w", filePath, err)
		}
		defer f.Close()
		return doUpstreamPutReader(ctx, bucket, key, f, size, contentType)
	}
	if mode == "async" {
		fallbackWriteInFlight.Add(1)
		go func() {
			defer fallbackWriteInFlight.Done()
			fallbackWriteSem <- struct{}{}
			defer func() { <-fallbackWriteSem }()
			_ = doFile(context.Background())
		}()
		return nil
	}
	return doFile(ctx)
}

// syncDeleteFromUpstream replicates a successful Züs DELETE. Async by default.
func syncDeleteFromUpstream(ctx context.Context, bucket, key string) error {
	mode := writeThroughEnabled()
	if mode == "" {
		return ErrWriteThroughDisabled
	}
	if mode == "async" {
		fallbackWriteInFlight.Add(1)
		go func() {
			defer fallbackWriteInFlight.Done()
			fallbackWriteSem <- struct{}{}
			defer func() { <-fallbackWriteSem }()
			_ = doUpstreamDelete(context.Background(), bucket, key)
		}()
		return nil
	}
	return doUpstreamDelete(ctx, bucket, key)
}

// doUpstreamPut does the actual PUT with retries + exponential backoff.
func doUpstreamPut(ctx context.Context, bucket, key string, payload []byte, contentType string) error {
	if minioFallbackClient == nil {
		initFallbackS3()
		if minioFallbackClient == nil {
			return fmt.Errorf("fallback_s3: client not initialised")
		}
	}
	upBucket := upstreamBucketFor(bucket)
	// Ensure the bucket exists on upstream (best-effort; ignore "already exists").
	_ = minioFallbackClient.MakeBucket(ctx, upBucket, minio.MakeBucketOptions{Region: serverConfig.FallbackS3Region})

	if contentType == "" {
		contentType = "application/octet-stream"
	}
	opts := minio.PutObjectOptions{ContentType: contentType}
	size := int64(len(payload))

	var lastErr error
	for attempt := 1; attempt <= fallbackWriteMaxAttempts; attempt++ {
		fallbackWriteStats.Attempts.Add(1)
		_, err := minioFallbackClient.PutObject(ctx, upBucket, key, bytes.NewReader(payload), size, opts)
		if err == nil {
			fallbackWriteStats.Successes.Add(1)
			fallbackWriteStats.BytesReplicated.Add(size)
			logger.Info("write-through: %s/%s → upstream/%s/%s %d bytes (attempt %d)",
				bucket, key, upBucket, key, size, attempt)
			return nil
		}
		lastErr = err
		fallbackWriteStats.Retries.Add(1)
		if ctx.Err() != nil {
			break
		}
		// exponential backoff
		time.Sleep(fallbackWriteBaseBackoff * time.Duration(1<<uint(attempt-1)))
	}
	fallbackWriteStats.Failures.Add(1)
	fallbackWriteStats.LastFailure.Store(fmt.Sprintf("%s/%s: %v (%s)", bucket, key, lastErr, time.Now().Format(time.RFC3339)))
	logger.LogIf(ctx, fmt.Errorf("fallback_s3 write-through PUT failed bucket=%s key=%s: %w", bucket, key, lastErr))
	return lastErr
}

// doUpstreamPutReader is the io.Reader equivalent — for large files where we
// don't want to buffer. It doesn't retry (can't re-read a Reader); instead
// the caller should catch the error and re-enqueue with a fresh file open.
func doUpstreamPutReader(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error {
	if minioFallbackClient == nil {
		initFallbackS3()
		if minioFallbackClient == nil {
			return fmt.Errorf("fallback_s3: client not initialised")
		}
	}
	upBucket := upstreamBucketFor(bucket)
	_ = minioFallbackClient.MakeBucket(ctx, upBucket, minio.MakeBucketOptions{Region: serverConfig.FallbackS3Region})

	if contentType == "" {
		contentType = "application/octet-stream"
	}
	opts := minio.PutObjectOptions{ContentType: contentType}

	fallbackWriteStats.Attempts.Add(1)
	_, err := minioFallbackClient.PutObject(ctx, upBucket, key, r, size, opts)
	if err != nil {
		fallbackWriteStats.Failures.Add(1)
		fallbackWriteStats.LastFailure.Store(fmt.Sprintf("%s/%s: %v (%s)", bucket, key, err, time.Now().Format(time.RFC3339)))
		logger.LogIf(ctx, fmt.Errorf("fallback_s3 write-through stream PUT failed bucket=%s key=%s: %w", bucket, key, err))
		return err
	}
	fallbackWriteStats.Successes.Add(1)
	fallbackWriteStats.BytesReplicated.Add(size)
	logger.Info("write-through (stream): %s/%s → upstream/%s/%s %d bytes",
		bucket, key, upBucket, key, size)
	return nil
}

// doUpstreamDelete DELETEs with retries.
func doUpstreamDelete(ctx context.Context, bucket, key string) error {
	if minioFallbackClient == nil {
		initFallbackS3()
		if minioFallbackClient == nil {
			return fmt.Errorf("fallback_s3: client not initialised")
		}
	}
	upBucket := upstreamBucketFor(bucket)
	var lastErr error
	for attempt := 1; attempt <= fallbackWriteMaxAttempts; attempt++ {
		fallbackWriteStats.Attempts.Add(1)
		err := minioFallbackClient.RemoveObject(ctx, upBucket, key, minio.RemoveObjectOptions{})
		if err == nil {
			fallbackWriteStats.Successes.Add(1)
			logger.Info("write-through DELETE: upstream/%s/%s (attempt %d)", upBucket, key, attempt)
			return nil
		}
		lastErr = err
		fallbackWriteStats.Retries.Add(1)
		if ctx.Err() != nil {
			break
		}
		time.Sleep(fallbackWriteBaseBackoff * time.Duration(1<<uint(attempt-1)))
	}
	fallbackWriteStats.Failures.Add(1)
	fallbackWriteStats.LastFailure.Store(fmt.Sprintf("DEL %s/%s: %v (%s)", bucket, key, lastErr, time.Now().Format(time.RFC3339)))
	logger.LogIf(ctx, fmt.Errorf("fallback_s3 write-through DELETE failed bucket=%s key=%s: %w", bucket, key, lastErr))
	return lastErr
}

// WriteThroughStatsSnapshot returns counters for monitoring — wire into the
// existing /internal/cache_stats endpoint (see cache_stats_router.go).
func WriteThroughStatsSnapshot() map[string]any {
	lastFail, _ := fallbackWriteStats.LastFailure.Load().(string)
	return map[string]any{
		"mode":             serverConfig.FallbackS3WriteThrough,
		"attempts":         fallbackWriteStats.Attempts.Load(),
		"successes":        fallbackWriteStats.Successes.Load(),
		"failures":         fallbackWriteStats.Failures.Load(),
		"retries":          fallbackWriteStats.Retries.Load(),
		"bytes_replicated": fallbackWriteStats.BytesReplicated.Load(),
		"last_failure":     lastFail,
		"in_flight_window": fallbackWriteMaxConcur,
	}
}

// WaitForWriteThroughDrain blocks until all in-flight async replications
// finish. Intended for shutdown and testing; do not call on the hot path.
func WaitForWriteThroughDrain(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		fallbackWriteInFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
