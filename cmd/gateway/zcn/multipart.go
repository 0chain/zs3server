package zcn

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0chain/gosdk/constants"
	"github.com/0chain/gosdk/core/sys"
	"github.com/0chain/gosdk/zboxcore/sdk"
	"github.com/google/uuid"

	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/cmd/gateway/zcn/seqpriorityqueue"
)

var (
	FileMap = make(map[string]*MultiPartFile)
	mapLock sync.Mutex
	// alloc   *sdk.Allocation
	localStorageDir = "store"
	moveTk          = newMoveTracker()
)

type MultiPartFile struct {
	memFile       *sys.MemChanFile
	lock          sync.Mutex
	fileSize      int64
	lastPartSize  int64
	lastPartID    int
	readyToUpload bool
	opWg          sync.WaitGroup
	seqPQ         *seqpriorityqueue.SeqPriorityQueue
	doneC         chan struct{} // indicates that the uploading is done
	readyUploadC  chan struct{} // indicates we are ready to start the uploading
	cancelC       chan struct{} // indicate the cancel of the uploading
	dataC         chan []byte   // data to be uploaded
}

func (mpf *MultiPartFile) UpdateFileSize(partID int, size int64) {
	mpf.lock.Lock()
	defer mpf.lock.Unlock()
	if mpf.readyToUpload {
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
		log.Println("see last part, partID:", partID, "file size:", mpf.fileSize)
		mpf.readyToUpload = true
		close(mpf.readyUploadC)
		mpf.lastPartSize = size
		return
	}

	if size == mpf.lastPartSize {
		mpf.fileSize += size
		return
	}

	// this is last part
	mpf.fileSize = int64(partID-1)*mpf.lastPartSize + size
	log.Println("see last part, partID:", partID, "file size:", mpf.fileSize)
	mpf.readyToUpload = true
	close(mpf.readyUploadC)
	mpf.readyUploadC = nil
}

func (mpf *MultiPartFile) notifyEnd() {
	mpf.lock.Lock()
	defer mpf.lock.Unlock()
	if !mpf.readyToUpload {
		mpf.readyToUpload = true
		if mpf.readyUploadC != nil {
			close(mpf.readyUploadC)
		}
	}
}

func (zob *zcnObjects) NewMultipartUpload(ctx context.Context, bucket string, object string, opts minio.ObjectOptions) (uploadID string, err error) {
	log.Println("initial multipart upload, partNumber:", opts.PartNumber)
	return zob.newMultiPartUpload(localStorageDir, bucket, object)
}

func (zob *zcnObjects) newMultiPartUpload(localStorageDir, bucket, object string) (string, error) {
	// var objectSize int64
	// objectSize := int64(371917281)
	// objectSize := int64(22491196)
	// log.Println("initial upload...")

	// Generate a unique upload ID
	uploadID := uuid.New().String()
	mapLock.Lock()
	memFile := &sys.MemChanFile{
		Buffer: make(chan []byte, 10),
		// false means encrypt is false
		ChunkWriteSize: int(zob.alloc.GetChunkReadSize(false)),
	}
	log.Println("ChunkReadSize:", memFile.ChunkWriteSize)
	multiPartFile := &MultiPartFile{
		memFile:      memFile,
		seqPQ:        seqpriorityqueue.NewSeqPriorityQueue(),
		doneC:        make(chan struct{}),
		dataC:        make(chan []byte, 100),
		readyUploadC: make(chan struct{}),
		cancelC:      make(chan struct{}),
	}
	FileMap[uploadID] = multiPartFile
	mapLock.Unlock()
	// Create the bucket directory if it doesn't exist
	bucketPath := filepath.Join(localStorageDir, bucket, uploadID, object)
	log.Println("bucketPath:", bucketPath)
	if err := os.MkdirAll(bucketPath, os.ModePerm); err != nil {
		log.Println(err)
		return "", fmt.Errorf("erro creating bucket: %v", err)
	}

	go func() {
		var buf bytes.Buffer
		var total int64
		st := time.Now()
		for {
			select {
			case <-multiPartFile.cancelC:
				log.Println("uploading is canceled, clean up temp dirs")
				// TODO: clean up temp dirs
				return
			case data, ok := <-multiPartFile.dataC:
				if ok {
					_, err := buf.Write(data)
					if err != nil {
						log.Panic(err)
					}

					n := buf.Len() / memFile.ChunkWriteSize
					bbuf := make([]byte, n*memFile.ChunkWriteSize)
					_, err = buf.Read(bbuf)
					if err != nil {
						log.Panic(err)
					}

					cn, err := io.Copy(multiPartFile.memFile, bytes.NewBuffer(bbuf))
					if err != nil {
						log.Panicf("upoad part failed, err: %v", err)
					}
					total += cn
					log.Println("^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^ uploaded:", total, " new:", cn)
				} else {
					cn, err := io.Copy(multiPartFile.memFile, &buf)
					if err != nil {
						log.Panic(err)
					}

					total += cn
					log.Println("^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^ uploaded:", total, " new:", cn, " duration:", time.Since(st))
					return
				}
			}
		}
	}()

	go func() {
		select {
		case <-multiPartFile.readyUploadC:
			log.Println("ready to upload to Zus storage...")
		case <-multiPartFile.cancelC:
			log.Println("uploading is canceled, clean up temp dirs")
			// TODO: clean up temp dirs
			return
		}
		// Create fileMeta and sdk.OperationRequest
		fileMeta := sdk.FileMeta{
			ActualSize: multiPartFile.fileSize, // Need to set the actual size
			RemoteName: object,
			RemotePath: "/" + filepath.Join(bucket, object),
			MimeType:   "application/octet-stream", // can get from request
		}
		options := []sdk.ChunkedUploadOption{
			sdk.WithChunkNumber(250),
		}
		operationRequest := sdk.OperationRequest{
			FileMeta:      fileMeta,
			FileReader:    multiPartFile.memFile,
			OperationType: constants.FileOperationInsert,
			Opts:          options,
			RemotePath:    fileMeta.RemotePath,
		}
		// if its update change operation type
		multiPartFile.opWg.Add(1)
		go func() {
			// run this in background, will block until the data is written to memFile
			// We should add ctx here to cancel the operation
			_ = zob.alloc.DoMultiOperation([]sdk.OperationRequest{operationRequest})
			multiPartFile.opWg.Done()
		}()

		for {
			select {
			case <-multiPartFile.cancelC:
				log.Println("uploading is canceled, clean up temp dirs")
				// TODO: clean up temp dirs
				return
			default:
				partNumber := multiPartFile.seqPQ.Popup()
				log.Println("==================================== popup part:", partNumber)

				if partNumber == -1 {
					close(multiPartFile.dataC)
					close(multiPartFile.doneC)
					log.Println("==================================== popup done")
					return
				}

				partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", partNumber))

				func() {
					// Open the part file for reading
					partFile, err := os.Open(partFilename)
					if err != nil {
						log.Panicf("could not open part file: %v, err: %v", partFilename, err)
					}
					defer partFile.Close()

					data, err := io.ReadAll(partFile)
					if err != nil {
						log.Panicf("read part: %v failed, err: %v", partNumber, err)
					}

					multiPartFile.dataC <- data
					log.Println("^^^^^^^^^ uploading part:", partNumber, "size:", len(data))
				}()
			}
		}
	}()
	return uploadID, nil

}

func (zob *zcnObjects) PutObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, data *minio.PutObjReader, opts minio.ObjectOptions) (pi minio.PartInfo, err error) {
	// func (zob *zcnObjects) PutObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, data *minio.PutObjReader, opts minio.ObjectOptions) (info minio.PartInfo, err error) {
	// Buffer to read each part

	// Create a unique filename for each part
	partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", partID))
	partETagFilename := partFilename + ".etag"

	partFile, err := os.Create(partFilename)
	if err != nil {
		log.Println(err)
		return minio.PartInfo{}, fmt.Errorf("error creating part file: %v", err)
	}
	defer partFile.Close()

	mapLock.Lock()
	multiPartFile, ok := FileMap[uploadID]
	mapLock.Unlock()
	if !ok {
		log.Printf("uploadID: %v not found\n", uploadID)
		return minio.PartInfo{}, fmt.Errorf("uploadID: %v not found", uploadID)
	}
	seqPQ := multiPartFile.seqPQ
	// Create an MD5 hash to calculate ETag
	hash := md5.New()

	var partSize = zob.alloc.GetChunkReadSize(false) // Set an appropriate part size set true if file is encrypted
	buf := make([]byte, partSize)
	// Read each part from the request body
	// We need to make sure we write atleast ChunkWriteSize bytes to memFile unless its the last part
	var size int
	for {
		n, err := data.Reader.Read(buf)
		if err == io.EOF {
			size += n
			// Write the part data to the part file
			if _, err := partFile.Write(buf[:n]); err != nil {
				log.Println(err)
				return minio.PartInfo{}, fmt.Errorf("error writing part data: %v", err)
			}

			// Update the hash with the read data
			hash.Write(buf[:n])
			break // End of file
		} else if err != nil {
			log.Println(err)
			return minio.PartInfo{}, fmt.Errorf("error reading part data: %v", err)
		}
		size += n

		// Write the part data to the part file
		if _, err := partFile.Write(buf[:n]); err != nil {
			log.Println(err)
			return minio.PartInfo{}, fmt.Errorf("error writing part data: %v", err)
		}

		// Update the hash with the read data
		hash.Write(buf[:n])
	}

	seqPQ.Push(partID)
	log.Println("VVVVVVVVVVVVVV pushed part:", partID)

	// Calculate ETag for the part
	eTag := hex.EncodeToString(hash.Sum(nil))

	// Save the ETag to a separate file
	if err := os.WriteFile(partETagFilename, []byte(eTag), 0644); err != nil {
		log.Println("error saving ETag file:", err)
		return minio.PartInfo{}, fmt.Errorf("error saving ETag file: %v", err)
	}

	multiPartFile.UpdateFileSize(partID, int64(size))

	return minio.PartInfo{
		PartNumber: partID,
		ETag:       eTag,
		Size:       int64(size),
		ActualSize: int64(size),
	}, nil
}

func (zob *zcnObjects) CompleteMultipartUpload(ctx context.Context, bucket, object, uploadID string, uploadedParts []minio.CompletePart, opts minio.ObjectOptions) (oi minio.ObjectInfo, err error) {
	// var objectSize int64
	// objectSize := int64(371917281)
	// objectSize := int64(22491196)

	mapLock.Lock()
	multiPartFile, ok := FileMap[uploadID]
	mapLock.Unlock()
	if !ok {
		log.Printf("uploadID: %v not found\n", uploadID)
		return minio.ObjectInfo{}, fmt.Errorf("uploadID: %v not found", uploadID)
	}
	multiPartFile.notifyEnd()

	// wait for upload to finish
	multiPartFile.seqPQ.Done()
	<-multiPartFile.doneC
	multiPartFile.opWg.Wait()
	log.Println("finish uploading!!")

	eTag, err := zob.constructCompleteObject(bucket, uploadID, object, localStorageDir, multiPartFile)
	if err != nil {
		log.Println("Error constructing complete object:", err)
		return minio.ObjectInfo{}, fmt.Errorf("error constructing complete object: %v", err)
	}

	if err = cleanupPartFilesAndDirs(bucket, uploadID, object, localStorageDir); err != nil {
		log.Println("Error cleaning up part files and directories:", err)
		// http.Error(w, "Error cleaning up part files and directories", http.StatusInternalServerError)
		return minio.ObjectInfo{}, fmt.Errorf("error cleaning up part files and directories: %v", err)
	}

	return minio.ObjectInfo{
		Bucket: bucket,
		Name:   object,
		ETag:   eTag,
	}, nil
}

// Function to construct the complete object file
func (zob *zcnObjects) constructCompleteObject(bucket, uploadID, object, localStorageDir string, multiPartFile *MultiPartFile) (string, error) {
	// Create a slice to store individual part ETags
	var partETags []string
	for partNumber := 1; ; partNumber++ {
		partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", partNumber))
		partETagFilename := partFilename + ".etag"

		// Break the loop when there are no more parts
		if _, err := os.Stat(partFilename); os.IsNotExist(err) {
			break
		}

		// func() {
		// 	// Open the part file for reading
		// 	partFile, err := os.Open(partFilename)
		// 	if err != nil {
		// 		log.Panicf("could not open part file: %v, err: %v", partFilename, err)
		// 	}
		// 	defer partFile.Close()

		// 	data, err := io.ReadAll(partFile)
		// 	if err != nil {
		// 		log.Panicf("read part: %v failed, err: %v", partNumber, err)
		// 	}

		// 	multiPartFile.dataC <- data
		// 	log.Println("^^^^^^^^^ uploading part:", partNumber, "size:", len(data))
		// }()

		// Read the ETag of the part
		partETagBytes, err := os.ReadFile(partETagFilename)
		if err != nil {
			return "", err
		}

		// Append the part ETag to the slice
		partETags = append(partETags, string(partETagBytes))
	}

	// Get the concatenated ETag value
	eTag := strings.Join(partETags, "")

	// Close the temporary file
	// if err := tmpCompleteObjectFile.Close(); err != nil {
	// 	return "", err
	// }

	// Rename the temporary file to its final destination
	// completeObjectFilename := filepath.Join(localStorageDir, bucket, object)
	// if err := os.Rename(tmpCompleteObjectFilename, completeObjectFilename); err != nil {
	// 	return "", err
	// }

	return eTag, nil
}

// Function to clean up temporary part files and directories
func cleanupPartFilesAndDirs(bucket, uploadID, object, localStorageDir string) error {
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
		// Check if the part file exists
		fs, err := os.Stat(partFilename)
		if err != nil {
			// If the part file does not exist, we have reached the end of the parts list
			break
		}

		// Read the ETag of the part
		partETagFilename := partFilename + ".etag"
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
	close(multiPartFile.cancelC)
	return cleanupPartFilesAndDirs(bucket, uploadID, object, localStorageDir)
}

type moveTracker struct {
	mu      sync.Mutex
	uploads map[string]*sync.Once
}

func newMoveTracker() *moveTracker {
	return &moveTracker{
		uploads: make(map[string]*sync.Once),
	}
}

func (mt *moveTracker) do(uploadID string, f func()) {
	mt.mu.Lock()
	once, ok := mt.uploads[uploadID]
	if !ok {
		once = &sync.Once{}
		mt.uploads[uploadID] = once
	}
	mt.mu.Unlock()

	once.Do(f)
}

func (mt *moveTracker) remove(uploadID string) {
	mt.mu.Lock()
	delete(mt.uploads, uploadID)
	mt.mu.Unlock()
}

func (zob *zcnObjects) CopyObjectPart(ctx context.Context, srcBucket, srcObject, destBucket, destObject, uploadID string, partID int, startOffset, length int64, srcInfo minio.ObjectInfo, srcOpts, dstOpts minio.ObjectOptions) (pi minio.PartInfo, err error) {
	// Check if the source bucket exists
	if _, err = zob.GetBucketInfo(ctx, srcBucket); err != nil {
		return pi, err
	}

	// Check if the destination bucket exists
	if _, err = zob.GetBucketInfo(ctx, destBucket); err != nil {
		return pi, err
	}

	// Check if the source object exists
	if _, err = zob.GetObjectInfo(ctx, srcBucket, srcObject, minio.ObjectOptions{}); err != nil {
		return pi, err
	}

	// Get or create the sync.Once for this uploadID
	moveTk.do(uploadID, func() {
		_, err = zob.moveZusObject(srcBucket, srcObject, destBucket, destObject)
	})

	if err != nil {
		return pi, err
	}

	// Mock the part copy action
	pi = minio.PartInfo{
		PartNumber:   partID,
		LastModified: srcInfo.ModTime,
		ETag:         srcInfo.ETag,
		Size:         srcInfo.Size,
	}

	return pi, nil
}
