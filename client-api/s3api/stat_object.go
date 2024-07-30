package s3api

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type StatObjectResponse struct {
	Key          string    // Name of the object
	LastModified time.Time // Date and time the object was last modified.
	Size         int64     // Size in bytes of the object.
	ContentType  string    // A standard MIME type describing the format of the object data.
	Expires      time.Time
}

func statObject(bucketName string, objectName string, minioCredentials MinioCredentials) (*StatObjectResponse, error) {
	minioClient, err := createCleint(minioCredentials)
	if err != nil {
		return nil, err
	}
	minioCacheClient, err := minio.New("miniocache:9000", &minio.Options{
		Creds:  credentials.NewStaticV4(minioCredentials.AccessKey, minioCredentials.SecretAccessKey, ""),
		Secure: USESSL,
	})
	if err != nil {
		fmt.Println("Minio cache client not initialized")
		return nil, err
	}
	objInfo, err := minioClient.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	statObjectResponse := StatObjectResponse{}
	statObjectResponse.ContentType = objInfo.ContentType
	statObjectResponse.Key = objInfo.Key
	statObjectResponse.LastModified = objInfo.LastModified
	statObjectResponse.Size = objInfo.Size
	statObjectResponse.Expires = objInfo.Expires
	objInfoCache, err := minioCacheClient.StatObject(context.Background(), bucketName, objectName, minio.StatObjectOptions{})
	if err == nil {
		if objInfoCache.Key == objInfo.Key && objInfoCache.LastModified.After(objInfo.LastModified) {
			statObjectResponse.ContentType = objInfoCache.ContentType
			statObjectResponse.Key = objInfoCache.Key
			statObjectResponse.LastModified = objInfoCache.LastModified
			statObjectResponse.Size = objInfoCache.Size
			statObjectResponse.Expires = objInfoCache.Expires
		}
	}
	return &statObjectResponse, nil
}
