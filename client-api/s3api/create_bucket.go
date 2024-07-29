package s3api

import (
	"context"
	"fmt"
	"log"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type CreateBucketResponse struct {
	Success    bool
	Bucketname string
}

func createBucket(bucketName string, location string, minioCredentials MinioCredentials) (CreateBucketResponse, error) {
	fmt.Println("Harsh create buckert calll", bucketName)
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

	minioCacheClient, err := minio.New("miniocache:9000", &minio.Options{
		Creds:  credentials.NewStaticV4(minioCredentials.AccessKey, minioCredentials.SecretAccessKey, ""),
		Secure: USESSL,
	})
	if err == nil {
		fmt.Println("Harsh minio client no err")
		err = minioCacheClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: location})
		if err != nil {
			fmt.Println("Harsh err creating bucket", err)
			exists, errBucketExists := minioCacheClient.BucketExists(ctx, bucketName)
			if errBucketExists == nil && exists {
				fmt.Printf("Harsh Bucket %s already exists\n", bucketName)
			} else {
				fmt.Println("Harsh minio client bucket exists", err)
			}
		} else {
			fmt.Printf("Harsh Successfully created %s\n", bucketName)
		}
	}

	err = minioClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: location})
	if err != nil {
		// Check to see if we already own this bucket (which happens if you run this twice)
		exists, errBucketExists := minioClient.BucketExists(ctx, bucketName)
		if errBucketExists == nil && exists {
			log.Printf("We already own %s\n", bucketName)
			return createBucketResponse, nil
		} else {
			//log.Fatalln(err)
			return createBucketResponse, err
		}
	}
	createBucketResponse.Bucketname = bucketName
	createBucketResponse.Success = true

	return createBucketResponse, nil
}
