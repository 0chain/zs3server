package s3api

import (
	"context"
	"fmt"
	"os"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type PutObjectResponse struct {
	Success bool
	bucket  string
	Name    string
	Size    int64
}

func putObject(bucketName string, objectName string) (PutObjectResponse, error) {
	ctx := context.Background()
	putObjectResponse := PutObjectResponse{}
	minioClient, err := minio.New(ENDPOINT, &minio.Options{
		Creds:  credentials.NewStaticV4(ACCESSKEY, SECRETACCESSKEY, ""),
		Secure: USESSL,
	})

	if err != nil {
		return putObjectResponse, err
	}
	file, err := os.Open("/tmp/" + objectName)
	if err != nil {
		fmt.Println(err)
		return putObjectResponse, err
	}
	defer file.Close()

	fileStat, err := file.Stat()
	if err != nil {
		fmt.Println(err)
		return putObjectResponse, err
	}

	uploadInfo, err := minioClient.PutObject(ctx, bucketName, objectName, file, fileStat.Size(), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		fmt.Println(err)
		return putObjectResponse, err
	}

	putObjectResponse.Success = true
	putObjectResponse.bucket = uploadInfo.Bucket
	putObjectResponse.Name = uploadInfo.Key
	putObjectResponse.Size = uploadInfo.Size

	return putObjectResponse, nil
}
