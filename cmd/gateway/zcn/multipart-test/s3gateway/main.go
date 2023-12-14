package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0chain/gosdk/constants"
	"github.com/0chain/gosdk/core/sys"
	"github.com/0chain/gosdk/zboxcore/sdk"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/peterlimg/s3gateway/seqpriorityqueue"
	"github.com/peterlimg/s3gateway/zcn"
)

var (
	FileMap = make(map[string]*MultiPartFile)
	mapLock sync.Mutex
	alloc   *sdk.Allocation
)

type MultiPartFile struct {
	memFile *sys.MemChanFile
	opWg    sync.WaitGroup
	seqPQ   *seqpriorityqueue.SeqPriorityQueue
	doneC   chan struct{}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	// Initialize the router
	router := mux.NewRouter()

	err := zcn.InitializeSDK("", "", 1)
	if err != nil {
		panic(err)
	}

	alloc, err = sdk.GetAllocation(zcn.AllocationID)
	if err != nil {
		panic(err)
	}

	localStorageDir := "./store"

	// Define your S3 API endpoints
	router.HandleFunc("/{bucket}/{object:.*}", makeS3Handler).Methods(http.MethodGet)
	// router.HandleFunc("/{bucket}/{object:.*}", makePutObjectHandler).Methods(http.MethodPut)
	// router.HandleFunc("/{bucket}/{object:.*}", makeMultipartUploadHandler(".")).Methods(http.MethodPost)
	router.HandleFunc("/{bucket}", makeListObjectsHandler).Methods(http.MethodGet)

	// Define your S3 API endpoints
	router.HandleFunc("/{bucket}/{object:.+}", makeInitiateMultipartUploadHandler(localStorageDir)).Methods(http.MethodPost).Queries("uploads", "")
	router.HandleFunc("/{bucket}/{object:.+}", makeUploadPartHandler(localStorageDir)).Methods(http.MethodPut).Queries("uploadId", "{uploadId:.*}")

	router.HandleFunc("/{bucket}/{object:.+}", makeCompleteMultipartUploadHandler(localStorageDir)).Methods(http.MethodPost).Queries("uploadId", "{uploadId:.*}")

	// Start the server
	log.Println("S3 Gateway listening on :8080")
	http.Handle("/", router)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func makeS3Handler(w http.ResponseWriter, r *http.Request) {
	// Implement your logic for handling GET requests here
}

func makePutObjectHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	bucket := vars["bucket"]
	object := vars["object"]

	// Create the bucket if it doesn't exist
	err := os.MkdirAll(bucket, os.ModePerm)
	if err != nil {
		log.Println(err)
		http.Error(w, "Error creating bucket", http.StatusInternalServerError)
		return
	}

	// Get the data from the request body
	data, err := io.ReadAll(r.Body)
	if err != nil {
		log.Println(err)
		http.Error(w, "Error reading request body", http.StatusInternalServerError)
		return
	}

	// Specify the directory where you want to save the object
	objectPath := filepath.Join(bucket, object)

	// Write the data to a local file
	err = os.WriteFile(objectPath, data, os.ModePerm)
	if err != nil {
		log.Printf("write file to: %v failed, err: %v\n", objectPath, err)
		http.Error(w, "Error saving object", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func makeListObjectsHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	bucket := vars["bucket"]

	// Specify the directory for the bucket
	bucketPath := bucket

	// Get the list of objects in the specified bucket directory
	objects, err := listObjects(bucketPath)
	if err != nil {
		log.Println(err)
		http.Error(w, "Error listing objects", http.StatusInternalServerError)
		return
	}

	// Return the list of objects as a response in XML format
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
	<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
		<Name>` + bucket + `</Name>
		<Prefix></Prefix>
		<Marker></Marker>
		<MaxKeys>1000</MaxKeys>
		<IsTruncated>false</IsTruncated>
		`))

	for _, object := range objects {
		// Get the last modified time of the file
		lastModified, err := getLastModifiedTime(filepath.Join(bucketPath, object))
		if err != nil {
			log.Println(err)
			continue
		}

		w.Write([]byte(fmt.Sprintf(`<Contents>
			<Key>%s</Key>
			<LastModified>%s</LastModified>
			<Size>%d</Size>
			<StorageClass>STANDARD</StorageClass>
		</Contents>`, object, lastModified.Format(time.RFC3339), getFileSize(filepath.Join(bucketPath, object)))))
	}

	w.Write([]byte(`</ListBucketResult>`))
}

func listObjects(directory string) ([]string, error) {
	var objects []string

	err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Extract the object key relative to the bucket
		objectKey := strings.TrimPrefix(path, directory+"/")

		objects = append(objects, objectKey)
		return nil
	})

	return objects, err
}

func getFileSize(filePath string) int64 {
	file, err := os.Stat(filePath)
	if err != nil {
		return 0
	}
	return file.Size()
}

func getLastModifiedTime(filePath string) (time.Time, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return time.Time{}, err
	}
	return fileInfo.ModTime(), nil
}

func makeInitiateMultipartUploadHandler(localStorageDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		bucket := vars["bucket"]
		object := vars["object"]
		// var objectSize int64
		objectSize := int64(371917281)
		// objectSize := int64(22491196)
		log.Println("initial upload...")

		// Generate a unique upload ID
		uploadID := uuid.New().String()
		mapLock.Lock()
		memFile := &sys.MemChanFile{
			Buffer: make(chan []byte, 10),
			// false means encrypt is false
			ChunkWriteSize: int(alloc.GetChunkReadSize(false)),
		}
		log.Println("ChunkReadSize:", memFile.ChunkWriteSize)
		multiPartFile := &MultiPartFile{
			memFile: memFile,
			seqPQ:   seqpriorityqueue.NewSeqPriorityQueue(),
			doneC:   make(chan struct{}),
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
			RemotePath: "/" + bucket,
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
			RemotePath:    bucketPath,
		}
		// if its update change operation type
		multiPartFile.opWg.Add(1)
		go func() {
			// run this in background, will block until the data is written to memFile
			// We should add ctx here to cancel the operation
			_ = alloc.DoMultiOperation([]sdk.OperationRequest{operationRequest})
			multiPartFile.opWg.Done()
		}()

		go func() {
			for {
				partNumber := multiPartFile.seqPQ.Popup()
				log.Println("==================================== popup part:", partNumber)

				if partNumber == -1 {
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

					data, err := ioutil.ReadAll(partFile)
					if err != nil {
						log.Panicf("read part: %v failed, err: %v", partNumber, err)
					}

					log.Println("^^^^^^^^^ uploading part:", partNumber, "size:", len(data))

					n, err := io.Copy(multiPartFile.memFile, bytes.NewBuffer(data))
					// n, err := io.Copy(multiPartFile.memFile, partFile)
					if err != nil {
						log.Panicf("upoad part: %v failed, err: %v", partNumber, err)
					}

					log.Println("^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^ uploaded part:", partNumber, " size:", n)
				}()

				log.Println("==================================== popup part:", partNumber)
				// if partNumber == -1 {
				// 	close(multiPartFile.doneC)
				// 	log.Println("==================================== popup done")
				// 	return
				// }
			}
		}()
		if err := os.MkdirAll(bucketPath, os.ModePerm); err != nil {
			log.Println(err)
			http.Error(w, "Error creating bucket", http.StatusInternalServerError)
			return
		}

		response := InitiateMultipartUploadResponse{UploadID: uploadID}

		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)

		// Marshal the response to XML and write it to the response writer
		xmlResponse, err := xml.MarshalIndent(response, "", "  ")
		if err != nil {
			log.Println(err)
			http.Error(w, "Error creating XML response", http.StatusInternalServerError)
			return
		}

		w.Write(xmlResponse)

		log.Printf("Initiating multipart upload for object %s with UploadID: %s\n", object, uploadID)
		// w.Write([]byte(uploadID))
	}
}

// InitiateMultipartUploadResponse represents the XML response for initiating a multipart upload
type InitiateMultipartUploadResponse struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	UploadID string   `xml:"UploadId"`
}

var gTotal int

func makeUploadPartHandler(localStorageDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		bucket := vars["bucket"]
		object := vars["object"]
		uploadID := r.URL.Query().Get("uploadId")

		// Buffer to read each part
		var partSize = alloc.GetChunkReadSize(false) // Set an appropriate part size set true if file is encrypted
		buf := make([]byte, partSize)
		// buf := make([]byte, 8*1024)

		// Create a unique filename for each part
		partNumberStr := r.URL.Query().Get("partNumber")
		partNumber, _ := strconv.Atoi(partNumberStr)
		partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", partNumber))
		partETagFilename := partFilename + ".etag"

		// log.Printf("make upload part %v, uploadID: %v, filename: %s", partNumber, uploadID, partFilename)
		// Create the part file
		partFile, err := os.Create(partFilename)
		if err != nil {
			log.Printf("create part file: %v failed, %v\n", partFilename, err)
			http.Error(w, "Error creating part file", http.StatusInternalServerError)
			return
		}
		defer partFile.Close()

		mapLock.Lock()
		multiPartFile, ok := FileMap[uploadID]
		mapLock.Unlock()
		if !ok {
			log.Printf("uploadID: %v not found\n", uploadID)
			http.Error(w, "uploadID not found", http.StatusInternalServerError)
			return
		}
		// partFile := multiPartFile.memFile
		seqPQ := multiPartFile.seqPQ
		// Create an MD5 hash to calculate ETag
		hash := md5.New()

		// Read each part from the request body
		// We need to make sure we write atleast ChunkWriteSize bytes to memFile unless its the last part
		for {
			n, err := r.Body.Read(buf)
			if err == io.EOF {
				gTotal += n
				// Write the part data to the part file
				if _, err := partFile.Write(buf[:n]); err != nil {
					log.Println(err)
					http.Error(w, "Error writing part data", http.StatusInternalServerError)
					return
				}

				// Update the hash with the read data
				hash.Write(buf[:n])
				break // End of file
			} else if err != nil {
				log.Println(err)
				http.Error(w, "Error reading part data", http.StatusInternalServerError)
				return
			}
			gTotal += n

			// Write the part data to the part file
			if _, err := partFile.Write(buf[:n]); err != nil {
				log.Println(err)
				http.Error(w, "Error writing part data", http.StatusInternalServerError)
				return
			}

			// Update the hash with the read data
			hash.Write(buf[:n])
		}

		seqPQ.Push(partNumber)
		log.Println("VVVVVVVVVVVVVV pushed part:", partNumber)

		// Calculate ETag for the part
		eTag := hex.EncodeToString(hash.Sum(nil))

		// Save the ETag to a separate file
		if err := ioutil.WriteFile(partETagFilename, []byte(eTag), 0644); err != nil {
			log.Println(err)
			http.Error(w, "Error saving ETag", http.StatusInternalServerError)
			return
		}

		// log.Printf("Uploaded part %d for multipart upload with UploadID: %s, ETag: %s\n", partNumber, uploadID, eTag)

		// Include the ETag in the response header
		w.Header().Set("ETag", eTag)
		w.WriteHeader(http.StatusOK)
	}
}

func makeCompleteMultipartUploadHandler(localStorageDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Println("complete total size:", gTotal)
		vars := mux.Vars(r)
		bucket := vars["bucket"]
		object := vars["object"]
		uploadID := r.URL.Query().Get("uploadId")

		// Implement logic to calculate ETag based on uploaded parts
		eTag, err := constructCompleteObject(bucket, uploadID, object, localStorageDir)
		if err != nil {
			log.Println("Error constructing complete object:", err)
			http.Error(w, "Error constructing complete object", http.StatusInternalServerError)
			return
		}

		// Include the correct ETag in the response header
		w.Header().Set("ETag", eTag)

		// log.Printf("Completed multipart upload for object %s with UploadID: %s, ETag: %s\n", object, uploadID, eTag)

		// wait for upload to finish
		mapLock.Lock()
		multiPartFile, ok := FileMap[uploadID]
		mapLock.Unlock()
		if !ok {
			log.Printf("uploadID: %v not found\n", uploadID)
			http.Error(w, "uploadID not found", http.StatusInternalServerError)
			return
		}
		multiPartFile.seqPQ.Done()
		<-multiPartFile.doneC
		multiPartFile.opWg.Wait()

		// TODO: do clean up after all has been uploaded to allocation
		// if err := cleanupPartFilesAndDirs(bucket, uploadID, object, localStorageDir); err != nil {
		// 	log.Println("Error cleaning up part files and directories:", err)
		// 	http.Error(w, "Error cleaning up part files and directories", http.StatusInternalServerError)
		// 	return
		// }
		// Build the XML response
		responseXML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<Location>https//%s.localhost:8080/%s</Location>
  <Bucket>%s</Bucket>
  <Key>%s</Key>
  <ETag>"%s"</ETag>
</CompleteMultipartUploadResult>`, bucket, object, bucket, object, eTag)

		// Set the Content-Type header to indicate XML response
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		// Write the XML response to the client
		w.Write([]byte(responseXML))
	}
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

		// Remove the part file
		// if err := os.Remove(partFilename); err != nil {
		// 	return "", err
		// }

		// Remove the ETag file
		// if err := os.Remove(partETagFilename); err != nil {
		// 	return "", err
		// }
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
	for partNumber := 1; ; partNumber++ {
		partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", partNumber))
		partETagFilename := partFilename + ".etag"

		// Break the loop when there are no more parts
		if _, err := os.Stat(partFilename); os.IsNotExist(err) {
			break
		}

		// Remove the part file
		if err := os.Remove(partFilename); err != nil {
			return err
		}

		// Remove the ETag file
		if err := os.Remove(partETagFilename); err != nil {
			return err
		}
	}

	// Remove the upload directory
	uploadDir := filepath.Join(localStorageDir, bucket, uploadID)
	if err := os.RemoveAll(uploadDir); err != nil {
		return err
	}

	return nil
}
