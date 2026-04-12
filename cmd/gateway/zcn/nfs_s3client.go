package zcn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/internal/hash"
)

// nfsObjectAPI provides direct in-process access to MinIO's cache ObjectLayer.
// This bypasses HTTP entirely — NFS writes go straight to /mcache disk.
//
// Performance: eliminates ~5ms HTTP loopback overhead per operation.
// ACID: same guarantees as S3 — data lands on /mcache (NVMe), writeback worker
// commits to blobbers asynchronously.
type nfsObjectAPI struct {
	// Lazy: the cache layer is initialized after our gateway layer,
	// so we fetch it on first use.
	once      sync.Once
	cacheAPI  minio.CacheObjectLayer
	objectAPI minio.ObjectLayer
}

var nfsObjAPI nfsObjectAPI

func (api *nfsObjectAPI) init() {
	api.once.Do(func() {
		api.cacheAPI = minio.GetGlobalCacheObjectAPI()
		api.objectAPI = minio.GetGlobalObjectAPI()
		if api.cacheAPI != nil {
			log.Printf("[NFS] Using in-process cache ObjectLayer (zero HTTP overhead)")
		} else if api.objectAPI != nil {
			log.Printf("[NFS] Using in-process ObjectLayer (no cache layer)")
		}
	})
}

// baseAPI returns the base ObjectLayer (for bucket ops, list, etc.).
func (api *nfsObjectAPI) baseAPI() minio.ObjectLayer {
	api.init()
	return api.objectAPI
}

func (api *nfsObjectAPI) getCacheAPI() minio.CacheObjectLayer {
	api.init()
	return api.cacheAPI
}

// put writes an object directly to the cache layer (no HTTP).
func (api *nfsObjectAPI) put(ctx context.Context, bucket, key string, data []byte) error {
	api.init()

	size := int64(len(data))
	reader := bytes.NewReader(data)
	hashReader, err := hash.NewReader(reader, size, "", "", size)
	if err != nil {
		return fmt.Errorf("nfs: hash reader: %w", err)
	}
	putReader := minio.NewPutObjReader(hashReader)
	opts := minio.ObjectOptions{UserDefined: map[string]string{"Content-Type": "application/octet-stream"}}

	if api.cacheAPI != nil {
		_, err = api.cacheAPI.PutObject(ctx, bucket, key, putReader, opts)
		return err
	}
	if api.objectAPI != nil {
		_, err = api.objectAPI.PutObject(ctx, bucket, key, putReader, opts)
		return err
	}
	return fmt.Errorf("nfs: object layer not ready")
}

// get reads an object directly from the cache layer (no HTTP).
func (api *nfsObjectAPI) get(ctx context.Context, bucket, key string) (io.ReadCloser, int64, error) {
	api.init()
	opts := minio.ObjectOptions{}

	if api.cacheAPI != nil {
		gr, err := api.cacheAPI.GetObjectNInfo(ctx, bucket, key, nil, http.Header{}, 0, opts)
		if err != nil {
			return nil, 0, err
		}
		return gr, gr.ObjInfo.Size, nil
	}
	if api.objectAPI != nil {
		gr, err := api.objectAPI.GetObjectNInfo(ctx, bucket, key, nil, http.Header{}, 0, opts)
		if err != nil {
			return nil, 0, err
		}
		return gr, gr.ObjInfo.Size, nil
	}
	return nil, 0, fmt.Errorf("nfs: object layer not ready")
}

// head checks if an object exists and returns its size.
func (api *nfsObjectAPI) head(ctx context.Context, bucket, key string) (int64, bool) {
	api.init()
	opts := minio.ObjectOptions{}

	if api.cacheAPI != nil {
		info, err := api.cacheAPI.GetObjectInfo(ctx, bucket, key, opts)
		if err != nil {
			return 0, false
		}
		return info.Size, true
	}
	if api.objectAPI != nil {
		info, err := api.objectAPI.GetObjectInfo(ctx, bucket, key, opts)
		if err != nil {
			return 0, false
		}
		return info.Size, true
	}
	return 0, false
}

// remove deletes an object.
func (api *nfsObjectAPI) remove(ctx context.Context, bucket, key string) error {
	api.init()
	opts := minio.ObjectOptions{}
	if api.cacheAPI != nil {
		_, err := api.cacheAPI.DeleteObject(ctx, bucket, key, opts)
		return err
	}
	if api.objectAPI != nil {
		_, err := api.objectAPI.DeleteObject(ctx, bucket, key, opts)
		return err
	}
	return fmt.Errorf("nfs: object layer not ready")
}

// listDir lists objects in a bucket with prefix.
func (api *nfsObjectAPI) listDir(ctx context.Context, bucket, prefix string) ([]minio.ObjectInfo, error) {
	api.init()
	var result minio.ListObjectsInfo
	var err error

	if api.cacheAPI != nil {
		result, err = api.cacheAPI.ListObjects(ctx, bucket, prefix, "", "/", 10000)
	} else if api.objectAPI != nil {
		result, err = api.objectAPI.ListObjects(ctx, bucket, prefix, "", "/", 10000)
	} else {
		return nil, fmt.Errorf("nfs: object layer not ready")
	}
	if err != nil {
		return nil, err
	}

	var objects []minio.ObjectInfo
	for _, obj := range result.Objects {
		objects = append(objects, obj)
	}
	// Add prefixes as directory entries
	for _, p := range result.Prefixes {
		objects = append(objects, minio.ObjectInfo{
			Bucket: bucket,
			Name:   p,
			IsDir:  true,
		})
	}
	return objects, nil
}

// listBuckets lists all buckets.
func (api *nfsObjectAPI) listBuckets(ctx context.Context) ([]minio.BucketInfo, error) {
	base := api.baseAPI()
	if base == nil {
		return nil, fmt.Errorf("nfs: object layer not ready")
	}
	return base.ListBuckets(ctx)
}

// makeBucket creates a bucket.
func (api *nfsObjectAPI) makeBucket(ctx context.Context, bucket string) error {
	base := api.baseAPI()
	if base == nil {
		return fmt.Errorf("nfs: object layer not ready")
	}
	return base.MakeBucketWithLocation(ctx, bucket, minio.BucketOptions{})
}

// bucketExists checks if a bucket exists.
func (api *nfsObjectAPI) bucketExists(ctx context.Context, bucket string) bool {
	base := api.baseAPI()
	if base == nil {
		return false
	}
	_, err := base.GetBucketInfo(ctx, bucket)
	return err == nil
}

// ready returns true if the object layer is initialized.
func (api *nfsObjectAPI) ready() bool {
	api.init()
	return api.objectAPI != nil || api.cacheAPI != nil
}

// Helper: create timeout context
func nfsCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
