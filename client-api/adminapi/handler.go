package adminapi

import (
	"net/http"
	"os"
)

var ENDPOINT = os.Getenv("MINIO_SERVER")
var USESSL = false

type MinioCredentials struct {
	AccessKey       string
	SecretAccessKey string
}

func Handler(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	minioCredentials := MinioCredentials{}
	minioCredentials.AccessKey = r.URL.Query().Get("accessKey")
	minioCredentials.SecretAccessKey = r.URL.Query().Get("secretAccessKey")
	switch action {
	case "listUsers":
		users, err := listUsers(minioCredentials)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, users)
	case "addUser":
		newAccessKey := r.URL.Query().Get("userAccessKey")
		newSecretKey := r.URL.Query().Get("userSecretKey")
		addUserResponse, err := addUser(minioCredentials, newAccessKey, newSecretKey)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, addUserResponse)
	case "removeUser":
		userAccessKey := r.URL.Query().Get("userAccessKey")
		removeUserResponse, err := removeUser(minioCredentials, userAccessKey)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, removeUserResponse)
	case "setUser":
		userAccessKey := r.URL.Query().Get("userAccessKey")
		userSecretKey := r.URL.Query().Get("userSecretKey")
		status := r.URL.Query().Get("status")
		removeUserResponse, err := setUser(minioCredentials, userAccessKey, userSecretKey, status)
		if err != nil {
			JSON(w, 500, map[string]string{"error": err.Error()})
			break
		}
		JSON(w, 200, removeUserResponse)
	default:
		JSON(w, 500, map[string]string{"message": "feature not avaliable"})
	}
}
