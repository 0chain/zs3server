package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"zsearch/indexer/model"
	"zsearch/utility"

	"github.com/blevesearch/bleve/v2"
	"github.com/google/go-tika/tika"
)

func IndexHandler(index bleve.Index, client *tika.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		file, handler, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "Error retrieving the file", http.StatusBadRequest)
			return
		}
		defer file.Close()
		fmt.Println("filenameee", handler.Filename)
		log.Printf("indexing file %+v \n", handler.Filename)
		bucketName := r.FormValue("bucketName")
		objName := r.FormValue("objName")
		body, err := client.ParseRecursive(context.Background(), file)
		if err != nil || len(body) == 0 {
			log.Printf("err parsing the file using tika %+v \n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		eText := body[0]
		cleanText := utility.CleanText(eText)
		fileInfo := model.FileInfo{}
		fileInfo.Path = bucketName + "/" + objName
		fileInfo.Filename = objName
		fileInfo.Content = cleanText
		err = utility.IndexFiles(index, []model.FileInfo{fileInfo})
		if err != nil {
			log.Printf("err indexing file %+v \n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Files indexed successfully"))

	}
}

func PutIndexHandler(index bleve.Index, client *tika.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("indexing file")
		bucketName := r.URL.Query().Get("bucketName")
		objName := r.URL.Query().Get("objName")
		fmt.Println("objName", objName)
		body, err := client.ParseRecursive(context.Background(), r.Body)
		if err != nil {
			log.Printf("err parsing the file using tika %+v \n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(body) == 0 {
			log.Printf("no texts processed")
			http.Error(w, "no texts processed", http.StatusInternalServerError)
			return
		}
		eText := body[0]
		cleanText := utility.CleanText(eText)
		fileInfo := model.FileInfo{}
		fileInfo.Path = bucketName + "/" + objName
		fileInfo.Filename = objName
		fileInfo.Content = cleanText
		err = utility.IndexFiles(index, []model.FileInfo{fileInfo})
		if err != nil {
			log.Printf("err indexing file %+v \n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Files indexed successfully"))

	}
}
