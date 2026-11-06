package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jawaracloud/skopos/internal/checkpoint"
)

// Config is the checkpoint service configuration loaded from
// SKOPOS_CHECKPOINT_* environment variables.
type Config struct {
	ListenAddr string
	Dir        string

	SealBytes       int64
	SealInterval    time.Duration
	MaxLocalBytes   int64
	MaxRequestBytes int64
	FlushTimeout    time.Duration
	LogLevel        slog.Level

	S3     checkpoint.S3Config
	Prefix string

	ForwardURL        string
	ReplayDeleteAfter bool
	ReplayTimeout     time.Duration
}

// ConfigFromEnv loads configuration from the process environment.
func ConfigFromEnv() (Config, error) {
	return configFromLookup(os.LookupEnv)
}

func configFromLookup(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		ListenAddr:      ":4319",
		Dir:             "/var/lib/skopos/checkpoint",
		SealBytes:       checkpoint.DefaultSealBytes,
		SealInterval:    checkpoint.DefaultSealInterval,
		MaxLocalBytes:   checkpoint.DefaultMaxLocalBytes,
		MaxRequestBytes: checkpoint.MaxRecordBytes,
		FlushTimeout:    checkpoint.DefaultFlushTimeout,
		Prefix:          "skopos/checkpoint",
		ReplayTimeout:   60 * time.Second,
		S3: checkpoint.S3Config{
			Region:  "us-east-1",
			Timeout: 60 * time.Second,
		},
	}

	var err error
	if cfg.ListenAddr, err = stringEnv(lookup, "SKOPOS_CHECKPOINT_LISTEN_ADDR", cfg.ListenAddr); err != nil {
		return Config{}, err
	}
	if cfg.Dir, err = stringEnv(lookup, "SKOPOS_CHECKPOINT_DIR", cfg.Dir); err != nil {
		return Config{}, err
	}
	if cfg.SealBytes, err = int64Env(lookup, "SKOPOS_CHECKPOINT_SEAL_BYTES", cfg.SealBytes); err != nil {
		return Config{}, err
	}
	if cfg.SealInterval, err = durationEnv(lookup, "SKOPOS_CHECKPOINT_SEAL_INTERVAL", cfg.SealInterval); err != nil {
		return Config{}, err
	}
	if cfg.MaxLocalBytes, err = int64Env(lookup, "SKOPOS_CHECKPOINT_MAX_LOCAL_BYTES", cfg.MaxLocalBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxRequestBytes, err = int64Env(lookup, "SKOPOS_CHECKPOINT_MAX_REQUEST_BYTES", cfg.MaxRequestBytes); err != nil {
		return Config{}, err
	}
	if cfg.FlushTimeout, err = durationEnv(lookup, "SKOPOS_CHECKPOINT_FLUSH_TIMEOUT", cfg.FlushTimeout); err != nil {
		return Config{}, err
	}
	if cfg.S3.Endpoint, err = stringEnv(lookup, "SKOPOS_CHECKPOINT_S3_ENDPOINT", cfg.S3.Endpoint); err != nil {
		return Config{}, err
	}
	if cfg.S3.Region, err = stringEnv(lookup, "SKOPOS_CHECKPOINT_S3_REGION", cfg.S3.Region); err != nil {
		return Config{}, err
	}
	if cfg.S3.Bucket, err = stringEnv(lookup, "SKOPOS_CHECKPOINT_S3_BUCKET", cfg.S3.Bucket); err != nil {
		return Config{}, err
	}
	if cfg.Prefix, err = stringEnv(lookup, "SKOPOS_CHECKPOINT_S3_PREFIX", cfg.Prefix); err != nil {
		return Config{}, err
	}
	if cfg.S3.AccessKey, err = stringEnv(lookup, "SKOPOS_CHECKPOINT_S3_ACCESS_KEY", cfg.S3.AccessKey); err != nil {
		return Config{}, err
	}
	if cfg.S3.SecretKey, err = stringEnv(lookup, "SKOPOS_CHECKPOINT_S3_SECRET_KEY", cfg.S3.SecretKey); err != nil {
		return Config{}, err
	}
	if cfg.S3.SessionToken, err = stringEnv(lookup, "SKOPOS_CHECKPOINT_S3_SESSION_TOKEN", cfg.S3.SessionToken); err != nil {
		return Config{}, err
	}
	if cfg.S3.VirtualHost, err = boolEnv(lookup, "SKOPOS_CHECKPOINT_S3_VIRTUAL_HOST", cfg.S3.VirtualHost); err != nil {
		return Config{}, err
	}
	if cfg.S3.Timeout, err = durationEnv(lookup, "SKOPOS_CHECKPOINT_S3_TIMEOUT", cfg.S3.Timeout); err != nil {
		return Config{}, err
	}
	if cfg.ForwardURL, err = stringEnv(lookup, "SKOPOS_CHECKPOINT_FORWARD_URL", cfg.ForwardURL); err != nil {
		return Config{}, err
	}
	if cfg.ReplayDeleteAfter, err = boolEnv(lookup, "SKOPOS_CHECKPOINT_REPLAY_DELETE_AFTER", cfg.ReplayDeleteAfter); err != nil {
		return Config{}, err
	}
	if cfg.ReplayTimeout, err = durationEnv(lookup, "SKOPOS_CHECKPOINT_REPLAY_TIMEOUT", cfg.ReplayTimeout); err != nil {
		return Config{}, err
	}

	level, err := stringEnv(lookup, "SKOPOS_CHECKPOINT_LOG_LEVEL", "info")
	if err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = parseLogLevel(level); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// ValidateServe checks the configuration needed by the OTLP receiver.
func (c Config) ValidateServe() error {
	if err := c.validateCommon(); err != nil {
		return err
	}
	if strings.TrimSpace(c.ListenAddr) == "" {
		return fmt.Errorf("SKOPOS_CHECKPOINT_LISTEN_ADDR: must not be empty")
	}
	if strings.TrimSpace(c.S3.Endpoint) == "" {
		return fmt.Errorf("SKOPOS_CHECKPOINT_S3_ENDPOINT: required in serve mode")
	}
	if strings.TrimSpace(c.S3.Bucket) == "" {
		return fmt.Errorf("SKOPOS_CHECKPOINT_S3_BUCKET: required in serve mode")
	}
	return nil
}

// ValidateReplay checks the configuration needed to drain S3 back into the
// Collector.
func (c Config) ValidateReplay() error {
	if err := c.validateCommon(); err != nil {
		return err
	}
	if strings.TrimSpace(c.S3.Endpoint) == "" {
		return fmt.Errorf("SKOPOS_CHECKPOINT_S3_ENDPOINT: required in replay mode")
	}
	if strings.TrimSpace(c.S3.Bucket) == "" {
		return fmt.Errorf("SKOPOS_CHECKPOINT_S3_BUCKET: required in replay mode")
	}
	if strings.TrimSpace(c.ForwardURL) == "" {
		return fmt.Errorf("SKOPOS_CHECKPOINT_FORWARD_URL: required in replay mode")
	}
	return nil
}

func (c Config) validateCommon() error {
	if strings.TrimSpace(c.Dir) == "" {
		return fmt.Errorf("SKOPOS_CHECKPOINT_DIR: must not be empty")
	}
	if c.SealBytes <= 0 {
		return fmt.Errorf("SKOPOS_CHECKPOINT_SEAL_BYTES: must be positive")
	}
	if c.SealInterval < checkpoint.MinSealInterval {
		return fmt.Errorf("SKOPOS_CHECKPOINT_SEAL_INTERVAL: must be at least %s", checkpoint.MinSealInterval)
	}
	if c.MaxLocalBytes <= 0 {
		return fmt.Errorf("SKOPOS_CHECKPOINT_MAX_LOCAL_BYTES: must be positive")
	}
	if c.SealBytes > checkpoint.MaxSealBytes {
		return fmt.Errorf("SKOPOS_CHECKPOINT_SEAL_BYTES: must not exceed %d", checkpoint.MaxSealBytes)
	}
	if c.SealBytes > c.MaxLocalBytes {
		return fmt.Errorf("SKOPOS_CHECKPOINT_SEAL_BYTES: must not exceed SKOPOS_CHECKPOINT_MAX_LOCAL_BYTES")
	}
	if c.MaxRequestBytes <= 0 {
		return fmt.Errorf("SKOPOS_CHECKPOINT_MAX_REQUEST_BYTES: must be positive")
	}
	if c.MaxRequestBytes > checkpoint.MaxRecordBytes {
		return fmt.Errorf("SKOPOS_CHECKPOINT_MAX_REQUEST_BYTES: must not exceed %d", checkpoint.MaxRecordBytes)
	}
	if c.FlushTimeout <= 0 {
		return fmt.Errorf("SKOPOS_CHECKPOINT_FLUSH_TIMEOUT: must be positive")
	}
	if c.ReplayTimeout <= 0 {
		return fmt.Errorf("SKOPOS_CHECKPOINT_REPLAY_TIMEOUT: must be positive")
	}
	if c.S3.Timeout <= 0 {
		return fmt.Errorf("SKOPOS_CHECKPOINT_S3_TIMEOUT: must be positive")
	}
	if strings.TrimSpace(c.S3.Region) == "" {
		return fmt.Errorf("SKOPOS_CHECKPOINT_S3_REGION: must not be empty")
	}
	return nil
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("SKOPOS_CHECKPOINT_LOG_LEVEL: unsupported level %q", raw)
	}
}

func stringEnv(lookup func(string) (string, bool), key, fallback string) (string, error) {
	if value, ok := lookup(key); ok {
		return value, nil
	}
	return fallback, nil
}

func int64Env(lookup func(string) (string, bool), key string, fallback int64) (int64, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}

func durationEnv(lookup func(string) (string, bool), key string, fallback time.Duration) (time.Duration, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}

func boolEnv(lookup func(string) (string, bool), key string, fallback bool) (bool, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}
