package adminapi

import "context"

type RemoveUserResponse struct {
	Success bool
}

func removeUser(minioCredentials MinioCredentials, accessKey string) (*RemoveUserResponse, error) {
	madmClnt, err := createClient(minioCredentials.AccessKey, minioCredentials.SecretAccessKey)
	if err != nil {
		return nil, err
	}
	err = madmClnt.RemoveUser(context.Background(), accessKey)

	if err != nil {
		return nil, err
	}
	removeUserResponse := RemoveUserResponse{}

	removeUserResponse.Success = true
	return &removeUserResponse, nil
}
