package adminapi

import "context"

type AddUserResponse struct {
	Success   bool
	AccessKey string
}

func addUser(minioCredentials MinioCredentials, accessKey string, secretKey string) (*AddUserResponse, error) {
	madmClnt, err := createClient(minioCredentials.AccessKey, minioCredentials.SecretAccessKey)
	if err != nil {
		return nil, err
	}
	err = madmClnt.AddUser(context.Background(), accessKey, secretKey)
	if err != nil {
		return nil, err
	}
	addUserResponse := AddUserResponse{}
	addUserResponse.Success = true
	addUserResponse.AccessKey = accessKey

	return &addUserResponse, nil
}
