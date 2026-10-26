package main

import (
	"log"
	"net/http"
	"os"
)

func hello(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("backend-3", "true")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Hello from the backend-3 running on port 8083"))
}

func health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func main() {
	router := http.NewServeMux()

	router.HandleFunc("/health", health)
	router.HandleFunc("/", hello)

	port := os.Getenv("PORT")
	if port == "" {
		port = ":8083"
	}

	server := &http.Server{
		Addr:    port,
		Handler: router,
	}

	err := server.ListenAndServe()
	if err != nil {
		log.Fatal("Error starting the server")
	}
}
