package s3api

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type ListObjectResponse struct {
	Name         string
	LastModified time.Time
}

func listobjects(bucketName string, minioCredentials MinioCredentials) ([]ListObjectResponse, error) {
	ctx := context.Background()
	minioClient, err := minio.New(ENDPOINT, &minio.Options{
		Creds:  credentials.NewStaticV4(minioCredentials.AccessKey, minioCredentials.SecretAccessKey, ""),
		Secure: USESSL,
	})

	if err != nil {
		return nil, err
	}
	listObjectsResponse := []ListObjectResponse{}
	for object := range minioClient.ListObjects(ctx, bucketName, minio.ListObjectsOptions{Recursive: true}) {
		if object.Err != nil {
			fmt.Println(object.Err.Error())
			return nil, object.Err
		}
		listObject := ListObjectResponse{}
		listObject.Name = object.Key
		listObject.LastModified = object.LastModified

		listObjectsResponse = append(listObjectsResponse, listObject)
	}
	return listObjectsResponse, nil
}
