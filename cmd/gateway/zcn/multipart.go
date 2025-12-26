package zcn

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0chain/gosdk/constants"
	"github.com/0chain/gosdk/zboxcore/sdk"
	"github.com/google/uuid"
	"github.com/pierrec/lz4/v4"

	"hash"

	minio "github.com/minio/minio/cmd"
	"github.com/minio/pkg/mimedb"
)

var (
	FileMap = make(map[string]*MultiPartFile)
	mapLock sync.Mutex
	// alloc   *sdk.Allocation
	localStorageDir = "store"
)

const lz4MimeType = "application/x-lz4"

const PartSize = 1024 * 128
const minParallelParts = 5  // Minimum number of parts to process in parallel
const maxParallelParts = 20 // Maximum number of parts to process in parallel

type MultiPartFile struct {
	memFile          *memFile
	lock             sync.Mutex
	fileSize         int64
	lastPartSize     int64
	lastPartID       int
	lastPartUpdated  bool
	errorC           chan error
	cancelC          chan struct{}  // indicate the cancel of the uploading
	dataC            chan []byte    // data to be uploaded
	orderedDataC     chan partData  // ordered channel for parallel processing
	partsReady       map[int]bool   // track which parts are ready to process
	partsReadyLock   sync.Mutex     // lock for partsReady map
	workerPool       chan struct{}  // worker pool semaphore (dynamic size = blobber count)
	parts            map[int][]byte // in-memory storage for parts
	partsLock        sync.Mutex     // lock for parts map
	etags            map[int]string // in-memory storage for part ETags
	availableParts   chan int       // channel for parts ready to process (replaces seqPQ)
	fileHasher       hash.Hash      // MD5 hasher for inline file hash
	fileHasherLock   sync.Mutex     // lock for file hasher
	allPartsReceived bool           // flag to indicate all parts have been received
	allPartsLock     sync.Mutex     // lock for allPartsReceived flag
}

type partData struct {
	partNumber int
	data       []byte
}

func (mpf *MultiPartFile) UpdateFileSize(partID int, size int64) {
	mpf.lock.Lock()
	defer mpf.lock.Unlock()
	if mpf.lastPartUpdated {
		return
	}

	// the first arrived part
	if mpf.lastPartSize == 0 {
		mpf.lastPartSize = size
		mpf.lastPartID = partID
		mpf.fileSize += size
		return
	}

	// size greater than the previous set part size, means the last set part is the last part
	if size > mpf.lastPartSize {
		// the prev set part is the last part
		mpf.fileSize = int64(mpf.lastPartID-1)*size + mpf.lastPartSize
		mpf.lastPartSize = size
		return
	}

	if size == mpf.lastPartSize {
		mpf.fileSize += size
		return
	}

	// this is last part
	mpf.fileSize = int64(partID-1)*mpf.lastPartSize + size
	mpf.lastPartUpdated = true
}

func (zob *zcnObjects) NewMultipartUpload(ctx context.Context, bucket string, object string, opts minio.ObjectOptions) (uploadID string, err error) {
	log.Println("initial multipart upload, partNumber:", opts.PartNumber)
	contentType := opts.UserDefined["content-type"]
	if contentType == "" {
		contentType = mimedb.TypeByExtension(path.Ext(object))
	}

	var toCompress bool

	if compress && !hasStringSuffixInSlice(object, minio.StandardExcludeCompressExtensions) && !hasPattern(minio.StandardExcludeCompressContentTypes, contentType) {
		toCompress = true
		contentType = lz4MimeType
	}

	return zob.newMultiPartUpload(localStorageDir, bucket, object, contentType, toCompress, opts.UserDefined)
}

func (zob *zcnObjects) newMultiPartUpload(localStorageDir, bucket, object, contentType string, toCompress bool, userDefined map[string]string) (string, error) {
	// Generate a unique upload ID
	var isUpdate bool
	var remotePath string
	if bucket == rootBucketName {
		remotePath = filepath.Join(rootPath, object)
	} else {
		remotePath = filepath.Join(rootPath, bucket, object)
	}
	// ref, err := getSingleRegularRef(zob.alloc, remotePath)
	// if err != nil {
	// 	if !isPathNoExistError(err) {
	// 		return "", err
	// 	}
	// }

	// if ref != nil {
	// 	isUpdate = true
	// }
	uploadID := uuid.New().String()
	mapLock.Lock()
	memFile := &memFile{
		memFileDataChan: make(chan memFileData, 240),
		errChan:         make(chan error),
	}
	chunkWriteSize := int(zob.alloc.GetChunkReadSize(encrypt))

	// Calculate dynamic worker pool size based on blobber count
	blobberCount := zob.alloc.DataShards + zob.alloc.ParityShards
	if blobberCount < minParallelParts {
		blobberCount = minParallelParts
	}
	if blobberCount > maxParallelParts {
		blobberCount = maxParallelParts
	}

	// Calculate chunking worker pool size (parallel chunking workers)
	chunkingWorkers := blobberCount
	if chunkingWorkers < 3 {
		chunkingWorkers = 3 // Minimum 3 chunking workers
	}
	if chunkingWorkers > 10 {
		chunkingWorkers = 10 // Cap at 10 to avoid excessive memory usage
	}

	multiPartFile := &MultiPartFile{
		memFile:          memFile,
		errorC:           make(chan error, 1),
		dataC:            make(chan []byte, 200), // Increased buffer for better pipelining (was 50)
		orderedDataC:     make(chan partData, 200), // Increased buffer (was 50)
		partsReady:       make(map[int]bool),
		workerPool:       make(chan struct{}, blobberCount), // Dynamic worker pool = blobber count
		cancelC:          make(chan struct{}, 1),
		parts:            make(map[int][]byte), // In-memory storage for parts
		etags:            make(map[int]string), // In-memory storage for ETags
		availableParts:   make(chan int, 200),  // Increased buffer (was 100)
		fileHasher:       md5.New(),            // Inline file hash (MD5)
		allPartsReceived: false,
	}
	FileMap[uploadID] = multiPartFile
	mapLock.Unlock()
	// No need to create directories - we're using in-memory storage

	// Parallel chunking workers - process data from dataC concurrently
	// For compression, we use a single sequential worker to maintain compression stream integrity
	// For non-compressed uploads, we use multiple parallel workers
	if toCompress {
		// Sequential chunking for compression (compression requires sequential processing)
		go func() {
			buf := &bytes.Buffer{}
			zw := lz4.NewWriter(buf)
			zw.Apply(lz4.CompressionLevelOption(lz4.Level1)) //nolint:errcheck
			var total int64
			st := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			for {
				select {
				case <-multiPartFile.cancelC:
					log.Println("upload is canceled, clean up temp dirs")
					memFile.errChan <- fmt.Errorf("upload is canceled")
					cleanupPartFilesAndDirs(bucket, uploadID, localStorageDir)
					return
				case <-ctx.Done():
					log.Println("upload is timed out, clean up temp dirs")
					memFile.errChan <- fmt.Errorf("upload is timed out")
					cleanupPartFilesAndDirs(bucket, uploadID, localStorageDir)
					return
				case data, ok := <-multiPartFile.dataC:
					if ok {
						// Inline file hash (MD5) - thread-safe
						multiPartFile.fileHasherLock.Lock()
						multiPartFile.fileHasher.Write(data)
						multiPartFile.fileHasherLock.Unlock()

						_, err := zw.Write(data)
						if err != nil {
							log.Println("write data to compression buffer failed:", err)
							multiPartFile.cancelC <- struct{}{}
							break
						}

						n := buf.Len() / chunkWriteSize
						if n == 0 {
							continue
						}
						if buf.Len()%chunkWriteSize == 0 && n > 1 {
							n--
						}
						bbuf := make([]byte, n*chunkWriteSize)
						_, err = buf.Read(bbuf)
						if err != nil {
							log.Panic(err)
						}

						current := 0
						for ; current < len(bbuf); current += chunkWriteSize {
							memFileData := memFileData{}
							end := current + chunkWriteSize
							if end > len(bbuf) {
								end = len(bbuf)
							}
							memFileData.buf = bbuf[current:end]
							multiPartFile.memFile.memFileDataChan <- memFileData
						}
						total += int64(len(bbuf))
					} else {
						err := zw.Close()
						if err != nil {
							multiPartFile.cancelC <- struct{}{}
							break
						}
						bbuf := make([]byte, buf.Len())
						_, err = buf.Read(bbuf)
						if err != nil {
							multiPartFile.memFile.errChan <- err
							return
						}
						current := 0
						for ; current < len(bbuf); current += chunkWriteSize {
							memFileData := memFileData{}
							end := current + chunkWriteSize
							if end >= len(bbuf) {
								end = len(bbuf)
								memFileData.err = io.EOF
							}
							memFileData.buf = bbuf[current:end]
							multiPartFile.memFile.memFileDataChan <- memFileData
						}
						close(multiPartFile.memFile.memFileDataChan)
						total += int64(len(bbuf))

						// Get final file hash
						multiPartFile.fileHasherLock.Lock()
						fileHash := hex.EncodeToString(multiPartFile.fileHasher.Sum(nil))
						multiPartFile.fileHasherLock.Unlock()
						log.Printf("uploaded: %d, duration: %v, file hash: %s", total, time.Since(st), fileHash)

						multiPartFile.fileSize = total
						return
					}
				}
			}
		}()
	} else {
		// Parallel chunking workers for non-compressed uploads
		var wg sync.WaitGroup
		var totalMutex sync.Mutex
		var total int64
		st := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		// Start multiple chunking workers that process data from dataC in parallel
		for i := 0; i < chunkingWorkers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-multiPartFile.cancelC:
						return
					case <-ctx.Done():
						log.Println("upload is timed out, clean up temp dirs")
						multiPartFile.cancelC <- struct{}{}
						memFile.errChan <- fmt.Errorf("upload is timed out")
						cleanupPartFilesAndDirs(bucket, uploadID, localStorageDir)
						return
					case data, ok := <-multiPartFile.dataC:
						if !ok {
							return
						}

						// Inline file hash (MD5) - thread-safe
						multiPartFile.fileHasherLock.Lock()
						multiPartFile.fileHasher.Write(data)
						multiPartFile.fileHasherLock.Unlock()

						// Chunk the data and send to memFileDataChan
						dataLen := len(data)
						current := 0
						for current < dataLen {
							memFileData := memFileData{}
							end := current + chunkWriteSize
							if end > dataLen {
								end = dataLen
							}
							// Make a copy of the chunk to avoid race conditions
							chunkBuf := make([]byte, end-current)
							copy(chunkBuf, data[current:end])
							memFileData.buf = chunkBuf
							multiPartFile.memFile.memFileDataChan <- memFileData
							current = end
						}

						totalMutex.Lock()
						total += int64(dataLen)
						totalMutex.Unlock()
					}
				}
			}()
		}

		// Monitor for completion and close channel when all workers are done
		go func() {
			wg.Wait()
			close(multiPartFile.memFile.memFileDataChan)

			// Get final file hash
			multiPartFile.fileHasherLock.Lock()
			fileHash := hex.EncodeToString(multiPartFile.fileHasher.Sum(nil))
			multiPartFile.fileHasherLock.Unlock()
			log.Printf("uploaded: %d, duration: %v, file hash: %s", total, time.Since(st), fileHash)
		}()
	}

	go func() {
		var customMeta string
		if len(userDefined) > 0 {
			meta, _ := json.Marshal(userDefined)
			customMeta = string(meta)
		}
		// Create fileMeta and sdk.OperationRequest
		fileMeta := sdk.FileMeta{
			RemoteName: filepath.Base(remotePath),
			RemotePath: remotePath,
			MimeType:   contentType,
			CustomMeta: customMeta,
		}
		options := []sdk.ChunkedUploadOption{
			sdk.WithChunkNumber(80),
			sdk.WithEncrypt(encrypt),
		}
		operationRequest := sdk.OperationRequest{
			FileMeta:      fileMeta,
			FileReader:    multiPartFile.memFile,
			OperationType: constants.FileOperationInsert,
			Opts:          options,
			RemotePath:    fileMeta.RemotePath,
			StreamUpload:  true,
			Workdir:       workDir,
		}
		// if its update change operation type
		if isUpdate {
			operationRequest.OperationType = constants.FileOperationUpdate
		}

		go func() {
			// run this in background, will block until the data is written to memFile
			uploadErr := zob.alloc.DoMultiOperation([]sdk.OperationRequest{operationRequest})
			if uploadErr != nil {
				cleanupPartFilesAndDirs(bucket, uploadID, localStorageDir)
			}
			multiPartFile.errorC <- uploadErr
		}()

		// Parallel part processing using worker pool (no seqPQ bottleneck)
		for {
			select {
			case <-multiPartFile.cancelC:
				log.Println("upload is canceled, clean up temp dirs")
				multiPartFile.memFile.errChan <- fmt.Errorf("upload is canceled")
				cleanupPartFilesAndDirs(bucket, uploadID, localStorageDir)
				return
			case partNumber, ok := <-multiPartFile.availableParts:
				if !ok {
					// Channel closed, all parts processed
					// Close orderedDataC to signal ordering goroutine to finish
					close(multiPartFile.orderedDataC)
					return
				}

				// Acquire worker from pool
				multiPartFile.workerPool <- struct{}{}
				go func(pn int) {
					defer func() { <-multiPartFile.workerPool }() // Release worker

					// Read part data from memory (no file I/O)
					multiPartFile.partsLock.Lock()
					partDataBytes, exists := multiPartFile.parts[pn]
					multiPartFile.partsLock.Unlock()

					if !exists {
						log.Printf("part %d not found in memory", pn)
						multiPartFile.cancelC <- struct{}{}
						return
					}

					// Make a copy to avoid race conditions
					data := make([]byte, len(partDataBytes))
					copy(data, partDataBytes)

					// Free memory for this part (keep ETag)
					multiPartFile.partsLock.Lock()
					delete(multiPartFile.parts, pn)
					multiPartFile.partsLock.Unlock()

					// Send to ordered channel (ordering goroutine will handle sequencing)
					multiPartFile.orderedDataC <- partData{
						partNumber: pn,
						data:       data,
					}
				}(partNumber)
			}
		}
	}()

	// Ordering goroutine: ensures parts are sent to dataC in correct order
	go func() {
		nextPart := 1
		partBuffer := make(map[int][]byte)

		for {
			select {
			case <-multiPartFile.cancelC:
				return
			case pd, ok := <-multiPartFile.orderedDataC:
				if !ok {
					// Channel closed, send any remaining buffered parts
					for i := nextPart; ; i++ {
						if data, exists := partBuffer[i]; exists {
							multiPartFile.dataC <- data
							delete(partBuffer, i)
						} else {
							break
						}
					}
					return
				}

				partBuffer[pd.partNumber] = pd.data

				// Send parts in order as they become available
				for {
					if data, exists := partBuffer[nextPart]; exists {
						multiPartFile.dataC <- data
						delete(partBuffer, nextPart)
						nextPart++
					} else {
						break
					}
				}
			}
		}
	}()

	return uploadID, nil

}

func (zob *zcnObjects) PutObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, data *minio.PutObjReader, opts minio.ObjectOptions) (pi minio.PartInfo, err error) {
	mapLock.Lock()
	multiPartFile, ok := FileMap[uploadID]
	mapLock.Unlock()
	if !ok {
		log.Printf("uploadID: %v not found\n", uploadID)
		return minio.PartInfo{}, fmt.Errorf("uploadID: %v not found", uploadID)
	}

	// Read all part data into memory (no temp file)
	buf := make([]byte, PartSize)
	partDataBuffer := &bytes.Buffer{}
	size, err := io.CopyBuffer(partDataBuffer, data.Reader, buf)
	if err != nil {
		log.Println(err)
		return minio.PartInfo{}, fmt.Errorf("error reading part data: %v", err)
	}

	// Store part data in memory
	partDataBytes := make([]byte, partDataBuffer.Len())
	copy(partDataBytes, partDataBuffer.Bytes())
	multiPartFile.partsLock.Lock()
	multiPartFile.parts[partID] = partDataBytes
	multiPartFile.partsLock.Unlock()

	// Signal part is ready for processing (replaces seqPQ.Push)
	// This allows immediate parallel processing without sequential bottleneck
	select {
	case multiPartFile.availableParts <- partID:
		// Part queued for processing
	default:
		// Channel full, but part is stored in memory and will be processed
		log.Printf("warning: availableParts channel full for part %d", partID)
	}

	// Calculate ETag for the part
	eTag := data.MD5CurrentHexString()

	// Store ETag in memory (no file I/O)
	multiPartFile.partsLock.Lock()
	multiPartFile.etags[partID] = eTag
	multiPartFile.partsLock.Unlock()

	multiPartFile.UpdateFileSize(partID, int64(size))

	return minio.PartInfo{
		PartNumber: partID,
		ETag:       eTag,
		Size:       int64(size),
		ActualSize: int64(size),
	}, nil
}

func (zob *zcnObjects) CompleteMultipartUpload(ctx context.Context, bucket, object, uploadID string, uploadedParts []minio.CompletePart, opts minio.ObjectOptions) (oi minio.ObjectInfo, err error) {

	mapLock.Lock()
	multiPartFile, ok := FileMap[uploadID]
	mapLock.Unlock()
	if !ok {
		log.Printf("uploadID: %v not found\n", uploadID)
		return minio.ObjectInfo{}, fmt.Errorf("uploadID: %v not found", uploadID)
	}

	// Signal that all parts have been received (for CompleteMultipartUpload)
	// This allows the parallel processing goroutine to finish when all parts are done
	multiPartFile.allPartsLock.Lock()
	multiPartFile.allPartsReceived = true
	multiPartFile.allPartsLock.Unlock()

	// Close availableParts channel to signal no more parts will arrive
	// This will cause the parallel processing goroutine to finish
	close(multiPartFile.availableParts)

	// wait for upload to finish
	err = <-multiPartFile.errorC
	if err != nil && !isSameRootError(err) {
		log.Println("Error uploading to Zus storage:", err)
		return minio.ObjectInfo{}, fmt.Errorf("error uploading to Zus storage: %v", err)
	}

	eTag, err := zob.constructCompleteObject(bucket, uploadID, object, localStorageDir)
	if err != nil {
		log.Println("Error constructing complete object:", err)
		return minio.ObjectInfo{}, fmt.Errorf("error constructing complete object: %v", err)
	}

	// Clear memory storage after successful upload
	multiPartFile.partsLock.Lock()
	multiPartFile.parts = make(map[int][]byte)
	multiPartFile.etags = make(map[int]string)
	multiPartFile.partsLock.Unlock()

	if err = cleanupPartFilesAndDirs(bucket, uploadID, localStorageDir); err != nil {
		log.Println("Error cleaning up part files and directories:", err)
		// http.Error(w, "Error cleaning up part files and directories", http.StatusInternalServerError)
		return minio.ObjectInfo{}, fmt.Errorf("error cleaning up part files and directories: %v", err)
	}
	log.Println("finish uploading: ", multiPartFile.fileSize, " name: ", object)
	return minio.ObjectInfo{
		Bucket:  bucket,
		Name:    object,
		ETag:    eTag,
		Size:    multiPartFile.fileSize,
		ModTime: time.Now(),
	}, nil
}

// Function to construct the complete object file
func (zob *zcnObjects) constructCompleteObject(bucket, uploadID, object, localStorageDir string) (string, error) {
	mapLock.Lock()
	multiPartFile, ok := FileMap[uploadID]
	mapLock.Unlock()
	if !ok {
		return "", fmt.Errorf("uploadID: %v not found", uploadID)
	}

	// Get ETags from memory (no file I/O)
	var partETags []string
	multiPartFile.partsLock.Lock()
	for partNumber := 1; ; partNumber++ {
		eTag, exists := multiPartFile.etags[partNumber]
		if !exists {
			break
		}
		partETags = append(partETags, eTag)
	}
	multiPartFile.partsLock.Unlock()

	// Get the concatenated ETag value
	eTag := strings.Join(partETags, "")

	return eTag, nil
}

// Function to clean up temporary part files and directories
func cleanupPartFilesAndDirs(bucket, uploadID, localStorageDir string) error {
	// Remove the upload directory
	uploadDir := filepath.Join(localStorageDir, bucket, uploadID)
	if err := os.RemoveAll(uploadDir); err != nil {
		return err
	}

	return nil
}

// GetMultipartInfo returns multipart info of the uploadId of the object
func (zob *zcnObjects) GetMultipartInfo(ctx context.Context, bucket, object, uploadID string, opts minio.ObjectOptions) (result minio.MultipartInfo, err error) {
	result.Bucket = bucket
	result.Object = object
	result.UploadID = uploadID
	return result, nil
}

func (zob *zcnObjects) ListObjectParts(ctx context.Context, bucket string, object string, uploadID string, partNumberMarker int, maxParts int, opts minio.ObjectOptions) (lpi minio.ListPartsInfo, err error) {
	mapLock.Lock()
	multiPartFile, ok := FileMap[uploadID]
	mapLock.Unlock()
	if !ok {
		return minio.ListPartsInfo{}, fmt.Errorf("uploadID: %v not found", uploadID)
	}

	// Read the parts information from memory (no file I/O)
	partsInfo := minio.ListPartsInfo{
		Object:   object,
		Bucket:   bucket,
		UploadID: uploadID,
	}

	multiPartFile.partsLock.Lock()
	defer multiPartFile.partsLock.Unlock()

	for i := partNumberMarker; i <= maxParts; i++ {
		eTag, eTagExists := multiPartFile.etags[i]
		partData, partExists := multiPartFile.parts[i]

		if !eTagExists && !partExists {
			// If the part does not exist, we have reached the end of the parts list
			break
		}

		var size int64
		if partExists {
			size = int64(len(partData))
		}

		// Append the part information to the parts list
		part := minio.PartInfo{
			PartNumber:   i,
			LastModified: time.Now(), // Use current time since we don't track mod time in memory
			ETag:         eTag,
			Size:         size,
			ActualSize:   size,
		}

		partsInfo.Parts = append(partsInfo.Parts, part)
	}

	return partsInfo, nil
}

func (zob *zcnObjects) AbortMultipartUpload(ctx context.Context, bucket string, object string, uploadID string, opts minio.ObjectOptions) error {
	log.Println("abort multipart upload, clean up memory")
	mapLock.Lock()
	multiPartFile, ok := FileMap[uploadID]
	mapLock.Unlock()
	if !ok {
		log.Printf("uploadID: %v not found\n", uploadID)
		return fmt.Errorf("abort - uploadID: %v not found", uploadID)
	}
	close(multiPartFile.cancelC)

	// Clear memory storage
	multiPartFile.partsLock.Lock()
	multiPartFile.parts = make(map[int][]byte)
	multiPartFile.etags = make(map[int]string)
	multiPartFile.partsLock.Unlock()

	return cleanupPartFilesAndDirs(bucket, uploadID, localStorageDir)
}
