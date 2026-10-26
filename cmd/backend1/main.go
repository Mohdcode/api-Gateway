package main

import (
	"log"
	"net/http"
	"os"
)

func hello(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("backend-1", "true")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Hello from the backend-1 running on port 8081"))
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
		port = ":8081"
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
