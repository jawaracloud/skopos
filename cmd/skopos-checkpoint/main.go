package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jawaracloud/skopos/internal/checkpoint"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usageText = `usage: skopos-checkpoint [serve|replay|version]

serve    receive OTLP/HTTP, append to the local WAL, and upload sealed
         segments to S3 (default)
replay   read checkpoint objects from S3 and re-inject them into the
         Collector; use -delete to remove each object after it is replayed
version  print the build version
`

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "version", "-version", "--version":
			fmt.Println(version)
			return
		}
	}

	cfg, err := ConfigFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skopos-checkpoint: config: %v\n", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	mode := "serve"
	if len(args) > 0 {
		mode = args[0]
		args = args[1:]
	}

	switch mode {
	case "serve":
		if len(args) != 0 {
			err = fmt.Errorf("serve: unexpected arguments %v\n%s", args, usageText)
			break
		}
		err = runServe(cfg, logger)
	case "replay":
		err = runReplay(cfg, logger, args)
	default:
		err = fmt.Errorf("unknown mode %q\n%s", mode, usageText)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "skopos-checkpoint: %v\n", err)
		os.Exit(1)
	}
}

func runServe(cfg Config, logger *slog.Logger) error {
	if err := cfg.ValidateServe(); err != nil {
		return err
	}
	cp, err := checkpoint.Open(checkpoint.Config{
		Dir:           cfg.Dir,
		SealBytes:     cfg.SealBytes,
		SealInterval:  cfg.SealInterval,
		MaxLocalBytes: cfg.MaxLocalBytes,
		FlushTimeout:  cfg.FlushTimeout,
		S3:            &cfg.S3,
		Prefix:        cfg.Prefix,
		Logger:        logger,
	})
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}

	s := &server{
		checkpoint:      cp,
		logger:          logger,
		maxRequestBytes: cfg.MaxRequestBytes,
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	logger.Info("starting checkpoint receiver",
		"version", version,
		"addr", cfg.ListenAddr,
		"dir", cfg.Dir,
		"seal_bytes", cfg.SealBytes,
		"seal_interval", cfg.SealInterval.String(),
		"max_local_bytes", cfg.MaxLocalBytes,
		"max_request_bytes", cfg.MaxRequestBytes,
		"s3_endpoint", cfg.S3.Endpoint,
		"s3_bucket", cfg.S3.Bucket,
		"s3_prefix", cfg.Prefix,
		"s3_virtual_host", cfg.S3.VirtualHost,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case serveErr := <-errCh:
		closeErr := cp.Close()
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", errors.Join(serveErr, closeErr))
		}
		return closeErr
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		shutdownErr := srv.Shutdown(shutdownCtx)
		cancel()
		closeErr := cp.Close()
		if shutdownErr != nil {
			return fmt.Errorf("shutdown: %w", errors.Join(shutdownErr, closeErr))
		}
		if closeErr != nil {
			return fmt.Errorf("checkpoint close: %w", closeErr)
		}
		return nil
	}
}

type server struct {
	checkpoint      *checkpoint.Checkpoint
	logger          *slog.Logger
	maxRequestBytes int64
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("POST /v1/traces", s.handleOTLP(checkpoint.SignalTraces))
	mux.HandleFunc("POST /v1/metrics", s.handleOTLP(checkpoint.SignalMetrics))
	mux.HandleFunc("POST /v1/logs", s.handleOTLP(checkpoint.SignalLogs))
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (s *server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if err := s.checkpoint.Ready(); err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ready\n")
}

func (s *server) handleStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(s.checkpoint.Stats()); err != nil {
		s.logger.Error("encode stats", "err", err)
	}
}

func (s *server) handleOTLP(signal checkpoint.Signal) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > s.maxRequestBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "" {
			mediaType, _, err := mime.ParseMediaType(contentType)
			if err != nil || mediaType != "application/x-protobuf" {
				http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
				return
			}
		}

		body := http.MaxBytesReader(w, r.Body, s.maxRequestBytes)
		var reader io.Reader = body
		switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
		case "", "identity":
		case "gzip":
			zr, err := gzip.NewReader(body)
			if err != nil {
				http.Error(w, "invalid gzip body", http.StatusBadRequest)
				return
			}
			defer zr.Close()
			reader = zr
		default:
			http.Error(w, "unsupported content encoding", http.StatusUnsupportedMediaType)
			return
		}

		payload, err := io.ReadAll(io.LimitReader(reader, s.maxRequestBytes+1))
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if int64(len(payload)) > s.maxRequestBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if len(payload) == 0 {
			http.Error(w, "empty request body", http.StatusBadRequest)
			return
		}

		if err := s.checkpoint.Append(signal, payload); err != nil {
			switch {
			case errors.Is(err, checkpoint.ErrBackpressure):
				w.Header().Set("Retry-After", "1")
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
			case errors.Is(err, checkpoint.ErrClosed):
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
			default:
				s.logger.Error("append failed", "signal", signal.String(), "err", err)
				http.Error(w, "failed to persist telemetry", http.StatusInternalServerError)
			}
			return
		}

		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}
}

func runReplay(cfg Config, logger *slog.Logger, args []string) error {
	if err := cfg.ValidateReplay(); err != nil {
		return err
	}
	flags := flag.NewFlagSet("replay", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	deleteAfter := flags.Bool("delete", cfg.ReplayDeleteAfter, "delete each checkpoint object after it is replayed")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("replay: unexpected arguments %v", flags.Args())
	}

	s3, err := checkpoint.NewS3(cfg.S3)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stats, err := checkpoint.Replay(ctx, checkpoint.ReplayConfig{
		S3:          s3,
		Prefix:      cfg.Prefix,
		ForwardURL:  cfg.ForwardURL,
		DeleteAfter: *deleteAfter,
		Timeout:     cfg.ReplayTimeout,
	})
	if err != nil {
		return err
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(stats); err != nil {
		return err
	}
	logger.Info("replay complete",
		"objects", stats.Objects,
		"records", stats.Records,
		"payload_bytes", stats.PayloadBytes,
		"deleted_objects", stats.DeletedObjects,
		"skipped_objects", stats.SkippedObjects,
	)
	return nil
}
