// Command e2e-server is the uninstrumented HTTP target used by the OBI
// end-to-end harness. OBI instruments it without any SDK or code changes.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "world"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "hello " + name})
	})
	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		ms := 25
		if raw := r.URL.Query().Get("ms"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n <= 1000 {
				ms = n
			}
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"slept_ms": ms})
	})

	log.Printf("e2e-server listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
