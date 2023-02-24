package adminapi

import (
	"context"

	"github.com/minio/madmin-go/v2"
)

type SetUserResponse struct {
	Success bool
}

func setUser(minioCredentials MinioCredentials, accessKey string, secretKey string, status string) (*SetUserResponse, error) {
	madmClnt, err := createClient(minioCredentials.AccessKey, minioCredentials.SecretAccessKey)
	if err != nil {
		return nil, err
	}
	err = madmClnt.SetUser(context.Background(), accessKey, secretKey, madmin.AccountStatus(status))
	if err != nil {
		return nil, err
	}
	setUserResponse := SetUserResponse{}
	setUserResponse.Success = true
	return &setUserResponse, nil
}
