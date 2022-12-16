package s3api

import (
	"context"
	"log"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type CreateBucketResponse struct {
	Success    bool
	Bucketname string
}

func createBucket(bucketName string, location string, minioCredentials MinioCredentials) (CreateBucketResponse, error) {
	ctx := context.Background()
	minioClient, err := minio.New(ENDPOINT, &minio.Options{
		Creds:  credentials.NewStaticV4(minioCredentials.AccessKey, minioCredentials.SecretAccessKey, ""),
		Secure: USESSL,
	})
	createBucketResponse := CreateBucketResponse{}
	if err != nil {
		//log.Fatalln(err)
		return createBucketResponse, err
	}

	err = minioClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: location})
	if err != nil {
		// Check to see if we already own this bucket (which happens if you run this twice)
		exists, errBucketExists := minioClient.BucketExists(ctx, bucketName)
		if errBucketExists == nil && exists {
			log.Printf("We already own %s\n", bucketName)
		} else {
			log.Fatalln(err)
			return createBucketResponse, err
		}
	}
	createBucketResponse.Bucketname = bucketName
	createBucketResponse.Success = true

	return createBucketResponse, nil
}
