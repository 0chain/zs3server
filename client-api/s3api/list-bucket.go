package s3api

import (
	"context"
	"log"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type ListBucketResponse struct {
	BucketName   string
	CreationDate time.Time
}

func listBucket() ([]ListBucketResponse, error) {
	ctx := context.Background()
	minioClient, err := minio.New(ENDPOINT, &minio.Options{
		Creds:  credentials.NewStaticV4(ACCESSKEY, SECRETACCESSKEY, ""),
		Secure: USESSL,
	})
	if err != nil {
		log.Fatalln(err)
	}

	bucketInfo, err := minioClient.ListBuckets(ctx)

	if err != nil {
		log.Fatal(err)
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
