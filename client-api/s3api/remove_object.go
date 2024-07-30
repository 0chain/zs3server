package s3api

import (
	"context"
	"fmt"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type RemoveObjectResponse struct {
	Success    bool
	ObjectName string
}

func removeObject(bucketName string, objectName string, minioCredentials MinioCredentials) (RemoveObjectResponse, error) {
	ctx := context.Background()
	minioClient, err := minio.New(ENDPOINT, &minio.Options{
		Creds:  credentials.NewStaticV4(minioCredentials.AccessKey, minioCredentials.SecretAccessKey, ""),
		Secure: USESSL,
	})
	removeObjectResponse := RemoveObjectResponse{}
	if err != nil {
		return removeObjectResponse, err
	}
	minioCacheClient, err := minio.New("miniocache:9000", &minio.Options{
		Creds:  credentials.NewStaticV4(minioCredentials.AccessKey, minioCredentials.SecretAccessKey, ""),
		Secure: USESSL,
	})
	if err != nil {
		return removeObjectResponse, err
	}
	err = minioClient.RemoveObject(ctx, bucketName, objectName, minio.RemoveObjectOptions{GovernanceBypass: true})
	if err != nil {
		fmt.Println(err)
		return removeObjectResponse, err
	}
	err = minioCacheClient.RemoveObject(ctx, bucketName, objectName, minio.RemoveObjectOptions{GovernanceBypass: true})
	if err != nil {
		fmt.Println("Cache not present", err)
	}
	removeObjectResponse.Success = true
	removeObjectResponse.ObjectName = objectName
	return removeObjectResponse, nil
}
