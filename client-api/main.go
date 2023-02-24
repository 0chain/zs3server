package main

import (
	"log"
	"net/http"

	"github.com/rs/cors"
	"github.com/zs3server/adminapi"
	"github.com/zs3server/s3api"
)

func main() {
	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
		AllowedHeaders:   []string{"Authorization", "Content-Type", "Access-Control-Allow-Origin"},
		AllowedMethods:   []string{"GET", "UPDATE", "PUT", "POST", "DELETE"},
		Debug:            false,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/admin", adminapi.Handler)
	mux.HandleFunc("/admin/", adminapi.Handler)
	mux.HandleFunc("/", s3api.Handler)
	err := http.ListenAndServe(":3001", c.Handler(mux))
	if err != nil {
		log.Fatalln(err)
	}
}
