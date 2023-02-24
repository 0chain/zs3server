package adminapi

import (
	"context"
	"time"
)

type ListUsersResponse struct {
	SecretKey  string
	UpdatedAt  time.Time
	MemberOf   []string
	PolicyName string
	Status     string
	AccessKey  string
}

func listUsers(minioCredentials MinioCredentials) (*[]ListUsersResponse, error) {
	madmClnt, err := createClient(minioCredentials.AccessKey, minioCredentials.SecretAccessKey)
	if err != nil {
		return nil, err
	}
	users, err := madmClnt.ListUsers(context.Background())
	if err != nil {
		return nil, err
	}
	listUsersResponse := []ListUsersResponse{}
	for k, v := range users {
		listUserResponse := ListUsersResponse{}
		listUserResponse.AccessKey = k
		listUserResponse.SecretKey = v.SecretKey
		listUserResponse.PolicyName = v.PolicyName
		listUserResponse.MemberOf = v.MemberOf
		listUserResponse.UpdatedAt = v.UpdatedAt
		listUserResponse.Status = string(v.Status)
		listUsersResponse = append(listUsersResponse, listUserResponse)
	}

	return &listUsersResponse, nil
}
