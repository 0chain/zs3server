package s3api

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type ListBucketsObject struct {
	BucketName    string
	CreationTime  time.Time
	BucketObjects []BucketObjects
}

type BucketObjects struct {
	Name         string
	LastModified time.Time
}

func listBucketsObjects(minioCredentials MinioCredentials) ([]ListBucketsObject, error) {
	//miniClient, err := getMinioClient()
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

	listBucketsObjects := []ListBucketsObject{}
	bucketInfo, err := minioClient.ListBuckets(ctx)
	for i := 0; i < len(bucketInfo); i++ {
		listbucketobject := ListBucketsObject{}
		bucketObjects := []BucketObjects{}
		for object := range minioClient.ListObjects(ctx, bucketInfo[i].Name, minio.ListObjectsOptions{Recursive: true}) {
			bucketObject := BucketObjects{}
			bucketObject.Name = object.Key
			bucketObject.LastModified = object.LastModified
			bucketObjects = append(bucketObjects, bucketObject)
		}
		bucketCacheObjects := []BucketObjects{}
		for object := range minioCacheClient.ListObjects(ctx, bucketInfo[i].Name, minio.ListObjectsOptions{Recursive: true}) {
			bucketObject := BucketObjects{}
			bucketObject.Name = object.Key
			bucketObject.LastModified = object.LastModified
			bucketCacheObjects = append(bucketCacheObjects, bucketObject)
		}
		mergeBucketObjects := mergeBucketObjects(bucketObjects, bucketCacheObjects)
		listbucketobject.BucketName = bucketInfo[i].Name
		listbucketobject.CreationTime = bucketInfo[i].CreationDate
		listbucketobject.BucketObjects = mergeBucketObjects
		listBucketsObjects = append(listBucketsObjects, listbucketobject)
	}

	return listBucketsObjects, nil
}

func mergeBucketObjects(l1, l2 []BucketObjects) []BucketObjects {
	mergedMap := make(map[string]BucketObjects)

	// Helper function to add/update map entries
	addOrUpdate := func(obj BucketObjects) {
		if existingObj, found := mergedMap[obj.Name]; !found || obj.LastModified.After(existingObj.LastModified) {
			mergedMap[obj.Name] = obj
		}
	}
	for _, obj := range l1 {
		addOrUpdate(obj)
	}
	for _, obj := range l1 {
		addOrUpdate(obj)
	}

	mergedList := make([]BucketObjects, 0, len(mergedMap))
	for _, obj := range mergedMap {
		mergedList = append(mergedList, obj)
	}

	return mergedList
}
