package zcn

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
)

type MultiPartFile struct {
	memFile *sys.MemChanFile
	opWg    sync.WaitGroup
	seqPQ   *seqpriorityqueue.SeqPriorityQueue
	doneC   chan struct{}
	dataC   chan []byte
}

func (zob *zcnObjects) NewMultipartUpload(ctx context.Context, bucket string, object string, opts minio.ObjectOptions) (uploadID string, err error) {
	log.Println("initial multipart upload, partNumber:", opts.PartNumber)
	return zob.newMultiPartUpload(localStorageDir, bucket, object)
}

func (zob *zcnObjects) newMultiPartUpload(localStorageDir, bucket, object string) (string, error) {
	var objectSize int64
	// objectSize := int64(371917281)
	// objectSize := int64(22491196)
	log.Println("initial upload...")

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
		memFile: memFile,
		seqPQ:   seqpriorityqueue.NewSeqPriorityQueue(),
		doneC:   make(chan struct{}),
		dataC:   make(chan []byte, 10),
	}
	FileMap[uploadID] = multiPartFile
	mapLock.Unlock()
	// Create the bucket directory if it doesn't exist
	bucketPath := filepath.Join(localStorageDir, bucket, uploadID, object)
	log.Println("bucketPath:", bucketPath)
	// Create fileMeta and sdk.OperationRequest
	fileMeta := sdk.FileMeta{
		ActualSize: objectSize, // Need to set the actual size
		RemoteName: object,
		RemotePath: "/" + filepath.Join(bucket, object),
		MimeType:   "application/octet-stream", // can get from request
	}
	options := []sdk.ChunkedUploadOption{
		sdk.WithChunkNumber(250),
	}
	operationRequest := sdk.OperationRequest{
		FileMeta:      fileMeta,
		FileReader:    memFile,
		OperationType: constants.FileOperationInsert,
		Opts:          options,
		RemotePath:    fileMeta.RemotePath,
	}
	// if its update change operation type
	multiPartFile.opWg.Add(1)
	go func() {
		// run this in background, will block until the data is written to memFile
		// We should add ctx here to cancel the operation
		_ = zob.alloc.DoMultiOperation([]sdk.OperationRequest{operationRequest}, multiPartFile.doneC)
		multiPartFile.opWg.Done()
	}()

	go func() {
		var buf bytes.Buffer
		var total int64
		for {
			select {
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
					// n, err := io.Copy(multiPartFile.memFile, partFile)
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
					log.Println("^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^ uploaded:", total, " new:", cn)
					return
				}
			}
		}
	}()

	go func() {
		for {
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
	}()
	if err := os.MkdirAll(bucketPath, os.ModePerm); err != nil {
		log.Println(err)
		return "", fmt.Errorf("erro creating bucket: %v", err)
	}

	return uploadID, nil
}

func (zob *zcnObjects) PutObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, data *minio.PutObjReader, opts minio.ObjectOptions) (pi minio.PartInfo, err error) {
	// func (zob *zcnObjects) PutObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, data *minio.PutObjReader, opts minio.ObjectOptions) (info minio.PartInfo, err error) {
	// Buffer to read each part
	var partSize = zob.alloc.GetChunkReadSize(false) // Set an appropriate part size set true if file is encrypted
	buf := make([]byte, partSize)

	// Create a unique filename for each part
	partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", partID))
	partETagFilename := partFilename + ".etag"

	// log.Printf("make upload part %v, uploadID: %v, filename: %s", partNumber, uploadID, partFilename)
	// Create the part file
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
	// partFile := multiPartFile.memFile
	seqPQ := multiPartFile.seqPQ
	// Create an MD5 hash to calculate ETag
	hash := md5.New()

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
		// http.Error(w, "Error saving ETag", http.StatusInternalServerError)
		// return
		return minio.PartInfo{}, fmt.Errorf("error saving ETag file: %v", err)
	}

	return minio.PartInfo{
		PartNumber: partID,
		ETag:       eTag,
		Size:       int64(size),
		ActualSize: int64(size),
	}, nil
}

func (zob *zcnObjects) CompleteMultipartUpload(ctx context.Context, bucket, object, uploadID string, uploadedParts []minio.CompletePart, opts minio.ObjectOptions) (oi minio.ObjectInfo, err error) {
	// func (zob *zcnObjects) CompleteMultipartUpload(ctx context.Context, bucket, object, uploadID string, uploadedParts []minio.CompletePart, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	// log.Println("complete total size:", gTotal)
	// Implement logic to calculate ETag based on uploaded parts
	eTag, err := constructCompleteObject(bucket, uploadID, object, localStorageDir)
	if err != nil {
		log.Println("Error constructing complete object:", err)
		return minio.ObjectInfo{}, fmt.Errorf("error constructing complete object: %v", err)
	}

	// wait for upload to finish
	mapLock.Lock()
	multiPartFile, ok := FileMap[uploadID]
	mapLock.Unlock()
	if !ok {
		log.Printf("uploadID: %v not found\n", uploadID)
		return minio.ObjectInfo{}, fmt.Errorf("uploadID: %v not found", uploadID)
	}

	multiPartFile.seqPQ.Done()
	<-multiPartFile.doneC
	multiPartFile.opWg.Wait()

	// TODO: do clean up after all has been uploaded to allocation
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
func constructCompleteObject(bucket, uploadID, object, localStorageDir string) (string, error) {
	// Create a temporary file to assemble the complete object
	tmpCompleteObjectFilename := filepath.Join(localStorageDir, bucket, uploadID, object+".tmp")
	tmpCompleteObjectFile, err := os.Create(tmpCompleteObjectFilename)
	if err != nil {
		return "", err
	}
	defer tmpCompleteObjectFile.Close()

	// Create a multi-writer to write to both the temporary file and calculate the ETag
	var allWriters []io.Writer
	allWriters = append(allWriters, tmpCompleteObjectFile)

	// Create a slice to store individual part ETags
	var partETags []string

	for partNumber := 1; ; partNumber++ {
		partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", partNumber))
		partETagFilename := partFilename + ".etag"

		// Break the loop when there are no more parts
		if _, err := os.Stat(partFilename); os.IsNotExist(err) {
			break
		}

		// Open the part file for reading
		partFile, err := os.Open(partFilename)
		if err != nil {
			return "", err
		}
		defer partFile.Close()

		// Copy the part data to both the temporary file and the hash
		if _, err := io.Copy(tmpCompleteObjectFile, partFile); err != nil {
			return "", err
		}

		// Read the ETag of the part
		partETagBytes, err := ioutil.ReadFile(partETagFilename)
		if err != nil {
			return "", err
		}

		// Append the part ETag to the slice
		partETags = append(partETags, string(partETagBytes))
	}

	// Get the concatenated ETag value
	eTag := strings.Join(partETags, "")

	// Close the temporary file
	if err := tmpCompleteObjectFile.Close(); err != nil {
		return "", err
	}

	// Rename the temporary file to its final destination
	completeObjectFilename := filepath.Join(localStorageDir, bucket, object)
	if err := os.Rename(tmpCompleteObjectFilename, completeObjectFilename); err != nil {
		return "", err
	}

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
	return cleanupPartFilesAndDirs(bucket, uploadID, object, localStorageDir)
}
