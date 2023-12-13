package main

import (
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
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	// Initialize the router
	router := mux.NewRouter()

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

		log.Println("initial upload...")

		// Generate a unique upload ID
		uploadID := uuid.New().String()

		// Create the bucket directory if it doesn't exist
		bucketPath := filepath.Join(localStorageDir, bucket, uploadID, object)
		log.Println("bucketPath:", bucketPath)
		if err := os.MkdirAll(bucketPath, os.ModePerm); err != nil {
			log.Println(err)
			http.Error(w, "Error creating bucket", http.StatusInternalServerError)
			return
		}

		// Create the upload directory
		// uploadDir := filepath.Join(bucketPath, fmt.Sprintf("%s_multipart_upload", object))
		// if err := os.MkdirAll(uploadDir, os.ModePerm); err != nil {
		// 	log.Println(err)
		// 	http.Error(w, "Error creating upload directory", http.StatusInternalServerError)
		// 	return
		// }

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
		const partSize = 8 * 1024 // Set an appropriate part size
		buf := make([]byte, partSize)

		// Create a unique filename for each part
		partNumberStr := r.URL.Query().Get("partNumber")
		partNumber, _ := strconv.Atoi(partNumberStr)
		partFilename := filepath.Join(localStorageDir, bucket, uploadID, object, fmt.Sprintf("part%d", partNumber))
		partETagFilename := partFilename + ".etag"

		log.Printf("make upload part %v, uploadID: %v, filename: %s", partNumber, uploadID, partFilename)
		// Create the part file
		partFile, err := os.Create(partFilename)
		if err != nil {
			log.Printf("create part file: %v failed, %v\n", partFilename, err)
			http.Error(w, "Error creating part file", http.StatusInternalServerError)
			return
		}
		defer partFile.Close()

		// Create an MD5 hash to calculate ETag
		hash := md5.New()

		// Read each part from the request body
		for {
			n, err := r.Body.Read(buf)
			if err == io.EOF {
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

			// Write the part data to the part file
			if _, err := partFile.Write(buf[:n]); err != nil {
				log.Println(err)
				http.Error(w, "Error writing part data", http.StatusInternalServerError)
				return
			}

			// Update the hash with the read data
			hash.Write(buf[:n])
		}

		// Calculate ETag for the part
		eTag := hex.EncodeToString(hash.Sum(nil))

		// Save the ETag to a separate file
		if err := ioutil.WriteFile(partETagFilename, []byte(eTag), 0644); err != nil {
			log.Println(err)
			http.Error(w, "Error saving ETag", http.StatusInternalServerError)
			return
		}

		log.Printf("Uploaded part %d for multipart upload with UploadID: %s, ETag: %s\n", partNumber, uploadID, eTag)

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

		log.Printf("Completed multipart upload for object %s with UploadID: %s, ETag: %s\n", object, uploadID, eTag)

		if err := cleanupPartFilesAndDirs(bucket, uploadID, object, localStorageDir); err != nil {
			log.Println("Error cleaning up part files and directories:", err)
			http.Error(w, "Error cleaning up part files and directories", http.StatusInternalServerError)
			return
		}

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
		if err := os.Remove(partFilename); err != nil {
			return "", err
		}

		// Remove the ETag file
		if err := os.Remove(partETagFilename); err != nil {
			return "", err
		}
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
