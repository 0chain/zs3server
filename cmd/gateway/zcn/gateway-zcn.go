package zcn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0chain/gosdk/constants"
	"github.com/minio/minio/internal/logger"

	"github.com/0chain/gosdk/zboxcore/sdk"
	"github.com/minio/cli"
	"github.com/minio/madmin-go"
	minio "github.com/minio/minio/cmd"
)

const (
	rootPath       = "/"
	rootBucketName = "root"
)

var configDir string
var allocationID string
var nonce int64

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

// NewGatewayLayer initializes 0chain gosdk and return zcnObjects
func (z *ZCN) NewGatewayLayer(creds madmin.Credentials) (minio.ObjectLayer, error) {
	err := initializeSDK(configDir, allocationID, nonce)
	if err != nil {
		return nil, err
	}

	allocation, err := sdk.GetAllocation(allocationID)
	if err != nil {
		return nil, err
	}

	zob := &zcnObjects{
		alloc:   allocation,
		metrics: minio.NewMetrics(),
	}

	return zob, nil
}

type zcnObjects struct {
	minio.GatewayUnsupported
	alloc   *sdk.Allocation
	metrics *minio.BackendMetrics
}

// Shutdown Remove temporary directory
func (zob *zcnObjects) Shutdown(ctx context.Context) error {
	os.RemoveAll(tempdir)
	return nil
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

	if opts.Force {
		return zob.alloc.DeleteFile(remotePath)
	}

	if ref.Size > 0 {
		return fmt.Errorf("%v bucket is not empty", bucketName)
	}

	return zob.alloc.DeleteFile(remotePath)
}

func (zob *zcnObjects) DeleteObject(ctx context.Context, bucket, object string, opts minio.ObjectOptions) (oInfo minio.ObjectInfo, err error) {
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		return
	}

	err = zob.alloc.DeleteFile(remotePath)
	if err != nil {
		return
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

	for _, object := range objects {
		remotePath := filepath.Join(basePath, object.ObjectName)
		err := zob.alloc.DeleteFile(remotePath)
		if err != nil {
			errs = append(errs, err)
		} else {
			delObs = append(delObs, minio.DeletedObject{
				ObjectName: object.ObjectName,
			})
		}
	}
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
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, filepath.Clean(remotePath))
	if err != nil {
		if isPathNoExistError(err) {
			return objInfo, minio.ObjectNotFound{Bucket: bucket, Object: object}
		}
		return
	}

	if ref.Type == dirType {
		return minio.ObjectInfo{}, minio.ObjectNotFound{Bucket: bucket, Object: object}
	}

	return minio.ObjectInfo{
		Bucket:      bucket,
		Name:        getRelativePathOfObj(ref.Path, bucket),
		ModTime:     ref.UpdatedAt.ToTime(),
		Size:        ref.ActualFileSize,
		IsDir:       ref.Type == dirType,
		AccTime:     time.Now(),
		ContentType: ref.MimeType,
	}, nil
}

// GetObjectNInfo Provides reader with read cursor placed at offset upto some length
func (zob *zcnObjects) GetObjectNInfo(ctx context.Context, bucket, object string, rs *minio.HTTPRangeSpec, h http.Header, lockType minio.LockType, opts minio.ObjectOptions) (gr *minio.GetObjectReader, err error) {
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	f, objectInfo, localPath, err := getFileReader(ctx, zob.alloc, bucket, object, remotePath)
	if err != nil {
		return nil, err
	}

	finfo, err := f.Stat()
	if err != nil {
		return nil, err
	}

	startOffset, length, err := rs.GetOffsetLength(finfo.Size())
	if err != nil {
		return nil, err
	}
	log.Printf("~~~~~~~~~~~~~~~~~~~~~~~~ startOffset: %v, length: %v\n", startOffset, length)

	fCloser := func() {
		mu.Lock()
		defer mu.Unlock()
		ds, ok := downloads[remotePath]
		if !ok {
			log.Println("file closer - download status not found for ", remotePath)
			return
		}

		ds.downloaded += length
		if ds.downloaded >= ds.objectInfo.Size {
			f.Close()
			os.Remove(localPath)
			delete(downloads, remotePath)

			log.Println("^^^^^^^^^^^ remove temp local file: ", localPath, "download from Zus:", ds.downloadTime)
		}
	}

	r := io.NewSectionReader(f, startOffset, length)
	log.Printf("~~~~~~~~~~~~~~~~~~~~~~~~~~~~section reader : %v\n", r)
	gr, err = minio.NewGetObjectReaderFromReader(r, *objectInfo, opts, fCloser)
	return
}

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

	var objects []minio.ObjectInfo
	var isDelimited bool
	if delimiter != "" {
		isDelimited = true
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

		objects = append(objects, minio.ObjectInfo{
			Bucket:       bucket,
			Name:         getRelativePathOfObj(ref.Path, bucket),
			ModTime:      ref.UpdatedAt.ToTime(),
			Size:         ref.ActualFileSize,
			IsDir:        false,
			ContentType:  ref.MimeType,
			ETag:         ref.ActualFileHash,
			StorageClass: "STANDARD",
		})
	}

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
	remotePath := filepath.Join(rootPath, bucket)
	createDirOp := sdk.OperationRequest{
		OperationType: constants.FileOperationCreateDir,
		RemotePath:    remotePath,
	}
	return zob.alloc.DoMultiOperation([]sdk.OperationRequest{
		createDirOp,
	})
}

func (zob *zcnObjects) PutObject(ctx context.Context, bucket, object string, r *minio.PutObjReader, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}

	var ref *sdk.ORef
	var isUpdate bool
	ref, err = getSingleRegularRef(zob.alloc, remotePath)
	if err != nil {
		if !isPathNoExistError(err) {
			return
		}
	}

	if ref != nil {
		isUpdate = true
	}

	contentType := opts.UserDefined["content-type"]
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	err = putFile(ctx, zob.alloc, remotePath, contentType, r, r.Size(), isUpdate, false)
	if err != nil {
		return
	}

	objInfo = minio.ObjectInfo{
		Bucket:  bucket,
		Name:    object,
		Size:    r.Size(),
		ModTime: time.Now(),
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
				sdk.WithEncrypt(false),
				sdk.WithChunkNumber(200),
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

	return objectInfo, nil
}
func (zob *zcnObjects) CopyObject(ctx context.Context, srcBucket, srcObject, destBucket, destObject string, srcInfo minio.ObjectInfo, srcOpts, dstOpts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
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

	var ref *sdk.ORef
	ref, err = getSingleRegularRef(zob.alloc, dstRemotePath)
	if err != nil {
		return
	}

	return minio.ObjectInfo{
		Bucket:  destBucket,
		Name:    destObject,
		ModTime: ref.UpdatedAt.ToTime(),
		Size:    ref.ActualFileSize,
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
