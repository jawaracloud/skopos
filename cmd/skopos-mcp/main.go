package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

var version = "dev"

func main() {
	cfg := ConfigFromEnv()
	cfg.Version = version
	server, err := NewServer(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "skopos-mcp: %v\n", err)
		os.Exit(1)
	}
	if cfg.HTTPAddr != "" {
		srv := &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           server.HTTPHandler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
		}
		fmt.Fprintf(os.Stderr, "skopos-mcp: listening on %s\n", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "skopos-mcp: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := server.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "skopos-mcp: %v\n", err)
		os.Exit(1)
	}
}
