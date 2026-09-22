package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

type response struct {
	Version   string `json:"version"`
	GrayStage string `json:"gray_stage"`
	Host      string `json:"host"`
	Time      string `json:"time"`
}

func main() {
	version := os.Getenv("DEMO_VERSION")
	if version == "" {
		version = "stable"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/gray/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, version)
	})
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, version)
	})

	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("gray demo api started: version=%s", version)
	log.Fatal(server.ListenAndServe())
}

func writeJSON(w http.ResponseWriter, r *http.Request, version string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response{
		Version:   version,
		GrayStage: r.Header.Get("x-gray-user"),
		Host:      os.Getenv("HOSTNAME"),
		Time:      time.Now().UTC().Format(time.RFC3339),
	})
}
