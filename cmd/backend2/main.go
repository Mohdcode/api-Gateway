package main

import (
	"log"
	"net/http"
	"os"
)

func hello(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("backend-2", "true")
	w.WriteHeader(http.StatusOK)
	log.Print("hello product page")
	w.Write([]byte("Hello from the backend-2"))
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
		port = ":8082"
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
