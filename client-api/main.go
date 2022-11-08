package main

import (
	"log"
	"net/http"

	"github.com/minio-sdk/s3api"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s3api.Handler)
	err := http.ListenAndServe(":3001", mux)
	if err != nil {
		log.Fatalln(err)
	}

	// // Upload the zip file
	// objectName := "test.txt"
	// filePath := "/tmp/test.txt"
	// contentType := "application/txt"

	// // Upload the zip file with FPutObject
	// info, err := minioClient.FPutObject(ctx, bucketName, objectName, filePath, minio.PutObjectOptions{ContentType: contentType})
	// if err != nil {
	// 	log.Fatalln(err)
	// }

	// log.Printf("Successfully uploaded %s of size %d\n", objectName, info.Size)
}
