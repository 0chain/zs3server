package zcn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/0chain/gosdk/zboxcore/sdk"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio/internal/logger"
	"golang.org/x/sync/singleflight"
)

// Sentinels returned by tryFallbackFetch.
var (
	ErrFallbackDisabled = errors.New("fallback_s3: disabled")
	ErrFallbackNotFound = errors.New("fallback_s3: not found upstream")
)

var (
	minioFallbackClient *minio.Client
	fallbackInitOnce    sync.Once
	fallbackGroup       singleflight.Group
	// in-flight cache-back dedup, so we don't kick off two uploads for same key
	fallbackCacheInFlight sync.Map
)

// initFallbackS3 builds the package-level minio client from serverConfig. Safe to call multiple times.
func initFallbackS3() {
	if !serverConfig.FallbackS3Enabled {
		return
	}
	fallbackInitOnce.Do(func() {
		endpoint := serverConfig.FallbackS3Endpoint
		// strip scheme if user included one
		host := endpoint
		for _, p := range []string{"https://", "http://"} {
			if len(host) >= len(p) && host[:len(p)] == p {
				host = host[len(p):]
				break
			}
		}
		cli, err := minio.New(host, &minio.Options{
			Creds:  credentials.NewStaticV4(serverConfig.FallbackS3AccessKey, serverConfig.FallbackS3SecretKey, ""),
			Secure: serverConfig.FallbackS3UseSSL,
			Region: serverConfig.FallbackS3Region,
		})
		if err != nil {
			logger.LogIf(context.Background(), fmt.Errorf("fallback_s3: init failed: %w", err))
			return
		}
		minioFallbackClient = cli
		logger.Info("fallback_s3: initialized endpoint=%s region=%s ssl=%v",
			serverConfig.FallbackS3Endpoint, serverConfig.FallbackS3Region, serverConfig.FallbackS3UseSSL)
	})
}

// upstreamBucketFor returns the upstream bucket name given a local bucket.
func upstreamBucketFor(localBucket string) string {
	if m := serverConfig.FallbackBucketMap; m != nil {
		if v, ok := m[localBucket]; ok && v != "" {
			return v
		}
	}
	return localBucket
}

// tryFallbackFetch attempts to fetch bucket/key from configured upstream S3.
// Returns ErrFallbackDisabled if the feature is off, ErrFallbackNotFound on upstream 404.
// On success the returned ReadCloser is teed into an async cache-back upload to Zus.
func tryFallbackFetch(ctx context.Context, alloc *sdk.Allocation, bucket, key string) (io.ReadCloser, *minio.ObjectInfo, error) {
	if !serverConfig.FallbackS3Enabled {
		return nil, nil, ErrFallbackDisabled
	}
	if minioFallbackClient == nil {
		initFallbackS3()
		if minioFallbackClient == nil {
			return nil, nil, ErrFallbackDisabled
		}
	}

	upBucket := upstreamBucketFor(bucket)
	obj, err := minioFallbackClient.GetObject(ctx, upBucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, nil, err
	}
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		errResp := minio.ToErrorResponse(err)
		if errResp.StatusCode == 404 || errResp.Code == "NoSuchKey" || errResp.Code == "NoSuchBucket" {
			return nil, nil, ErrFallbackNotFound
		}
		return nil, nil, err
	}

	start := time.Now()

	// Tee bytes: one half streams to the client (pr), the other is uploaded back to Zus.
	pr, pw := io.Pipe()
	tee := io.TeeReader(obj, pw)

	// Wrapper so that closing 'obj' also closes the pipe writer so the goroutine sees EOF.
	clientReader := &teeReadCloser{r: tee, src: obj, pw: pw}

	// Dedup cache-back per key so concurrent clients don't start multiple uploads.
	cacheKey := bucket + "/" + key
	if _, loaded := fallbackCacheInFlight.LoadOrStore(cacheKey, struct{}{}); loaded {
		// Someone else is already uploading; just discard our pipe writer so no deadlock.
		// Drain the write side to /dev/null goroutine.
		go func() {
			defer pw.Close()
			_, _ = io.Copy(io.Discard, pr)
		}()
	} else {
		// Cache-back to Zus in background.
		go func() {
			defer fallbackCacheInFlight.Delete(cacheKey)
			defer pr.Close()
			var remotePath string
			if bucket == rootBucketName {
				remotePath = filepath.Join(rootPath, key)
			} else {
				remotePath = filepath.Join(rootPath, bucket, key)
			}
			ctype := info.ContentType
			if ctype == "" {
				ctype = "application/octet-stream"
			}
			upErr := putFile(context.Background(), alloc, remotePath, ctype, pr, info.Size, false, nil)
			if upErr != nil {
				logger.LogIf(context.Background(), fmt.Errorf("fallback_s3: cache-back failed bucket=%s key=%s: %w", bucket, key, upErr))
				return
			}
			logger.Info("FALLBACK: fetched %s/%s from upstream (%d bytes, %s), cached back to Zus",
				bucket, key, info.Size, time.Since(start))
		}()
	}

	return clientReader, &info, nil
}

// teeReadCloser closes both the upstream object and the pipe writer so the
// cache-back goroutine sees EOF once the client is done reading.
type teeReadCloser struct {
	r   io.Reader
	src io.Closer
	pw  *io.PipeWriter
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err == io.EOF {
		_ = t.pw.Close()
	} else if err != nil {
		_ = t.pw.CloseWithError(err)
	}
	return n, err
}
func (t *teeReadCloser) Close() error {
	_ = t.pw.Close()
	return t.src.Close()
}

// fallbackStat issues a lightweight HEAD (StatObject) to the upstream so
// S3 clients that do a HeadObject before GetObject (mc cp, aws s3 cp)
// get a 200 with object metadata instead of a Züs 404. No bytes streamed;
// callers should let the subsequent GET trigger tryFallbackFetch which
// does the tee-to-cache-back.
func fallbackStat(ctx context.Context, bucket, key string) (*minio.ObjectInfo, error) {
	if !serverConfig.FallbackS3Enabled {
		return nil, ErrFallbackDisabled
	}
	if minioFallbackClient == nil {
		initFallbackS3()
		if minioFallbackClient == nil {
			return nil, ErrFallbackDisabled
		}
	}
	upBucket := upstreamBucketFor(bucket)
	info, err := minioFallbackClient.StatObject(ctx, upBucket, key, minio.StatObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.StatusCode == 404 || errResp.Code == "NoSuchKey" || errResp.Code == "NoSuchBucket" {
			return nil, ErrFallbackNotFound
		}
		return nil, err
	}
	return &info, nil
}

// fallbackFetchSingleflight wraps tryFallbackFetch with singleflight-keyed dedup
// so two concurrent GETs for the same missing object share one upstream fetch.
// NOTE: only the winner receives the streaming reader; followers get ErrFallbackDisabled
// so the caller treats them as cache miss and retries Zus (which, by then, is likely
// populated). This keeps the semantics simple and avoids duplicating the byte stream.
func fallbackFetchSingleflight(ctx context.Context, alloc *sdk.Allocation, bucket, key string) (io.ReadCloser, *minio.ObjectInfo, error) {
	type result struct {
		rc   io.ReadCloser
		info *minio.ObjectInfo
		err  error
	}
	k := bucket + "|" + key
	v, _, _ := fallbackGroup.Do(k, func() (interface{}, error) {
		rc, info, err := tryFallbackFetch(ctx, alloc, bucket, key)
		return &result{rc, info, err}, nil
	})
	r := v.(*result)
	return r.rc, r.info, r.err
}
