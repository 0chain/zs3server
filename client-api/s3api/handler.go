package s3api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
)

var ENDPOINT = os.Getenv("MINIO_SERVER")
var USESSL = false

type MinioCredentials struct {
	AccessKey       string `json:"accessKey"`
	SecretAccessKey string `json:"secretAccessKey"`
}

func Handler(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	minioCredentials := MinioCredentials{}
	if err := json.NewDecoder(r.Body).Decode(&minioCredentials); err != nil {
		fmt.Println(err)
		JSON(w, 500, nil)
		return
	}
	switch action {
	case "createBucket":
		bucketName := r.URL.Query().Get("bucketName")
		location := r.URL.Query().Get("location")
		createBucket(bucketName, location, minioCredentials)
		JSON(w, 200, map[string]string{"Status": "succuess"})
	case "listBuckets":
		buckets, err := listBucket(minioCredentials)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, buckets)
	/*
	   list all the buckets and their objects
	*/
	case "listbucketsObjest":
		bucketObjectList, err := listBucketsObjects(minioCredentials)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, bucketObjectList)
	/*
		list all the objects for particular bucket
		it requires bucketName
		{endpoint}?action=listObjects&bucketName={$bucketname}
	*/
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
		JSON(w, 200, getObjectResponse)

	case "putObject":
		bucketName := r.URL.Query().Get("bucketName")
		ObjectName := r.URL.Query().Get("objectName")
		// r.ParseMultipartForm(32 << 20)
		// file, handler, err := r.FormFile("uploadfile")
		// if err != nil {
		// 	fmt.Println(err)
		// 	return
		// }
		putObjectResponse, err := putObject(bucketName, ObjectName, minioCredentials)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, putObjectResponse)
	default:
		JSON(w, 500, map[string]string{"message": "feature not avaliable"})
	}

}

func JSON(w http.ResponseWriter, code int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		log.Fatalln(err)
		w.WriteHeader(500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(response)
}
