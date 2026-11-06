package main

import (
	"testing"
	"time"

	"github.com/jawaracloud/skopos/internal/checkpoint"
)

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := configFromLookup(envLookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":4319" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.Dir != "/var/lib/skopos/checkpoint" {
		t.Errorf("Dir = %q", cfg.Dir)
	}
	if cfg.SealBytes != checkpoint.DefaultSealBytes {
		t.Errorf("SealBytes = %d", cfg.SealBytes)
	}
	if cfg.SealInterval != checkpoint.DefaultSealInterval {
		t.Errorf("SealInterval = %s", cfg.SealInterval)
	}
	if cfg.MaxLocalBytes != checkpoint.DefaultMaxLocalBytes {
		t.Errorf("MaxLocalBytes = %d", cfg.MaxLocalBytes)
	}
	if cfg.MaxRequestBytes != checkpoint.MaxRecordBytes {
		t.Errorf("MaxRequestBytes = %d", cfg.MaxRequestBytes)
	}
	if cfg.Prefix != "skopos/checkpoint" {
		t.Errorf("Prefix = %q", cfg.Prefix)
	}
	if err := cfg.ValidateServe(); err == nil {
		t.Error("ValidateServe succeeded without an S3 endpoint")
	}
	if err := cfg.ValidateReplay(); err == nil {
		t.Error("ValidateReplay succeeded without a forward URL")
	}
}

func TestConfigValidServe(t *testing.T) {
	cfg, err := configFromLookup(envLookup(map[string]string{
		"SKOPOS_CHECKPOINT_LISTEN_ADDR":       ":9999",
		"SKOPOS_CHECKPOINT_DIR":               "/tmp/checkpoint",
		"SKOPOS_CHECKPOINT_SEAL_BYTES":        "1048576",
		"SKOPOS_CHECKPOINT_SEAL_INTERVAL":     "5s",
		"SKOPOS_CHECKPOINT_MAX_LOCAL_BYTES":   "1073741824",
		"SKOPOS_CHECKPOINT_MAX_REQUEST_BYTES": "1048576",
		"SKOPOS_CHECKPOINT_S3_ENDPOINT":       "http://rustfs:9000",
		"SKOPOS_CHECKPOINT_S3_BUCKET":         "skopos-checkpoint",
		"SKOPOS_CHECKPOINT_S3_PREFIX":         "durable",
		"SKOPOS_CHECKPOINT_S3_ACCESS_KEY":     "access",
		"SKOPOS_CHECKPOINT_S3_SECRET_KEY":     "secret",
		"SKOPOS_CHECKPOINT_S3_VIRTUAL_HOST":   "true",
		"SKOPOS_CHECKPOINT_LOG_LEVEL":         "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateServe(); err != nil {
		t.Fatalf("ValidateServe: %v", err)
	}
	if cfg.ListenAddr != ":9999" || cfg.Dir != "/tmp/checkpoint" {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if cfg.SealBytes != 1<<20 || cfg.SealInterval != 5*time.Second {
		t.Errorf("unexpected seal config: %+v", cfg)
	}
	if !cfg.S3.VirtualHost || cfg.S3.AccessKey != "access" {
		t.Errorf("unexpected S3 config: %+v", cfg.S3)
	}
	if cfg.LogLevel.String() != "DEBUG" {
		t.Errorf("LogLevel = %s", cfg.LogLevel)
	}
}

func TestConfigValidReplay(t *testing.T) {
	cfg, err := configFromLookup(envLookup(map[string]string{
		"SKOPOS_CHECKPOINT_S3_ENDPOINT":    "http://rustfs:9000",
		"SKOPOS_CHECKPOINT_S3_BUCKET":      "skopos-checkpoint",
		"SKOPOS_CHECKPOINT_FORWARD_URL":    "http://otel-collector:4318",
		"SKOPOS_CHECKPOINT_REPLAY_TIMEOUT": "30s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateReplay(); err != nil {
		t.Fatalf("ValidateReplay: %v", err)
	}
	if cfg.ForwardURL != "http://otel-collector:4318" {
		t.Errorf("ForwardURL = %q", cfg.ForwardURL)
	}
	if cfg.ReplayTimeout != 30*time.Second {
		t.Errorf("ReplayTimeout = %s", cfg.ReplayTimeout)
	}
}

func TestConfigErrors(t *testing.T) {
	tests := map[string]map[string]string{
		"bad int":             {"SKOPOS_CHECKPOINT_SEAL_BYTES": "nope"},
		"bad duration":        {"SKOPOS_CHECKPOINT_SEAL_INTERVAL": "soon"},
		"bad bool":            {"SKOPOS_CHECKPOINT_S3_VIRTUAL_HOST": "maybe"},
		"bad log level":       {"SKOPOS_CHECKPOINT_LOG_LEVEL": "verbose"},
		"seal above local":    {"SKOPOS_CHECKPOINT_SEAL_BYTES": "2048", "SKOPOS_CHECKPOINT_MAX_LOCAL_BYTES": "1024"},
		"seal above replay":   {"SKOPOS_CHECKPOINT_SEAL_BYTES": "209715201", "SKOPOS_CHECKPOINT_MAX_LOCAL_BYTES": "209715202"},
		"negative seal":       {"SKOPOS_CHECKPOINT_SEAL_BYTES": "-1"},
		"short seal interval": {"SKOPOS_CHECKPOINT_SEAL_INTERVAL": "500us"},
		"request over record": {"SKOPOS_CHECKPOINT_MAX_REQUEST_BYTES": "16777217"},
	}
	for name, env := range tests {
		t.Run(name, func(t *testing.T) {
			cfg, err := configFromLookup(envLookup(env))
			if err == nil {
				err = cfg.ValidateServe()
			}
			if err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
}
