package s3api

import (
	"context"
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
	listBucketsObjects := []ListBucketsObject{}
	bucketInfo, err := minioClient.ListBuckets(ctx)
	for i := 0; i < len(bucketInfo); i++ {
		listbucketobject := ListBucketsObject{}
		bucketObjects := []BucketObjects{}
		for object := range minioClient.ListObjects(ctx, bucketInfo[i].Name, minio.ListObjectsOptions{Recursive: true}) {
			bucketObect := BucketObjects{}
			bucketObect.Name = object.Key
			bucketObect.LastModified = object.LastModified
			bucketObjects = append(bucketObjects, bucketObect)
		}
		listbucketobject.BucketName = bucketInfo[i].Name
		listbucketobject.CreationTime = bucketInfo[i].CreationDate
		listbucketobject.BucketObjects = bucketObjects
		listBucketsObjects = append(listBucketsObjects, listbucketobject)
	}

	return listBucketsObjects, nil
}
