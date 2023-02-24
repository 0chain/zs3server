package s3api

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type ListBucketResponse struct {
	BucketName   string
	CreationDate time.Time
}

func listBucket(minioCredentials MinioCredentials) ([]ListBucketResponse, error) {
	ctx := context.Background()
	minioClient, err := minio.New(ENDPOINT, &minio.Options{
		Creds:  credentials.NewStaticV4(minioCredentials.AccessKey, minioCredentials.SecretAccessKey, ""),
		Secure: USESSL,
	})
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	bucketInfo, err := minioClient.ListBuckets(ctx)

	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	listBucketRespone := []ListBucketResponse{}
	for i := 0; i < len(bucketInfo); i++ {
		listbucketInfo := ListBucketResponse{}
		listbucketInfo.BucketName = bucketInfo[i].Name
		listbucketInfo.CreationDate = bucketInfo[i].CreationDate
		listBucketRespone = append(listBucketRespone, listbucketInfo)
	}

	return listBucketRespone, nil
}
