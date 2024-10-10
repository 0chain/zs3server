package handler

import (
	"context"
	"log"
	"net/http"

	"zsearch/indexer/model"

	"github.com/google/go-tika/tika"
)

func IndexHandler(jobChan chan<- model.FileInfo, client *tika.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		file, handler, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "Error retrieving the file", http.StatusBadRequest)
			return
		}
		defer file.Close()
		log.Printf("indexing file %+v \n", handler.Filename)
		bucketName := r.FormValue("bucketName")
		objName := r.FormValue("objName")
		body, err := client.ParseRecursive(context.Background(), file)
		if err != nil {
			log.Printf("err parsing the file using tika %+v \n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(body) == 0 {
			log.Println("No texts processed")
			return
		}

		fileInfo := model.FileInfo{
			Path:     bucketName + "/" + objName,
			Filename: objName,
			Content:  body[0],
		}

		jobChan <- fileInfo
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Files indexed successfully"))

	}
}

func PutIndexHandler(jobChan chan<- model.FileInfo, client *tika.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bucketName := r.URL.Query().Get("bucketName")
		objName := r.URL.Query().Get("objName")
		log.Printf("parsing file %s", bucketName+"/"+objName)

		body, err := client.ParseRecursive(context.Background(), r.Body)
		if err != nil {
			log.Printf("Error parsing the file using Tika for %s/%s: %+v", bucketName, objName, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(body) == 0 {
			log.Printf("No texts processed for %s/%s", bucketName, objName)
			http.Error(w, "no texts processed", http.StatusInternalServerError)
			return
		}

		fileInfo := model.FileInfo{
			Path:     bucketName + "/" + objName,
			Filename: objName,
			Content:  body[0],
		}

		jobChan <- fileInfo

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("File received for processing"))

	}
}
