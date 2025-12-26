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

	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/cmd/gateway/zcn/seqpriorityqueue"
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
const maxMemorySize = 1024 * 1024 * 1024 // 1GB

type MultiPartFile struct {
	memFile         *memFile
	lock            sync.Mutex
	fileSize        int64
	lastPartSize    int64
	lastPartID      int
	lastPartUpdated bool
	errorC          chan error
	seqPQ           *seqpriorityqueue.SeqPriorityQueue
	cancelC         chan struct{} // indicate the cancel of the uploading
	dataC           chan []byte   // data to be uploaded
	parts           map[int][]byte // in-memory storage for parts
	partsLock       sync.Mutex     // lock for parts map
	totalMemorySize int64          // total memory used by parts
	etags           map[int]string // in-memory storage for part ETags
	useMemory       bool           // flag to indicate if using memory storage
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
	multiPartFile := &MultiPartFile{
		memFile:         memFile,
		seqPQ:           seqpriorityqueue.NewSeqPriorityQueue(),
		errorC:           make(chan error, 1),
		dataC:            make(chan []byte, 20),
		cancelC:          make(chan struct{}, 1),
		parts:            make(map[int][]byte),
		totalMemorySize:  0,
		etags:            make(map[int]string),
		useMemory:        true, // default to memory storage
	}
	FileMap[uploadID] = multiPartFile
	mapLock.Unlock()
	// Create the bucket directory if it doesn't exist
	bucketPath := filepath.Join(localStorageDir, bucket, uploadID, object)
	if err := os.MkdirAll(bucketPath, os.ModePerm); err != nil {
		log.Println(err)
		return "", fmt.Errorf("erro creating bucket: %v", err)
	}

	go func() {
		buf := &bytes.Buffer{}
		var (
			zw    *lz4.Writer
			total int64
			err   error
		)
		if toCompress {
			zw = lz4.NewWriter(buf)
			zw.Apply(lz4.CompressionLevelOption(lz4.Level1)) //nolint:errcheck
		}
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
					if toCompress {
						_, err = zw.Write(data)
					} else {
						_, err = buf.Write(data)
					}
					if err != nil {
						log.Println("write data to buffer failed:", err)
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
					cn := len(bbuf)

					total += int64(cn)
				} else {
					if toCompress {
						err = zw.Close()
						if err != nil {
							multiPartFile.cancelC <- struct{}{}
							break
						}
					}
					bbuf := make([]byte, buf.Len())
					_, err := buf.Read(bbuf)
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
					cn := len(bbuf)
					close(multiPartFile.memFile.memFileDataChan)
					total += int64(cn)
					log.Println("uploaded:", total, " duration:", time.Since(st))
					if toCompress {
						multiPartFile.fileSize = total
					}
					return
				}
			}
		}
	}()

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

		for {
			select {
			case <-multiPartFile.cancelC:
				log.Println("upload is canceled, clean up temp dirs")
				multiPartFile.memFile.errChan <- fmt.Errorf("upload is canceled")
				// TODO: clean up temp dirs
				cleanupPartFilesAndDirs(bucket, uploadID, localStorageDir)
				return
			default:
				partNumber := multiPartFile.seqPQ.Popup()

				if partNumber == -1 {
					close(multiPartFile.dataC)
					return
				}

				func() {
					// Check if part is stored in memory
					multiPartFile.partsLock.Lock()
					partData, inMemory := multiPartFile.parts[partNumber]
					multiPartFile.partsLock.Unlock()

					if inMemory {
						// Read from memory
						// Make a copy to avoid race conditions
						data := make([]byte, len(partData))
						copy(data, partData)
						multiPartFile.dataC <- data
						// Free part data from memory after sending to channel (keep ETag for later use)
						multiPartFile.partsLock.Lock()
						if partSize, exists := multiPartFile.parts[partNumber]; exists {
							multiPartFile.totalMemorySize -= int64(len(partSize))
							delete(multiPartFile.parts, partNumber)
							// Keep ETag in memory for constructCompleteObject
						}
						multiPartFile.partsLock.Unlock()
					} else {
						// Read from file (fallback)
						partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", partNumber))
						partFile, err := os.Open(partFilename)
						if err != nil {
							log.Println("open error: ", err)
							multiPartFile.cancelC <- struct{}{}
							return
						}
						defer func() {
							partFile.Close()
							_ = os.Remove(partFilename)
						}()
						stat, err := partFile.Stat()
						if err != nil {
							log.Println("stat error: ", err)
							multiPartFile.cancelC <- struct{}{}
							return
						}
						data := make([]byte, stat.Size())
						_, err = io.ReadFull(partFile, data)
						if err != nil {
							log.Printf("read part: %v failed, err: %v\n", partNumber, err)
							multiPartFile.cancelC <- struct{}{}
							return
						}

						multiPartFile.dataC <- data
					}
				}()
			}
		}
	}()
	return uploadID, nil

}

func (zob *zcnObjects) PutObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, data *minio.PutObjReader, opts minio.ObjectOptions) (pi minio.PartInfo, err error) {
	partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", partID))
	partETagFilename := partFilename + ".etag"

	mapLock.Lock()
	multiPartFile, ok := FileMap[uploadID]
	mapLock.Unlock()
	if !ok {
		log.Printf("uploadID: %v not found\n", uploadID)
		return minio.PartInfo{}, fmt.Errorf("uploadID: %v not found", uploadID)
	}
	seqPQ := multiPartFile.seqPQ

	// Read all data into memory buffer
	var partData []byte
	var eTag string
	var size int64

	// Create MD5 hash for ETag calculation
	hash := md5.New()

	// Read data into buffer
	buf := make([]byte, PartSize)
	var totalRead int64
	for {
		n, err := data.Reader.Read(buf)
		if n > 0 {
			partData = append(partData, buf[:n]...)
			hash.Write(buf[:n])
			totalRead += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Println(err)
			return minio.PartInfo{}, fmt.Errorf("error reading part data: %v", err)
		}
	}
	size = totalRead

	// Calculate ETag
	eTag = hex.EncodeToString(hash.Sum(nil))

	// Check if we should use memory storage
	multiPartFile.partsLock.Lock()
	useMemory := multiPartFile.useMemory && (multiPartFile.totalMemorySize+size <= maxMemorySize)
	
	if useMemory {
		// Store in memory
		multiPartFile.parts[partID] = partData
		multiPartFile.etags[partID] = eTag
		multiPartFile.totalMemorySize += size
		multiPartFile.partsLock.Unlock()
		log.Printf("Stored part %d in memory (size: %d, total memory: %d)", partID, size, multiPartFile.totalMemorySize)
	} else {
		// Fall back to file storage
		multiPartFile.useMemory = false
		multiPartFile.partsLock.Unlock()
		
		// Create directory if it doesn't exist
		if err := os.MkdirAll(filepath.Dir(partFilename), os.ModePerm); err != nil {
			log.Println(err)
			return minio.PartInfo{}, fmt.Errorf("error creating directory: %v", err)
		}

		partFile, err := os.Create(partFilename)
		if err != nil {
			log.Println(err)
			return minio.PartInfo{}, fmt.Errorf("error creating part file: %v", err)
		}
		defer partFile.Close()

		_, err = partFile.Write(partData)
		if err != nil {
			log.Println(err)
			return minio.PartInfo{}, fmt.Errorf("error writing part data: %v", err)
		}

		// Save the ETag to a separate file
		if err := os.WriteFile(partETagFilename, []byte(eTag), 0644); err != nil {
			log.Println("error saving ETag file:", err)
			return minio.PartInfo{}, fmt.Errorf("error saving ETag file: %v", err)
		}
		log.Printf("Stored part %d in file (size: %d, exceeded memory limit)", partID, size)
	}

	seqPQ.Push(partID)
	multiPartFile.UpdateFileSize(partID, size)

	return minio.PartInfo{
		PartNumber: partID,
		ETag:       eTag,
		Size:       size,
		ActualSize: size,
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

	// wait for upload to finish
	multiPartFile.seqPQ.Done()
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

	// Clean up memory storage
	multiPartFile.partsLock.Lock()
	multiPartFile.parts = make(map[int][]byte)
	multiPartFile.etags = make(map[int]string)
	multiPartFile.totalMemorySize = 0
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
	// Create a slice to store individual part ETags
	var partETags []string

	mapLock.Lock()
	multiPartFile, ok := FileMap[uploadID]
	mapLock.Unlock()
	if !ok {
		return "", fmt.Errorf("uploadID: %v not found", uploadID)
	}

	for partNumber := 1; ; partNumber++ {
		// Check if ETag is stored in memory
		multiPartFile.partsLock.Lock()
		etag, inMemory := multiPartFile.etags[partNumber]
		multiPartFile.partsLock.Unlock()

		if inMemory {
			// Use ETag from memory
			partETags = append(partETags, etag)
		} else {
			// Try to read from file
			partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", partNumber))
			partETagFilename := partFilename + ".etag"

			// Break the loop when there are no more parts
			if _, err := os.Stat(partETagFilename); os.IsNotExist(err) {
				break
			}

			// Read the ETag of the part
			partETagBytes, err := os.ReadFile(partETagFilename)
			if err != nil {
				return "", err
			}

			// Append the part ETag to the slice
			partETags = append(partETags, string(partETagBytes))
		}
	}

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
	// Generate the path for the parts information file
	partsInfoPath := filepath.Join(localStorageDir, bucket, uploadID, object)

	// Check if the parts information file exists
	_, err = os.Stat(partsInfoPath)
	if err != nil {
		return minio.ListPartsInfo{}, fmt.Errorf("Unable to list object parts: %w", err)
	}

	// Read the parts information from the file (you may use your preferred method to read and parse the information)
	// For simplicity, we assume a simple JSON file here
	partsInfo := minio.ListPartsInfo{
		Object:   object,
		Bucket:   bucket,
		UploadID: uploadID,
	}

	for i := partNumberMarker; i <= maxParts; i++ {
		partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", i))
		// Read the ETag of the part
		partETagFilename := partFilename + ".etag"
		// Check if the part file exists
		fs, err := os.Stat(partETagFilename)
		if err != nil {
			// If the part file does not exist, we have reached the end of the parts list
			break
		}

		partETagBytes, err := os.ReadFile(partETagFilename)
		if err != nil {
			return minio.ListPartsInfo{}, fmt.Errorf("Unable to read part ETag: %w", err)
		}

		// Append the part information to the parts list
		part := minio.PartInfo{
			PartNumber:   i,
			LastModified: fs.ModTime(),
			ETag:         hex.EncodeToString(partETagBytes),
			Size:         fs.Size(),
			ActualSize:   fs.Size(),
		}

		partsInfo.Parts = append(partsInfo.Parts, part)
	}

	return partsInfo, nil
}

func (zob *zcnObjects) AbortMultipartUpload(ctx context.Context, bucket string, object string, uploadID string, opts minio.ObjectOptions) error {
	log.Println("abort multipart upload, clean up temp dirs")
	mapLock.Lock()
	multiPartFile, ok := FileMap[uploadID]
	mapLock.Unlock()
	if !ok {
		log.Printf("uploadID: %v not found\n", uploadID)
		return fmt.Errorf("abort - uploadID: %v not found", uploadID)
	}
	// Clear memory storage
	multiPartFile.partsLock.Lock()
	multiPartFile.parts = make(map[int][]byte)
	multiPartFile.etags = make(map[int]string)
	multiPartFile.totalMemorySize = 0
	multiPartFile.partsLock.Unlock()
	close(multiPartFile.cancelC)
	return cleanupPartFilesAndDirs(bucket, uploadID, localStorageDir)
}
