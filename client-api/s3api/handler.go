package s3api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
)

var ENDPOINT = os.Getenv("MINIO_SERVER")
var USESSL = false

type MinioCredentials struct {
	AccessKey       string
	SecretAccessKey string
}

func Handler(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	minioCredentials := MinioCredentials{}
	minioCredentials.AccessKey = r.URL.Query().Get("accessKey")
	minioCredentials.SecretAccessKey = r.URL.Query().Get("secretAccessKey")
	switch action {
	case "createBucket":
		bucketName := r.URL.Query().Get("bucketName")
		location := r.URL.Query().Get("location")
		CreateBucketResponse, err := createBucket(bucketName, location, minioCredentials)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, CreateBucketResponse)

	case "listBuckets":
		buckets, err := listBucket(minioCredentials)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, buckets)

	case "listBucketsObjects":
		bucketObjectList, err := listBucketsObjects(minioCredentials)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, bucketObjectList)

	case "listObjects":
		bucketName := r.URL.Query().Get("bucketName")
		bucketOjects, err := listobjects(bucketName, minioCredentials)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, bucketOjects)

	case "getObject":
		bucketName := r.URL.Query().Get("bucketName")
		objectName := r.URL.Query().Get("objectName")
		getObjectResponse, err := getObject(bucketName, objectName, minioCredentials)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(objectName))
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, getObjectResponse.ObjectPath)
		JSON(w, 200, getObjectResponse)

	case "putObject":
		bucketName := r.URL.Query().Get("bucketName")
		file, header, err := r.FormFile("file")

		if err != nil {
			log.Println("[-] Error in r.FormFile ", err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "{'error': %s}", err)
			return
		}
		defer file.Close()

		out, err := os.Create("/tmp/" + header.Filename)
		if err != nil {
			log.Println("[-] Unable to create the file for writing. Check your write access privilege.", err)
			fmt.Fprintf(w, "[-] Unable to create the file for writing. Check your write access privilege.", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
		defer out.Close()

		// write the content from POST to the file
		_, err = io.Copy(out, file)
		if err != nil {
			log.Println("[-] Error copying file.", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		putObjectResponse, err := putObject(bucketName, header.Filename, minioCredentials)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, putObjectResponse)

	case "removeObject":
		bucketName := r.URL.Query().Get("bucketName")
		objectName := r.URL.Query().Get("objectName")
		removeObjectResponse, err := removeObject(bucketName, objectName, minioCredentials)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, removeObjectResponse)

	default:
		JSON(w, 500, map[string]string{"message": "feature not avaliable"})
	}

}

func JSON(w http.ResponseWriter, code int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		//log.Fatalln(err)
		w.WriteHeader(500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(response)
}
