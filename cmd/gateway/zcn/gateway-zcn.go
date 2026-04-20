package zcn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/0chain/gosdk/constants"
	"github.com/minio/minio/internal/logger"
	"github.com/minio/pkg/mimedb"
	"github.com/mitchellh/go-homedir"

	"github.com/0chain/gosdk/zboxcore/sdk"
	"github.com/minio/cli"
	"github.com/minio/madmin-go"
	minio "github.com/minio/minio/cmd"
)

const (
	rootPath               = "/"
	rootBucketName         = "root"
	s3DirectoryContentType = "application/x-directory; charset=UTF-8"
	s3ContentHash          = "d41d8cd98f00b204e9800998ecf8427e"
	// largeObjectCacheSkipBytes: objects at or above this size bypass the
	// /nfs_export tmpfs write-then-rename path and stream directly to the
	// blobbers. Prevents ENOSPC on a modest tmpfs when concurrent multi-GiB
	// PUTs would need 2× their size in tmpfs during the write-then-rename.
	largeObjectCacheSkipBytes = int64(1) << 30
)

var (
	configDir    string
	allocationID string
	nonce        int64
	encrypt      bool
	compress     bool
	workDir      string
	serverConfig serverOptions
	walWriter    *WALWriter // WAL intent log for writeback cache crash recovery
)

// Per-tier hit counters. Incremented in Fix A (fast local serve) and on
// the TryCacheRead full-file path. blobberReadCount bumps whenever we
// fall through to getFileReader (i.e. neither tier served the request).
// Exposed via GET /internal/cache_stats.
var (
	tmpfsHitCount     int64
	spilloverHitCount int64
	blobberReadCount  int64
	// NFS-path counters (bumped from prewarmHandler — FSAL_ZUS's prewarm
	// call is the single funnel for all NFS read activity through
	// zs3server, so counting here captures the full NFS data path).
	nfsTmpfsHitCount     int64
	nfsSpilloverHitCount int64
	nfsPrewarmFetchCount int64 // blobber fetches originating from NFS prewarm
)

var zFlags = []cli.Flag{
	cli.StringFlag{
		Name:        "configDir",
		Usage:       "Config directory containing config.yaml, wallet.json, allocation.txt, etc.",
		Destination: &configDir,
	},
	cli.StringFlag{
		Name:        "allocationId",
		Usage:       "Allocation id of an allocation",
		Destination: &allocationID,
	},
	cli.Int64Flag{
		Name:        "nonce",
		Usage:       "nonce to use in transaction",
		Destination: &nonce,
	},
}

func init() {
	const zcnGateWayTemplate = `NAME:
	{{.HelpName}} - {{.Usage}}

  USAGE:
	{{.HelpName}} {{if .VisibleFlags}}[FLAGS]{{end}} ZCN-NAMENODE [ZCN-NAMENODE...]
  {{if .VisibleFlags}}
  FLAGS:
	{{range .VisibleFlags}}{{.}}
	{{end}}{{end}}
  ZCN-NAMENODE:
	ZCN namenode URI

  EXAMPLES:
	1. Start minio gateway server for ZeroChain backend
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_ROOT_USER{{.AssignmentOperator}}accesskey
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_ROOT_PASSWORD{{.AssignmentOperator}}secretkey
	   {{.Prompt}} {{.HelpName}} zcn://namenode:8200

	2. Start minio gateway server for ZCN with edge caching enabled
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_ROOT_USER{{.AssignmentOperator}}accesskey
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_ROOT_PASSWORD{{.AssignmentOperator}}secretkey
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_DRIVES{{.AssignmentOperator}}"/mnt/drive1,/mnt/drive2,/mnt/drive3,/mnt/drive4"
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_EXCLUDE{{.AssignmentOperator}}"bucket1/*,*.png"
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_QUOTA{{.AssignmentOperator}}90
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_AFTER{{.AssignmentOperator}}3
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_WATERMARK_LOW{{.AssignmentOperator}}75
	   {{.Prompt}} {{.EnvVarSetCommand}} MINIO_CACHE_WATERMARK_HIGH{{.AssignmentOperator}}85
	   {{.Prompt}} {{.HelpName}} hdfs://namenode:8200
  `

	minio.RegisterGatewayCommand(cli.Command{
		Name:               minio.ZCNBAckendGateway,
		Usage:              "0chain dStorage",
		Action:             zcnGatewayMain,
		CustomHelpTemplate: zcnGateWayTemplate,
		Flags:              zFlags,
		HideHelpCommand:    true,
	})
}

func zcnGatewayMain(ctx *cli.Context) {
	if ctx.Args().First() == "help" {
		cli.ShowCommandHelpAndExit(ctx, minio.ZCNBAckendGateway, 1)
	}

	minio.StartGateway(ctx, &ZCN{args: ctx.Args()})
}

// ZCN implements gateway
type ZCN struct {
	args []string
}

// Name implements gateway interface
func (z *ZCN) Name() string {
	return minio.ZCNBAckendGateway
}

var (
	contentLock sync.Mutex
)

// currentAlloc/currentBS are set by NewGatewayLayer for use by other in-package
// HTTP handlers (e.g. prewarm).
var (
	currentAlloc *sdk.Allocation
	currentBS    *BlobberSync
)

// NewGatewayLayer initializes 0chain gosdk and return zcnObjects
func (z *ZCN) NewGatewayLayer(creds madmin.Credentials) (minio.ObjectLayer, error) {
	err := initializeSDK(configDir, allocationID, nonce)
	if err != nil {
		return nil, err
	}
	log.Println("0chain gosdk initialized: ", allocationID, "compress: ", compress, "encrypt: ", encrypt)
	if serverConfig.UploadWorkers > 0 {
		sdk.SetHighModeWorkers(serverConfig.UploadWorkers)
	}
	if serverConfig.DownloadWorkers > 0 {
		sdk.SetDownloadWorkerCount(serverConfig.DownloadWorkers)
	}
	allocation, err := sdk.GetAllocation(allocationID)
	if err != nil {
		return nil, err
	}
	sdk.CurrentMode = sdk.UploadModeHigh
	sdk.SetSingleClietnMode(true)
	sdk.SetShouldVerifyHash(false)
	sdk.SetSaveProgress(false)
	currentAlloc = allocation
	zob := &zcnObjects{
		alloc:   allocation,
		metrics: minio.NewMetrics(),
	}
	debug.SetGCPercent(50)
	workDir, err = homedir.Dir()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	zob.ctxCancel = cancel
	IntiBatchUploadWorkers(ctx, allocation, serverConfig.BatchWaitTime, serverConfig.MaxBatchSize, serverConfig.BatchWorkers)
	if serverConfig.SDKBatchSize > 0 {
		sdk.BatchSize = serverConfig.SDKBatchSize
	} else {
		sdk.BatchSize = serverConfig.MaxConcurrentRequests
	}
	if serverConfig.LockedBlobbersCap > 0 {
		sdk.LockedBlobbersCap = serverConfig.LockedBlobbersCap
	}
	sdk.SetMultiOpBatchSize(serverConfig.MaxBatchSize)

	// Initialize WAL intent log for writeback cache crash recovery
	if serverConfig.EnableWAL {
		walDir := serverConfig.WALDir
		if walDir == "" {
			walDir = filepath.Join(workDir, ".zcn", "wal")
		}
		walWorkers := serverConfig.WALCommitWorkers
		if walWorkers == 0 {
			walWorkers = 5
		}
		var walErr error
		walWriter, walErr = NewWALWriter(walDir, allocation, walWorkers)
		if walErr != nil {
			log.Printf("WAL init failed (writeback cache still works, no crash recovery): %v", walErr)
		} else {
			log.Printf("WAL intent log enabled at %s", walDir)
		}
	}

	// Start adaptive config loop — periodically tunes batch/worker settings
	// based on median file size observed in recent operations.
	StartAdaptiveLoop()

	// Sequential prefetch predictor: watches GET patterns per bucket+dir
	// and pre-spawns cacheBackFullFetch for the next N keys when a sorted
	// run is detected. Single biggest win on training loops.
	InitPrefetchPredictor(allocation, cacheBackFullFetch, func() string {
		switch {
		case serverConfig.NFSTmpfsCacheEnabled && serverConfig.NFSGaneshaExportDir != "":
			return serverConfig.NFSGaneshaExportDir
		case serverConfig.NFSSpilloverCacheEnabled && serverConfig.NFSSpilloverDir != "":
			return serverConfig.NFSSpilloverDir
		}
		return ""
	})

	// NFS-Ganesha mode: sync export directory to blobbers via inotify.
	// NFS-Ganesha runs externally (apt install nfs-ganesha nfs-ganesha-vfs).
	// This watcher makes it ACID by committing changes to blobbers async.
	//
	// Gated by serverConfig.NFSSyncEnabled (default true). When false, the
	// inotify watcher, processEvents, and batchCommit/batchDelete workers
	// never start — useful for pure-S3 benchmarks where /nfs_export is not
	// written to via NFS and the per-write stat+Getxattr in cacheBackTee
	// is pure overhead.
	if serverConfig.NFSGaneshaExportDir != "" {
		if !serverConfig.NFSSyncEnabled {
			log.Printf("[NFS-Ganesha] NFS-Sync watcher disabled via config (nfs_sync_enabled=false); processEvents/batchCommit/batchDelete goroutines not started")
		} else {
			workers := serverConfig.NFSSyncWorkers
			if workers == 0 {
				workers = 8
			}
			evict := true
			if !serverConfig.NFSCacheEvict {
				evict = serverConfig.NFSCacheEvict
			}
			directThreshold := serverConfig.NFSDirectThreshold
			if directThreshold == 0 {
				directThreshold = 2 * 1024 * 1024 // default 2MB
			}
			if directThreshold < 0 {
				directThreshold = 0 // negative = disabled
			}
			log.Printf("[NFS-Ganesha] cache_mode=%s, direct_threshold=%dMB, evict=%v, spillover=%s",
				serverConfig.NFSCacheMode, directThreshold/(1024*1024), evict, serverConfig.NFSSpilloverDir)
			if bs, err := StartBlobberSync(serverConfig.NFSGaneshaExportDir, allocation, workers, serverConfig.NFSSpilloverDir, evict, directThreshold); err != nil {
				log.Printf("[NFS-Ganesha] Failed to start blobber sync: %v", err)
			} else {
				currentBS = bs
			}
		}
	}

	// Start go-nfs gateway (fallback, NFSv3 on port 2049)
	if serverConfig.EnableNFS && serverConfig.NFSGaneshaExportDir == "" {
		nfsPort := serverConfig.NFSPort
		if nfsPort == 0 {
			nfsPort = 2049
		}
		if err := StartNFSServer(nfsPort, allocation, serverConfig.NFSCacheDir, serverConfig.NFSCacheMode); err != nil {
			log.Printf("[NFS] Failed to start NFS server: %v", err)
		}
	}

	return zob, nil
}

type zcnObjects struct {
	minio.GatewayUnsupported
	alloc     *sdk.Allocation
	metrics   *minio.BackendMetrics
	ctxCancel context.CancelFunc
}

// Shutdown Remove temporary directory
func (zob *zcnObjects) Shutdown(ctx context.Context) error {
	os.RemoveAll(tempdir)
	zob.ctxCancel()
	return nil
}

func (zob *zcnObjects) IsNotificationSupported() bool {
	return true
}

func (zob *zcnObjects) Production() bool {
	return true
}

func (zob *zcnObjects) GetMetrics(ctx context.Context) (*minio.BackendMetrics, error) {
	return zob.metrics, nil
}

// DeleteBucket Delete only empty bucket unless forced
func (zob *zcnObjects) DeleteBucket(ctx context.Context, bucketName string, opts minio.DeleteBucketOptions) error {
	if bucketName == rootBucketName {
		return errors.New("cannot remove root path")
	}

	remotePath := filepath.Join(rootPath, bucketName)

	ref, err := getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		return err
	}

	if ref.Type != dirType {
		return fmt.Errorf("%v is object not bucket", bucketName)
	}

	if opts.Force || ref.Size == 0 {
		op := sdk.OperationRequest{
			OperationType: constants.FileOperationDelete,
			RemotePath:    remotePath,
		}
		if err := zob.alloc.DoMultiOperation([]sdk.OperationRequest{op}); err != nil {
			return err
		}
		mirrorS3DeleteBucketToExport(bucketName)
		return nil
	}
	return minio.BucketNotEmpty{Bucket: bucketName}
}

func (zob *zcnObjects) DeleteObject(ctx context.Context, bucket, object string, opts minio.ObjectOptions) (oInfo minio.ObjectInfo, err error) {
	// Mark WAL intent as deleted (prevents crash recovery re-commit)
	if walWriter != nil {
		walWriter.Delete(bucket, object)
	}

	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		// If not on blobber, that's OK — it was in WAL and we deleted it
		if isPathNoExistError(err) && walWriter != nil {
			return minio.ObjectInfo{Bucket: bucket, Name: object, ModTime: time.Now()}, nil
		}
		return
	}

	op := sdk.OperationRequest{
		OperationType: constants.FileOperationDelete,
		RemotePath:    remotePath,
	}

	err = zob.alloc.DoMultiOperation([]sdk.OperationRequest{op})
	if err != nil {
		return
	}
	mirrorS3DeleteObjectToExport(bucket, object)
	// Replicate DELETE to upstream S3 (if write-through enabled). Errors in
	// async mode are logged and swallowed; mirror mode surfaces them.
	if writeThroughEnabled() != "" {
		if wtErr := syncDeleteFromUpstream(ctx, bucket, object); wtErr != nil && writeThroughEnabled() == "mirror" {
			err = wtErr
			return
		}
	}
	return minio.ObjectInfo{
		Bucket:  bucket,
		Name:    ref.Name,
		ModTime: time.Now(),
		Size:    ref.ActualFileSize,
		IsDir:   ref.Type == dirType,
	}, nil
}

func (zob *zcnObjects) DeleteObjects(ctx context.Context, bucket string, objects []minio.ObjectToDelete, opts minio.ObjectOptions) (delObs []minio.DeletedObject, errs []error) {
	var basePath string
	if bucket == rootBucketName {
		basePath = rootPath
	} else {
		basePath = filepath.Join(rootPath, bucket)
	}
	ops := make([]sdk.OperationRequest, 0, len(objects))
	for _, object := range objects {
		remotePath := filepath.Join(basePath, object.ObjectName)
		ops = append(ops, sdk.OperationRequest{
			OperationType: constants.FileOperationDelete,
			RemotePath:    remotePath,
		})
		delObs = append(delObs, minio.DeletedObject{})
		errs = append(errs, nil)
	}
	err := zob.alloc.DoMultiOperation(ops)
	if err != nil {
		for i := 0; i < len(errs); i++ {
			errs[i] = err
		}
	} else {
		for i := 0; i < len(delObs); i++ {
			delObs[i].ObjectName = objects[i].ObjectName
			mirrorS3DeleteObjectToExport(bucket, objects[i].ObjectName)
		}
	}
	log.Println("DeletedObjects", len(delObs), len(errs))
	return
}

// GetBucketInfo Get directory's metadata and present it as minio.BucketInfo
func (zob *zcnObjects) GetBucketInfo(ctx context.Context, bucket string) (bi minio.BucketInfo, err error) {
	var remotePath string
	if bucket == rootBucketName {
		remotePath = rootPath
	} else {
		remotePath = filepath.Join(rootPath, bucket)
	}

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		if isPathNoExistError(err) {
			if remotePath == rootPath {
				return minio.BucketInfo{Name: rootBucketName}, nil
			}
			return bi, minio.BucketNotFound{Bucket: bucket}
		}
		return
	}

	if ref.Type != dirType {
		return bi, minio.BucketNotFound{Bucket: bucket}
	}

	return minio.BucketInfo{Name: ref.Name, Created: ref.CreatedAt.ToTime()}, nil
}

// GetObjectInfo Get file meta data and respond it as minio.ObjectInfo
func (zob *zcnObjects) GetObjectInfo(ctx context.Context, bucket, object string, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	// WAL tombstone gate (same as GetObjectNInfo) so HEAD calls also honour
	// the recent-delete grace window.
	if walWriter != nil && walWriter.WasRecentlyDeleted(bucket, object) {
		return minio.ObjectInfo{}, minio.ObjectNotFound{Bucket: bucket, Object: object}
	}

	// MinIO writeback cache serves metadata for cached objects.
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, filepath.Clean(remotePath))
	if err != nil {
		// S3-upstream fallback HEAD: mc cp / aws s3 cp call HeadObject
		// before GetObject; without this, clients give up before our
		// GET-path fallback can fire. Treat both "path does not exist"
		// and "no consensus" as "not on Züs; ask upstream".
		if serverConfig.FallbackS3Enabled && (isPathNoExistError(err) || isConsensusFailedError(err)) {
			if info, fbErr := fallbackStat(ctx, bucket, object); fbErr == nil && info != nil {
				return minio.ObjectInfo{
					Bucket:      bucket,
					Name:        object,
					ModTime:     info.LastModified,
					Size:        info.Size,
					ETag:        info.ETag,
					ContentType: info.ContentType,
				}, nil
			}
		}
		if isPathNoExistError(err) {
			return objInfo, minio.ObjectNotFound{Bucket: bucket, Object: object}
		}
		return
	}

	if ref.Type == dirType && object != "" && object[len(object)-1] != '/' {
		return minio.ObjectInfo{}, minio.ObjectNotFound{Bucket: bucket, Object: object}
	}
	if ref.Type == dirType {
		ref.MimeType = s3DirectoryContentType
		ref.ActualFileHash = s3ContentHash
		ref.ActualFileSize = 0
	}

	var userDefined map[string]string
	if ref.CustomMeta != "" {
		_ = json.Unmarshal([]byte(ref.CustomMeta), &userDefined)
	}

	return minio.ObjectInfo{
		Bucket:      bucket,
		Name:        getRelativePathOfObj(ref.Path, bucket),
		ModTime:     ref.UpdatedAt.ToTime(),
		Size:        ref.ActualFileSize,
		IsDir:       ref.Type == dirType,
		AccTime:     time.Now(),
		ContentType: ref.MimeType,
		ETag:        ref.ActualFileHash,
		UserDefined: userDefined,
	}, nil
}

// GetObjectNInfo Provides reader with read cursor placed at offset upto some length
func (zob *zcnObjects) GetObjectNInfo(ctx context.Context, bucket, object string, rs *minio.HTTPRangeSpec, h http.Header, lockType minio.LockType, opts minio.ObjectOptions) (gr *minio.GetObjectReader, err error) {
	// WAL tombstone gate: a recently-DELETEd key must not serve content
	// even if a lagging blobber still has it. Restores S3 read-after-delete
	// linearizability while the gosdk DELETE multi-blobber propagation is
	// in flight. See wal.go:WasRecentlyDeleted.
	if walWriter != nil && walWriter.WasRecentlyDeleted(bucket, object) {
		return nil, minio.ObjectNotFound{Bucket: bucket, Object: object}
	}

	// Fire the sequential-access predictor FIRST, before any early return
	// from Fix A / Fix B / TryCacheRead. This way cache-hit patterns also
	// trigger prefetch for not-yet-cached future keys in the same run.
	RecordGet(bucket, object)

	// MinIO writeback cache serves data via optimized sendfile path.
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	var rangeStart int64 = 1
	var rangeEnd int64 = 0
	if rs != nil {
		if rs.IsSuffixLength {
			rangeStart = -rs.Start
			// take absolute value of difference between start and end
			rangeEnd = rangeStart
			if rs.End-rs.Start > 0 {
				rangeEnd += rs.End - rs.Start
			} else {
				rangeEnd += rs.Start - rs.End
			}
		} else {
			rangeStart = rs.Start
			rangeEnd = rs.End
		}
	}

	// Check local caches (NFS export, MinIO writeback) before blobber download.
	// Only for full-file reads: cache_router's suffix-range conversion and
	// ObjectInfo.Size truncation break S3A parquet range reads (EOFException).
	// Range reads fall through to gosdk which handles ranges correctly.
	if rs == nil && !serverConfig.NFSCacheDisabled {
		if cached := TryCacheRead(bucket, object, rangeStart, rangeEnd); cached != nil {
			switch cached.Source {
			case "nfs_export":
				atomic.AddInt64(&tmpfsHitCount, 1)
				cacheHitRate.RecordHit(true)
			case "spillover":
				atomic.AddInt64(&spilloverHitCount, 1)
				cacheHitRate.RecordHit(true)
			}
			closer := cached.Reader.Close
			cleanup := func() { _ = closer() }
			gr, err = minio.NewGetObjectReaderFromReader(cached.Reader, *cached.ObjectInfo, opts, cleanup)
			return
		}
	}

	// Fix A: Local-file fast path. If the file exists on /nfs_export (tmpfs
	// tier) and is marked committed (user.zus.committed xattr) with non-zero
	// on-disk blocks, serve the range directly. Bypasses gosdk consensus,
	// erasure decode, and blobber round trips. Applies to BOTH full-file and
	// range reads (TryCacheRead above only handles rs==nil).
	if serverConfig.NFSTmpfsCacheEnabled && serverConfig.NFSGaneshaExportDir != "" && !serverConfig.NFSCacheDisabled {
		localPath := filepath.Join(serverConfig.NFSGaneshaExportDir, bucket, object)
		if fi, statErr := os.Stat(localPath); statErr == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			var xbuf [4]byte
			if n, _ := syscall.Getxattr(localPath, "user.zus.committed", xbuf[:]); n > 0 {
				var st syscall.Stat_t
				if syscall.Lstat(localPath, &st) == nil && st.Blocks > 0 {
					lf, oerr := os.Open(localPath)
					if oerr == nil {
						totalSize := fi.Size()
						var start, length int64
						if rs == nil {
							start, length = 0, totalSize
						} else if rs.IsSuffixLength {
							// bytes=-N -> last N bytes; rs.Start is negative.
							start = totalSize + rs.Start
							if start < 0 {
								start = 0
							}
							length = totalSize - start
						} else {
							start = rs.Start
							if rs.End < 0 {
								length = totalSize - start
							} else {
								length = rs.End - rs.Start + 1
							}
						}
						if start >= 0 && start < totalSize && length > 0 {
							if _, serr := lf.Seek(start, io.SeekStart); serr == nil {
								contentType := mime.TypeByExtension(filepath.Ext(object))
								if contentType == "" {
									contentType = "application/octet-stream"
								}
								objInfo := minio.ObjectInfo{
									Bucket:      bucket,
									Name:        object,
									Size:        totalSize,
									ModTime:     fi.ModTime(),
									ContentType: contentType,
									ETag:        fmt.Sprintf("%x-%d", fi.ModTime().UnixNano(), totalSize),
								}
								reader := &limitedReadCloser{
									R: io.LimitReader(lf, length),
									C: lf,
								}
								atomic.AddInt64(&tmpfsHitCount, 1)
								cacheHitRate.RecordHit(true)
								gr, err = minio.NewGetObjectReaderFromReader(reader, objInfo, opts, func() { _ = lf.Close() })
								return
							}
						}
						_ = lf.Close()
					}
				}
			}
		}
	}

	// Fix A (spillover tier): mirror of tmpfs block above, checking
	// NFSSpilloverDir. Lets us isolate "spillover only" from "tmpfs only"
	// for benchmarking. Same xattr + sparse (Blocks>0) gate.
	if serverConfig.NFSSpilloverCacheEnabled && serverConfig.NFSSpilloverDir != "" && !serverConfig.NFSCacheDisabled {
		localPath := filepath.Join(serverConfig.NFSSpilloverDir, bucket, object)
		if fi, statErr := os.Stat(localPath); statErr == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			var xbuf [4]byte
			if n, _ := syscall.Getxattr(localPath, "user.zus.committed", xbuf[:]); n > 0 {
				var st syscall.Stat_t
				if syscall.Lstat(localPath, &st) == nil && st.Blocks > 0 {
					lf, oerr := os.Open(localPath)
					if oerr == nil {
						totalSize := fi.Size()
						var start, length int64
						if rs == nil {
							start, length = 0, totalSize
						} else if rs.IsSuffixLength {
							start = totalSize + rs.Start
							if start < 0 {
								start = 0
							}
							length = totalSize - start
						} else {
							start = rs.Start
							if rs.End < 0 {
								length = totalSize - start
							} else {
								length = rs.End - rs.Start + 1
							}
						}
						if start >= 0 && start < totalSize && length > 0 {
							if _, serr := lf.Seek(start, io.SeekStart); serr == nil {
								contentType := mime.TypeByExtension(filepath.Ext(object))
								if contentType == "" {
									contentType = "application/octet-stream"
								}
								objInfo := minio.ObjectInfo{
									Bucket:      bucket,
									Name:        object,
									Size:        totalSize,
									ModTime:     fi.ModTime(),
									ContentType: contentType,
									ETag:        fmt.Sprintf("%x-%d", fi.ModTime().UnixNano(), totalSize),
								}
								reader := &limitedReadCloser{
									R: io.LimitReader(lf, length),
									C: lf,
								}
								atomic.AddInt64(&spilloverHitCount, 1)
								cacheHitRate.RecordHit(true)
								gr, err = minio.NewGetObjectReaderFromReader(reader, objInfo, opts, func() { _ = lf.Close() })
								return
							}
						}
						_ = lf.Close()
					}
				}
			}
		}
	}

	f, objectInfo, fCloser, _, err := getFileReader(ctx, zob.alloc, bucket, object, remotePath, rangeStart, rangeEnd)
	if err != nil {
		// S3-upstream fallback: if Zus says not-found or consensus failed
		// and fallback is enabled, fetch from external S3 and concurrently
		// cache-back to Zus. getFileReader wraps pathDoesNotExist as
		// minio.ObjectNotFound, so match that too.
		_, isMinioNotFound := err.(minio.ObjectNotFound)
		if (isPathNoExistError(err) || isConsensusFailedError(err) || isMinioNotFound) && serverConfig.FallbackS3Enabled {
			rc, upInfo, fbErr := fallbackFetchSingleflight(ctx, zob.alloc, bucket, object)
			if fbErr == nil && rc != nil && upInfo != nil {
				oi := minio.ObjectInfo{
					Bucket:      bucket,
					Name:        object,
					Size:        upInfo.Size,
					ModTime:     upInfo.LastModified,
					ETag:        upInfo.ETag,
					ContentType: upInfo.ContentType,
				}
				cleanup := func() { _ = rc.Close() }
				gr, err = minio.NewGetObjectReaderFromReader(rc, oi, opts, cleanup)
				return
			}
			if fbErr != nil && fbErr != ErrFallbackDisabled && fbErr != ErrFallbackNotFound {
				log.Printf("fallback_s3: error bucket=%s key=%s: %v", bucket, object, fbErr)
			}
			return nil, minio.ObjectNotFound{Bucket: bucket, Object: object}
		}
		return nil, err
	}

	// Fix B: S3 read-miss cache-back.
	// Full-file reads (rs == nil): TeeReader the fetched stream into the cache
	// so one blobber fetch serves client AND fills cache simultaneously.
	// Range reads (rs != nil): can't tee partial data to a full file, so we
	// fire a separate background getFileReader(full) via cacheBackFullFetch.
	// Inflight singleflight prevents duplicate work.
	//
	// Destination tier: prefer tmpfs (NFSGaneshaExportDir) when
	// NFSTmpfsCacheEnabled; else fall back to NFSSpilloverDir when
	// NFSSpilloverCacheEnabled; else skip cache-back entirely.
	//
	// This is the first blobber read (we just came out of getFileReader),
	// so bump the blobber counter before dispatching the tee/fetch.
	atomic.AddInt64(&blobberReadCount, 1)
	cacheHitRate.RecordHit(false)

	cacheDir := ""
	switch {
	case serverConfig.NFSTmpfsCacheEnabled && serverConfig.NFSGaneshaExportDir != "":
		cacheDir = serverConfig.NFSGaneshaExportDir
	case serverConfig.NFSSpilloverCacheEnabled && serverConfig.NFSSpilloverDir != "":
		cacheDir = serverConfig.NFSSpilloverDir
	}

	// Large-file fallback: if the chosen cacheDir is tmpfs AND the full
	// file size is larger than tmpfs total capacity (minus margin), divert
	// cache-back to spilloverDir instead. No amount of eviction can make
	// tmpfs fit a file larger than itself; skipping this check would cause
	// cacheBackTee/FullFetch to fail with ENOSPC deep in io.Copy.
	// Assumption: file_size <= spillover_max_bytes.
	var fileSizeForCache int64
	if objectInfo != nil {
		fileSizeForCache = objectInfo.Size
	}
	if cacheDir != "" && cacheDir == serverConfig.NFSGaneshaExportDir &&
		serverConfig.NFSSpilloverDir != "" && currentBS != nil &&
		currentBS.ShouldUseSpillover(fileSizeForCache) {
		log.Printf("[cache-back] file %s/%s size=%d exceeds tmpfs capacity — using spillover dir",
			bucket, object, fileSizeForCache)
		cacheDir = serverConfig.NFSSpilloverDir
	}

	if rs != nil && cacheDir != "" && !serverConfig.NFSCacheDisabled {
		localPath := filepath.Join(cacheDir, bucket, object)
		skip := false
		if fi, lerr := os.Lstat(localPath); lerr == nil && !fi.IsDir() && fi.Size() > 0 {
			var xbuf [2]byte
			if n, _ := syscall.Getxattr(localPath, "user.zus.committed", xbuf[:]); n > 0 {
				var st syscall.Stat_t
				if syscall.Lstat(localPath, &st) == nil && st.Blocks > 0 {
					skip = true
				}
			}
		}
		cacheKey := bucket + "/" + object
		// Range-read miss: serve this range from blobber (we just did)
		// AND spawn a background full-file fetch so subsequent range reads
		// on the same file hit Fix A's local-file fast path.
		// Admission-gated via ShouldCacheFile: the file must fit free tmpfs
		// without forcing an eviction AND hit-rate must be >=40% AND the key
		// must not have been recently evicted. Prevents the 1G-thrash loop
		// where every miss cached back → evicted an older file → re-missed.
		var fileSizeForCB int64
		if objectInfo != nil {
			fileSizeForCB = objectInfo.Size
		}
		if !skip && currentBS != nil && currentBS.ShouldCacheFile(fileSizeForCB, bucket, object) {
			if _, loaded := cacheBackInflight.LoadOrStore(cacheKey, true); !loaded {
				go cacheBackFullFetch(zob.alloc, bucket, object, remotePath, cacheDir, cacheKey)
			}
		}
	}
	if rs == nil && cacheDir != "" && !serverConfig.NFSCacheDisabled {
		localPath := filepath.Join(cacheDir, bucket, object)
		skip := false
		if fi, lerr := os.Lstat(localPath); lerr == nil && !fi.IsDir() && fi.Size() > 0 {
			var xbuf [2]byte
			if n, _ := syscall.Getxattr(localPath, "user.zus.committed", xbuf[:]); n > 0 {
				skip = true
			}
		}
		cacheKey := bucket + "/" + object
		// Use the same 3-part admission gate as the range-miss path above.
		// Prevents thrashing on full-file GETs when the dataset > cache.
		var fullFileSize int64
		if objectInfo != nil {
			fullFileSize = objectInfo.Size
		}
		if currentBS != nil && !currentBS.ShouldCacheFile(fullFileSize, bucket, object) {
			skip = true
		}
		if !skip {
			if _, loaded := cacheBackInflight.LoadOrStore(cacheKey, true); !loaded {
				pr, pw := io.Pipe()
				originalCloser := fCloser
				tee := io.TeeReader(f, pw)
				f = tee
				fCloser = func() {
					_ = pw.Close()
					if originalCloser != nil {
						originalCloser()
					}
				}
				go cacheBackTee(pr, zob.alloc, bucket, object, cacheDir, cacheKey)
			}
		}
	}

	gr, err = minio.NewGetObjectReaderFromReader(f, *objectInfo, opts, fCloser)
	return
}

// cacheBackTee drains a pipe reader (fed by io.TeeReader on the client
// response path) into /nfs_export/<bucket>/<object>, then sets the
// committed xattr and renames atomically. Runs concurrently with the
// client response but does not block it -- the pipe decouples producer
// (client reader) from consumer (cache writer).
func cacheBackTee(pr *io.PipeReader, alloc *sdk.Allocation, bucket, object, nfsDir, cacheKey string) {
	defer cacheBackInflight.Delete(cacheKey)
	defer pr.Close()

	localPath := filepath.Join(nfsDir, bucket, object)
	rel := exportRelPath(bucket, object)

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		log.Printf("[cache-back-tee] mkdir error %s: %v", localPath, err)
		_, _ = io.Copy(io.Discard, pr) // drain so client isn't blocked
		return
	}

	mu := acquirePathLock(rel)
	defer mu.Unlock()

	// Re-check authoritative gate after lock: PutObject or a previous
	// cache-back may have committed the file while we waited.
	if fi, serr := os.Lstat(localPath); serr == nil && !fi.IsDir() && fi.Size() > 0 {
		var xbuf [2]byte
		if n, _ := syscall.Getxattr(localPath, "user.zus.committed", xbuf[:]); n > 0 {
			_, _ = io.Copy(io.Discard, pr)
			return
		}
	}

	tmp := localPath + ".cacheback"
	fh, err := os.Create(tmp)
	if err != nil {
		log.Printf("[cache-back-tee] create error %s: %v", tmp, err)
		_, _ = io.Copy(io.Discard, pr)
		return
	}
	_, copyErr := io.Copy(fh, pr)
	fh.Close()
	if copyErr != nil {
		os.Remove(tmp)
		log.Printf("[cache-back-tee] copy error %s: %v", localPath, copyErr)
		return
	}

	// Set xattr on tmp BEFORE rename so it travels with the inode atomically.
	if xerr := syscall.Setxattr(tmp, "user.zus.committed", []byte{'1'}, 0); xerr != nil {
		log.Printf("[cache-back-tee] setxattr tmp error %s: %v", tmp, xerr)
	}

	if currentBS != nil && rel != "" {
		currentBS.MarkCommitted(rel)
	}

	if err := os.Rename(tmp, localPath); err != nil {
		os.Remove(tmp)
		log.Printf("[cache-back-tee] rename error %s: %v", localPath, err)
		return
	}
	_ = syscall.Setxattr(localPath, "user.zus.committed", []byte{'1'}, 0)
}

// cacheBackFullFetch runs as a goroutine after a range-read miss to fetch the
// full object from blobbers and populate /nfs_export, so that subsequent range
// reads of the same file hit Fix A (local-file fast path). Mirrors the
// pre-TeeReader cacheBackOnMiss semantics; needed because range reads can't
// share a stream with a full-file cache write.
func cacheBackFullFetch(alloc *sdk.Allocation, bucket, object, remotePath, nfsDir, cacheKey string) {
	defer cacheBackInflight.Delete(cacheKey)

	localPath := filepath.Join(nfsDir, bucket, object)
	rel := exportRelPath(bucket, object)

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		log.Printf("[cache-back-fetch] mkdir error %s: %v", localPath, err)
		return
	}

	mu := acquirePathLock(rel)
	defer mu.Unlock()

	// Re-check: another goroutine may have filled it while we waited.
	if fi, lerr := os.Lstat(localPath); lerr == nil && !fi.IsDir() && fi.Size() > 0 {
		var xbuf [2]byte
		if n, _ := syscall.Getxattr(localPath, "user.zus.committed", xbuf[:]); n > 0 {
			var st syscall.Stat_t
			if syscall.Lstat(localPath, &st) == nil && st.Blocks > 0 {
				return
			}
		}
	}

	ctx := context.Background()
	f, _, fCloser, _, err := getFileReader(ctx, alloc, bucket, object, remotePath, 1, 0)
	if err != nil {
		log.Printf("[cache-back-fetch] fetch error %s/%s: %v", bucket, object, err)
		return
	}
	defer fCloser()

	tmp := localPath + ".cachefetch"
	out, err := os.Create(tmp)
	if err != nil {
		log.Printf("[cache-back-fetch] create %s: %v", tmp, err)
		return
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		os.Remove(tmp)
		log.Printf("[cache-back-fetch] copy %s: %v", tmp, err)
		return
	}
	out.Close()

	// xattr before rename so it travels with the inode
	_ = syscall.Setxattr(tmp, "user.zus.committed", []byte{'1'}, 0)
	if currentBS != nil {
		currentBS.MarkCommitted(rel)
	}
	if err := os.Rename(tmp, localPath); err != nil {
		os.Remove(tmp)
		log.Printf("[cache-back-fetch] rename %s: %v", tmp, err)
		return
	}
	// Defensive: re-set xattr on final inode
	_ = syscall.Setxattr(localPath, "user.zus.committed", []byte{'1'}, 0)
}

// cacheBackInflight deduplicates background cache-back downloads.
var cacheBackInflight sync.Map

// ListBuckets Lists directories of root path(/) and root path itself as buckets.
func (zob *zcnObjects) ListBuckets(ctx context.Context) (buckets []minio.BucketInfo, err error) {
	rootRef, err := getSingleRegularRef(zob.alloc, rootPath)
	if err != nil {
		if isPathNoExistError(err) {
			buckets = append(buckets, minio.BucketInfo{
				Name:    rootBucketName,
				Created: time.Now().Add(-time.Hour * 30),
			})
			return buckets, nil
		}
		return nil, err
	}

	dirRefs, err := listRootDir(zob.alloc, "d")
	if err != nil {
		return nil, err
	}

	// Consider root path as bucket as well.
	buckets = append(buckets, minio.BucketInfo{
		Name:    rootBucketName,
		Created: rootRef.CreatedAt.ToTime(),
	})

	for _, dirRef := range dirRefs {
		buckets = append(buckets, minio.BucketInfo{
			Name:    dirRef.Name,
			Created: dirRef.CreatedAt.ToTime(),
		})
	}
	return
}

func (zob *zcnObjects) ListObjectsV2(ctx context.Context, bucket, prefix, continuationToken, delimiter string, maxKeys int, fetchOwner bool, startAfter string) (result minio.ListObjectsV2Info, err error) {
	marker := continuationToken
	if marker == "" {
		marker = startAfter
	}

	var resultV1 minio.ListObjectsInfo
	resultV1, err = zob.ListObjects(ctx, bucket, prefix, marker, delimiter, maxKeys)
	if err != nil {
		return
	}

	result.Objects = resultV1.Objects
	result.Prefixes = resultV1.Prefixes
	result.ContinuationToken = continuationToken
	result.NextContinuationToken = resultV1.NextMarker
	result.IsTruncated = resultV1.IsTruncated
	return
}

// ListObjects Lists files of directories as objects
func (zob *zcnObjects) ListObjects(ctx context.Context, bucket, prefix, marker, delimiter string, maxKeys int) (result minio.ListObjectsInfo, err error) {
	// objFileType For root path list objects should only provide file and not dirs.
	// Dirs under root path are presented as buckets as well
	var remotePath, objFileType string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, prefix)
		objFileType = fileType
	} else {
		remotePath = filepath.Join(rootPath, bucket, prefix)
	}

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		if isPathNoExistError(err) {
			return result, nil
		}
		return
	}

	if ref.Type == fileType {
		if strings.HasSuffix(prefix, "/") {
			return minio.ListObjectsInfo{
					IsTruncated: false,
					Objects:     []minio.ObjectInfo{},
					Prefixes:    []string{},
				},
				nil
		}
		return minio.ListObjectsInfo{
				IsTruncated: false,
				Objects: []minio.ObjectInfo{
					{
						Bucket:       bucket,
						Name:         getRelativePathOfObj(ref.Path, bucket),
						Size:         ref.ActualFileSize,
						IsDir:        false,
						ModTime:      ref.UpdatedAt.ToTime(),
						ETag:         ref.ActualFileHash,
						ContentType:  ref.MimeType,
						AccTime:      time.Now(),
						StorageClass: "STANDARD",
					},
				},
				Prefixes: []string{},
			},
			nil
	}

	// if len(prefix) > 0 && prefix[len(prefix)-1] != '/' {
	// 	return minio.ListObjectsInfo{
	// 			IsTruncated: false,
	// 			Objects:     []minio.ObjectInfo{},
	// 			Prefixes:    []string{prefix + "/"},
	// 		},
	// 		nil
	// }

	var objects []minio.ObjectInfo
	// if prefix != "" {
	// 	userDefined := make(map[string]string)
	// 	if ref.CustomMeta != "" {
	// 		_ = json.Unmarshal([]byte(ref.CustomMeta), &userDefined)
	// 	}
	// 	log.Println("prefixNonEmpty: ", prefix)
	// 	objects = append(objects, minio.ObjectInfo{
	// 		Bucket:       bucket,
	// 		Name:         prefix,
	// 		ModTime:      ref.UpdatedAt.ToTime(),
	// 		Size:         0,
	// 		IsDir:        true,
	// 		ContentType:  s3DirectoryContentType,
	// 		ETag:         s3ContentHash,
	// 		StorageClass: "STANDARD",
	// 		UserDefined:  userDefined,
	// 	})
	// }
	var isDelimited bool
	if delimiter != "" {
		isDelimited = true
	} else {
		objFileType = fileType
	}
	refs, isTruncated, nextMarker, prefixes, err := listRegularRefs(zob.alloc, remotePath, marker, objFileType, maxKeys, isDelimited)
	if err != nil {
		if remotePath == rootPath && isPathNoExistError(err) {
			return minio.ListObjectsInfo{}, nil
		}
		return minio.ListObjectsInfo{}, err
	}

	for _, ref := range refs {
		if ref.Type == dirType {
			continue
		}
		userDefined := make(map[string]string)
		if ref.CustomMeta != "" {
			_ = json.Unmarshal([]byte(ref.CustomMeta), &userDefined)
		}
		objects = append(objects, minio.ObjectInfo{
			Bucket:       bucket,
			Name:         getRelativePathOfObj(ref.Path, bucket),
			ModTime:      ref.UpdatedAt.ToTime(),
			Size:         ref.ActualFileSize,
			IsDir:        false,
			ContentType:  ref.MimeType,
			ETag:         ref.ActualFileHash,
			StorageClass: "STANDARD",
			UserDefined:  userDefined,
		})
	}

	// MinIO writeback cache includes uncommitted cached objects in listing.
	result.IsTruncated = isTruncated
	result.NextMarker = nextMarker
	result.Objects = objects
	result.Prefixes = prefixes
	return
}

// getRelativePathOfObj returns the relative path of a file without the leading slash and without the name of the bucket
func getRelativePathOfObj(refPath, bucketName string) string {
	//eg: refPath = "/myFile.txt" bucketName = "/", return value = "myFile.txt"
	//eg: refPath = "/buck1/myFile.txt" bucketName = anything other than "/" or "root", return value = "myFile.txt"
	//eg: refPath = "/myFile.txt" bucketName = "abc", return value = "myFile.txt"
	//remotePath = "/xyz/abc/def", return value = "abc/def"

	if bucketName == rootPath || bucketName == rootBucketName {
		return strings.TrimPrefix(refPath, rootPath)
	}

	return getCommonPrefix(refPath)
}

func (zob *zcnObjects) MakeBucketWithLocation(ctx context.Context, bucket string, opts minio.BucketOptions) error {
	// Create a directory; ignore opts
	if bucket == rootBucketName {
		return nil
	}
	remotePath := filepath.Join(rootPath, bucket)
	createDirOp := sdk.OperationRequest{
		OperationType: constants.FileOperationCreateDir,
		RemotePath:    remotePath,
	}
	if err := zob.alloc.DoMultiOperation([]sdk.OperationRequest{
		createDirOp,
	}); err != nil {
		return err
	}
	mirrorS3MakeBucketToExport(bucket)
	return nil
}

func (zob *zcnObjects) PutObject(ctx context.Context, bucket, object string, r *minio.PutObjReader, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	// var ref *sdk.ORef
	// var isUpdate bool
	// err = lockPath(ctx, remotePath)
	// if err != nil {
	// 	return
	// }
	// ref, err = getSingleRegularRef(zob.alloc, remotePath)
	// if err != nil {
	// 	if !isPathNoExistError(err) {
	// 		unlockPath(remotePath)
	// 		return
	// 	}
	// }

	// if ref != nil {
	// 	logger.Info("updateFile: ", remotePath)
	// 	isUpdate = true
	// 	unlockPath(remotePath)
	// } else {
	// 	defer unlockPath(remotePath)
	// }

	contentType := opts.UserDefined["content-type"]
	if contentType == "" {
		contentType = mimedb.TypeByExtension(path.Ext(object))
	}

	if object[len(object)-1] == '/' {
		createDirOp := sdk.OperationRequest{
			OperationType: constants.FileOperationCreateDir,
			RemotePath:    remotePath,
		}
		customMeta, _ := json.Marshal(opts.UserDefined)
		createDirOp.FileMeta.CustomMeta = string(customMeta)
		err = zob.alloc.DoMultiOperation([]sdk.OperationRequest{
			createDirOp,
		})
		if err != nil {
			return
		} else {
			mirrorS3MakeDirToExport(bucket, object)
			return minio.ObjectInfo{
				Bucket:      bucket,
				Name:        object,
				Size:        0,
				ModTime:     time.Now(),
				IsDir:       true,
				ContentType: s3DirectoryContentType,
				ETag:        s3ContentHash,
				UserDefined: opts.UserDefined,
			}, nil
		}
	}

	// Single-cache architecture: write bytes to /nfs_export first (fast,
	// tmpfs), then upload from local to blobbers. Both NFS and S3 read
	// paths see the same file in /nfs_export — no stubs, no dual cache.
	//
	// Two distinct races we have to defend against, with two distinct gates:
	//   (1) BlobberSync.processEvents fires on the inotify Create/Write
	//       events from os.Create/io.Copy. If the file is brand new (no
	//       stub xattr) and not in bs.committed, processEvents queues it
	//       for upload via commitBatch — racing this PutObject's own
	//       putFile call. We MUST MarkCommitted before opening the file
	//       so the very first inotify event finds the path in bs.committed.
	//   (2) spillCommittedFiles iterates bs.committed and could read+
	//       truncate a file mid-write. The defense-in-depth committed
	//       xattr gate (added in v3) is what prevents this — set the
	//       xattr only AFTER putFile success, so any spillover candidate
	//       lacking the xattr is skipped.
	exportDir := serverConfig.NFSGaneshaExportDir
	rel := exportRelPath(bucket, object)
	size := r.Size()
	// Race E: acquire the per-path lock for the full single-cache
	// write window so concurrent S3 PUT (retry), NFS write, spill, and
	// cache-back-on-miss on the same object serialise against each
	// other rather than trampling one another's local bytes and xattrs.
	var pmu *sync.Mutex
	if rel != "" {
		pmu = acquirePathLock(rel)
		defer pmu.Unlock()
	}
	// Large-object guard: /nfs_export is a modest tmpfs (typically 8 GiB).
	// Writing a >=1 GiB tmp file plus renaming to the final name keeps two
	// copies briefly, and concurrent uploads multiply that. That blew up as
	// ENOSPC on 1.5 GiB PUTs (obj_00000 / obj_00001 io.Copy → "no space left
	// on device"), which minio surfaces as a closed connection to the client.
	// For large objects, spool the body to NFSSpilloverDir (roomy NVMe) and
	// stream the upload from that file, skipping the tmpfs cache entirely.
	largeObjUploaded := false
	if size >= largeObjectCacheSkipBytes && exportDir != "" && rel != "" {
		spillDir := serverConfig.NFSSpilloverDir
		if spillDir == "" {
			spillDir = os.TempDir()
		}
		if mkErr := os.MkdirAll(spillDir, 0o755); mkErr != nil {
			logger.LogIf(ctx, mkErr)
		}
		spillPath := filepath.Join(spillDir, fmt.Sprintf("zs3_large.%d.%d", time.Now().UnixNano(), os.Getpid()))
		sf, sErr := os.Create(spillPath)
		if sErr != nil {
			logger.LogIf(ctx, sErr)
			// Last-resort: direct stream from HTTP body.
			err = putFile(ctx, zob.alloc, remotePath, contentType, r, size, false, opts.UserDefined)
		} else {
			n, cpErr := io.Copy(sf, r)
			sf.Close()
			if cpErr != nil {
				os.Remove(spillPath)
				err = cpErr
			} else {
				sr, oErr := os.Open(spillPath)
				if oErr != nil {
					os.Remove(spillPath)
					err = oErr
				} else {
					err = putFile(ctx, zob.alloc, remotePath, contentType, sr, n, false, opts.UserDefined)
					sr.Close()
					os.Remove(spillPath)
				}
			}
		}
		if err != nil {
			return
		}
		largeObjUploaded = true
		// Skip the tmpfs cache-write block by clearing exportDir; but we
		// also need to avoid the outer-else fallback that re-calls putFile
		// with the drained body. The largeObjUploaded flag below handles it.
		exportDir = ""
	}
	if largeObjUploaded {
		// Large-obj path already uploaded to blobbers; build objInfo and return.
		objInfo = minio.ObjectInfo{
			Bucket:      bucket,
			Name:        object,
			Size:        size,
			ModTime:     time.Now(),
			UserDefined: opts.UserDefined,
		}
		return
	}
	if exportDir != "" && rel != "" {
		localPath := filepath.Join(exportDir, rel)
		if mkErr := os.MkdirAll(filepath.Dir(localPath), 0o755); mkErr != nil {
			logger.LogIf(ctx, mkErr)
		}
		// Gate (1): admit to bs.committed BEFORE open/write so the inotify
		// event handler skips this file (it's "ours").
		if currentBS != nil {
			currentBS.MarkCommitted(rel)
		}
		// Linearizability: write to a unique per-request tmp file, then
		// atomically rename into place AFTER the blobber commit succeeds.
		// os.Create(localPath) directly would truncate in-place and readers
		// with an open fd would see zero/partial bytes mid-write (Porcupine
		// caught this as NON-LINEARIZABLE). Rename is atomic on the same
		// tmpfs, so any reader sees either the old inode or the new one,
		// never a mix.
		tmpPath := fmt.Sprintf("%s.tmp.%d.%d", localPath, time.Now().UnixNano(), os.Getpid())
		localF, cErr := os.Create(tmpPath)
		if cErr != nil {
			logger.LogIf(ctx, cErr)
			// Fall back to direct blobber upload without local cache
			err = putFile(ctx, zob.alloc, remotePath, contentType, r, size, false, opts.UserDefined)
		} else {
			n, cpErr := io.Copy(localF, r)
			localF.Close()
			if cpErr != nil {
				os.Remove(tmpPath)
				err = cpErr
			} else {
				size = n
				// Upload from tmp cache to blobbers FIRST; rename only on success.
				localR, oErr := os.Open(tmpPath)
				if oErr != nil {
					os.Remove(tmpPath)
					err = oErr
				} else {
					err = putFile(ctx, zob.alloc, remotePath, contentType, localR, n, false, opts.UserDefined)
					localR.Close()
					if err == nil {
						// Atomic replace of the visible name. Any concurrent
						// reader that had localPath open sees the old inode
						// until it closes; new opens see the new inode.
						if rErr := os.Rename(tmpPath, localPath); rErr != nil {
							os.Remove(tmpPath)
							err = rErr
						}
					} else {
						os.Remove(tmpPath)
					}
				}
			}
		}
	} else {
		err = putFile(ctx, zob.alloc, remotePath, contentType, r, size, false, opts.UserDefined)
	}
	if err != nil {
		return
	}

	// Gate (2): xattr is set ONLY after putFile succeeds. This is what
	// spillCommittedFiles checks — any candidate without the xattr is mid-
	// write and gets skipped. Same gate also tells initialScan to skip on
	// restart.
	if exportDir != "" && rel != "" {
		localPath := filepath.Join(exportDir, rel)
		_ = syscall.Setxattr(localPath, "user.zus.committed", []byte{'1'}, 0)

		// A successful PUT resurrects a previously-deleted key: clear any
		// live tombstone so the next GET returns the new content rather
		// than continuing to mask it as NoSuchKey.
		if walWriter != nil {
			walWriter.ClearTombstone(bucket, object)
		}

		// Write-through to upstream S3 (if configured). Stream from the
		// local /nfs_export copy so we don't re-read from blobbers.
		// doUpstreamPut handles bucket-create-on-first-use + retries.
		// In "async" mode this returns immediately; in "mirror" mode it
		// blocks and may return an error that we surface to the client.
		if writeThroughEnabled() != "" {
			if wtErr := syncPutStreamToUpstream(ctx, bucket, object, localPath, size, contentType); wtErr != nil {
				if writeThroughEnabled() == "mirror" {
					err = wtErr
					return
				}
				// async-mode error is already logged in doUpstreamPut
			}
		}
	}

	objInfo = minio.ObjectInfo{
		Bucket:      bucket,
		Name:        object,
		Size:        size,
		ModTime:     time.Now(),
		UserDefined: opts.UserDefined,
	}
	return
}

func (zob *zcnObjects) PutMultipleObjects(
	ctx context.Context,
	bucket string,
	objects []string,
	r []*minio.PutObjReader,
	opts []minio.ObjectOptions,
) ([]minio.ObjectInfo, error) {
	total := len(objects)
	if total <= 0 {
		return nil, fmt.Errorf("no files to upload")
	}

	if total != len(r) || total != len(opts) {
		return nil, fmt.Errorf("length mismatch of objects with file readers or with options")
	}

	remotePaths := make([]string, total)
	for i, object := range objects {
		if bucket == rootBucketName {
			remotePaths[i] = filepath.Join(rootPath, object)
		} else {
			remotePaths[i] = filepath.Join(rootPath, bucket, object)
		}
	}
	operationRequests := make([]sdk.OperationRequest, total)
	objectInfo := make([]minio.ObjectInfo, total)
	var wg sync.WaitGroup
	errCh := make(chan error)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var ref *sdk.ORef
			ref, err := getSingleRegularRef(zob.alloc, remotePaths[idx])
			if err != nil {
				if !isPathNoExistError(err) {
					errCh <- err
					return
				}
			}

			var isUpdate bool
			if ref != nil {
				isUpdate = true
			}

			_, fileName := filepath.Split(remotePaths[idx])
			fileMeta := sdk.FileMeta{
				Path:       "",
				RemotePath: remotePaths[idx],
				ActualSize: r[idx].Size(),
				RemoteName: fileName,
			}

			options := []sdk.ChunkedUploadOption{
				sdk.WithEncrypt(encrypt),
				sdk.WithChunkNumber(120),
			}
			operationRequests[idx] = sdk.OperationRequest{
				FileMeta:      fileMeta,
				FileReader:    newMinioReader(r[idx]),
				OperationType: constants.FileOperationInsert,
				Opts:          options,
			}
			if isUpdate {
				operationRequests[idx].OperationType = constants.FileOperationUpdate
			}
			objectInfo[idx] = minio.ObjectInfo{
				Bucket:  bucket,
				Name:    objects[idx],
				Size:    r[idx].Size(),
				ModTime: time.Now(),
			}
		}(i)

		select {
		case err := <-errCh:
			logger.Error("error while getting file ref and creating operationRequests.")
			return nil, err
		default:
		}
	}
	wg.Wait()

	errn := zob.alloc.DoMultiOperation(operationRequests)
	if errn != nil {
		logger.Error("error in sending multioperation to gosdk: %v", errn)
		return nil, errn
	}

	for i := range objectInfo {
		mirrorS3PutToExport(bucket, objects[i], r[i].Size())
	}
	return objectInfo, nil
}
func (zob *zcnObjects) CopyObject(ctx context.Context, srcBucket, srcObject, destBucket, destObject string, srcInfo minio.ObjectInfo, srcOpts, dstOpts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	// MinIO writeback cache handles copy from cache automatically.
	var srcRemotePath, dstRemotePath string
	if srcBucket == rootBucketName {
		srcRemotePath = filepath.Join(rootPath, srcObject)
	} else {
		srcRemotePath = filepath.Join(rootPath, srcBucket, srcObject)
	}

	if destBucket == rootBucketName {
		dstRemotePath = filepath.Join(rootPath, destObject)
	} else {
		dstRemotePath = filepath.Join(rootPath, destBucket, destObject)
	}

	var ref *sdk.ORef
	if srcRemotePath == dstRemotePath {
		ref, err = getSingleRegularRef(zob.alloc, dstRemotePath)
		if err != nil {
			return
		}
		if ref.Type == dirType {
			ref.MimeType = s3DirectoryContentType
			ref.ActualFileSize = 0
			ref.ActualFileHash = s3ContentHash
		}
		return minio.ObjectInfo{
			Bucket:      destBucket,
			Name:        destObject,
			ModTime:     ref.UpdatedAt.ToTime(),
			Size:        ref.ActualFileSize,
			ContentType: ref.MimeType,
			IsDir:       ref.Type == dirType,
			ETag:        ref.ActualFileHash,
		}, nil
	}
	copyOp := sdk.OperationRequest{
		OperationType: constants.FileOperationCopy,
		RemotePath:    srcRemotePath,
		DestPath:      dstRemotePath,
	}
	err = zob.alloc.DoMultiOperation([]sdk.OperationRequest{
		copyOp,
	})
	if err != nil {
		return
	}

	ref, err = getSingleRegularRef(zob.alloc, dstRemotePath)
	if err != nil {
		return
	}
	if ref.Type == dirType {
		ref.MimeType = s3DirectoryContentType
		ref.ActualFileSize = 0
		ref.ActualFileHash = s3ContentHash
		mirrorS3MakeDirToExport(destBucket, destObject)
	} else {
		mirrorS3PutToExport(destBucket, destObject, ref.ActualFileSize)
	}

	return minio.ObjectInfo{
		Bucket:      destBucket,
		Name:        destObject,
		ModTime:     ref.UpdatedAt.ToTime(),
		Size:        ref.ActualFileSize,
		IsDir:       ref.Type == dirType,
		ContentType: ref.MimeType,
		ETag:        ref.ActualFileHash,
	}, nil
}

func (zob *zcnObjects) StorageInfo(ctx context.Context) (si minio.StorageInfo, _ []error) {
	si.Backend.Type = madmin.Gateway
	si.Backend.GatewayOnline = true
	return
}

/*
//Unfortunately share file is done by minio client which does't need to communicate with server. It generates share url with access key id and
//secret key
func (zob *zcnObjects) ShareFile(ctx context.Context, bucket, object, clientID, pubEncryp string, expires, availableAfter time.Duration) (string, error) {
	var remotePath string
	if bucket == "" || (bucket == rootBucketName && object == "") {
		//share entire allocation i.e. rootpath
	} else if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	var ref *sdk.ORef
	ref, err := getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		return "", err
	}

	_, fileName := filepath.Split(remotePath)

	authTicket, err := zob.alloc.GetAuthTicket(remotePath, fileName, ref.Type, clientID, pubEncryp, int64(expires.Seconds()), int64(availableAfter.Seconds()))
	if err != nil {
		return "", err
	}

	_ = authTicket
	//get public url from 0NFT
	return "", nil
}

func (zob *zcnObjects) RevokeShareCredential(ctx context.Context, bucket, object, clientID string) (err error) {
	var remotePath string
	if bucket == "" || (bucket == rootBucketName && object == "") {
		//share entire allocation i.e. rootpath
	} else if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	_, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		return
	}

	return zob.alloc.RevokeShare(remotePath, clientID)
}
*/

// ListMultipartUploads(ctx context.Context, bucket, prefix, keyMarker, uploadIDMarker, delimiter string, maxUploads int) (result ListMultipartsInfo, err error)
// CopyObjectPart(ctx context.Context, srcBucket, srcObject, destBucket, destObject string, uploadID string, partID int,
// 	startOffset int64, length int64, srcInfo ObjectInfo, srcOpts, dstOpts ObjectOptions) (info PartInfo, err error)
