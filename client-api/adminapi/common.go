package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/minio/madmin-go/v2"
)

func createClient(accessKey string, secretAccessKey string) (*madmin.AdminClient, error) {
	mdmClnt, err := madmin.New(ENDPOINT, accessKey, secretAccessKey, USESSL)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	return mdmClnt, nil
}

func JSON(w http.ResponseWriter, code int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(response)
}
