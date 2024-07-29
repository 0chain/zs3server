package s3api

import (
	"context"
	"fmt"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type GetObjectResponse struct {
	Success    bool
	ObjectPath string
}

func getObject(bucketName string, objectName string, minioCredentials MinioCredentials) (GetObjectResponse, error) {
	fmt.Println("Harsh getObject")
	ctx := context.Background()
	minioClient, err := minio.New(ENDPOINT, &minio.Options{
		Creds:  credentials.NewStaticV4(minioCredentials.AccessKey, minioCredentials.SecretAccessKey, ""),
		Secure: USESSL,
	})
	minioCacheClient, err := minio.New("miniocache:9000", &minio.Options{
		Creds:  credentials.NewStaticV4(minioCredentials.AccessKey, minioCredentials.SecretAccessKey, ""),
		Secure: USESSL,
	})
	getObjectResponse := GetObjectResponse{}
	if err != nil {
		return getObjectResponse, err
	}
	err = minioCacheClient.FGetObject(ctx, bucketName, objectName, "/tmp/"+bucketName+"/"+objectName, minio.GetObjectOptions{})
	if err != nil {
		fmt.Println("Harsh file not find in cache")
	} else {
		getObjectResponse.Success = true
		getObjectResponse.ObjectPath = "/tmp/" + bucketName + "/" + objectName

		return getObjectResponse, nil
	}
	//get and download the object from server
	err = minioClient.FGetObject(ctx, bucketName, objectName, "/tmp/"+bucketName+"/"+objectName, minio.GetObjectOptions{})
	if err != nil {
		fmt.Println(err)
		return getObjectResponse, err
	}
	getObjectResponse.Success = true
	getObjectResponse.ObjectPath = "/tmp/" + bucketName + "/" + objectName

	return getObjectResponse, nil
}
