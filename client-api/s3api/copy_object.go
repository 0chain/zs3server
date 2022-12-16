package s3api

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type CopyObjectResponse struct {
	Success      bool
	Bucket       string
	Key          string
	Size         int64
	LastModified time.Time
	VersionID    string
}

func copyObject(sourceBucket string, desBucket string, sourceObject string, desObject string, minioCredentials MinioCredentials) (CopyObjectResponse, error) {
	copyObjectResponse := CopyObjectResponse{}
	minioClient, err := createCleint(minioCredentials)
	if err != nil {
		return copyObjectResponse, err
	}

	srcOpts := minio.CopySrcOptions{
		Bucket: sourceBucket,
		Object: sourceObject,
	}
	dstOpts := minio.CopyDestOptions{
		Bucket: desBucket,
		Object: desObject,
	}
	// Copy object call
	uploadInfo, err := minioClient.CopyObject(context.Background(), dstOpts, srcOpts)
	if err != nil {
		fmt.Println(err)
		return copyObjectResponse, err
	}

	copyObjectResponse.Bucket = uploadInfo.Bucket
	copyObjectResponse.Key = uploadInfo.Key
	copyObjectResponse.LastModified = uploadInfo.LastModified
	copyObjectResponse.Size = uploadInfo.Size
	copyObjectResponse.VersionID = uploadInfo.VersionID
	copyObjectResponse.Success = true
	return copyObjectResponse, nil
}

func createCleint(minioCredentials MinioCredentials) (*minio.Client, error) {
	minioClient, err := minio.New(ENDPOINT, &minio.Options{
		Creds:  credentials.NewStaticV4(minioCredentials.AccessKey, minioCredentials.SecretAccessKey, ""),
		Secure: USESSL,
	})
	if err != nil {
		return nil, err
	}
	return minioClient, nil
}
