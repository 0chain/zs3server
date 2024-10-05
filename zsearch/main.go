package main

import (
	"log"
	"net/http"
	ihandler "zsearch/indexer/handler"
	"zsearch/search/handler"
	"zsearch/utility"

	"github.com/google/go-tika/tika"
)

func main() {

	client := tika.NewClient(nil, "http://tika:9998")

	index, err := utility.OpenOrCreateIndex("/vindex/files_index.bleve")
	if err != nil {
		log.Fatalln(err)
	}
	defer index.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/index", ihandler.IndexHandler(index, client))
	mux.HandleFunc("/search", handler.SearchHandler(index))
	mux.HandleFunc("/zindex", ihandler.PutIndexHandler(index, client))
	log.Println("Server is starting on port 3003")
	if err := http.ListenAndServe(":3003", mux); err != nil {
		log.Fatalln(err)
	}
}
