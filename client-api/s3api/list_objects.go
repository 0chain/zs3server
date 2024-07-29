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

	minioCacheClient, err := minio.New("miniocache:9000", &minio.Options{
		Creds:  credentials.NewStaticV4(minioCredentials.AccessKey, minioCredentials.SecretAccessKey, ""),
		Secure: USESSL,
	})
	if err != nil {
		fmt.Println("Minio cache client not initialized")
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

	listObjectCacheResponse := []ListObjectResponse{}
	for object := range minioCacheClient.ListObjects(ctx, bucketName, minio.ListObjectsOptions{Recursive: true}) {
		if object.Err != nil {
			fmt.Println(object.Err.Error())
			return nil, object.Err
		}
		listObject := ListObjectResponse{}
		listObject.Name = object.Key
		listObject.LastModified = object.LastModified

		listObjectCacheResponse = append(listObjectCacheResponse, listObject)
	}
	fmt.Println("Harsh list cache obj", listObjectCacheResponse)
	mergedListObj := mergeListObjects(listObjectsResponse, listObjectCacheResponse)
	return mergedListObj, nil
}

func mergeListObjects(l1, l2 []ListObjectResponse) []ListObjectResponse {
	mergedMap := make(map[string]ListObjectResponse)

	// Helper function to add/update map entries
	addOrUpdate := func(obj ListObjectResponse) {
		if existingObj, found := mergedMap[obj.Name]; !found || obj.LastModified.After(existingObj.LastModified) {
			mergedMap[obj.Name] = obj
		}
	}
	for _, obj := range l1 {
		addOrUpdate(obj)
	}
	for _, obj := range l2 {
		addOrUpdate(obj)
	}

	mergedList := make([]ListObjectResponse, 0, len(mergedMap))
	for _, obj := range mergedMap {
		mergedList = append(mergedList, obj)
	}

	return mergedList
}
