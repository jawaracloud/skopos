package main

import (
	"fmt"
	"os"
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
	if err := server.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "skopos-mcp: %v\n", err)
		os.Exit(1)
	}
}
