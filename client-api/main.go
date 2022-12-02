package main

import (
	"log"
	"net/http"

	"github.com/minio-sdk/s3api"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/", s3api.Handler)
	err := http.ListenAndServe(":3001", mux)
	if err != nil {
		log.Fatalln(err)
	}
}
