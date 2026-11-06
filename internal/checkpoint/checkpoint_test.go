package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testCheckpointConfig(t *testing.T, endpoint string) Config {
	t.Helper()
	return Config{
		Dir:           t.TempDir(),
		SealBytes:     128,
		SealInterval:  20 * time.Millisecond,
		MaxLocalBytes: 1 << 20,
		FlushTimeout:  2 * time.Second,
		Prefix:        "checkpoints",
		Logger:        discardLogger(),
		S3: &S3Config{
			Endpoint:  endpoint,
			Region:    "us-east-1",
			Bucket:    "bucket",
			AccessKey: "access",
			SecretKey: "secret",
		},
	}
}

func TestCheckpointUploadsSealedSegment(t *testing.T) {
	server := newFakeS3(t)
	cfg := testCheckpointConfig(t, server.URL)
	cp, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}

	payloads := [][]byte{
		bytes.Repeat([]byte{0x01}, 100),
		bytes.Repeat([]byte{0x02}, 100),
	}
	for _, payload := range payloads {
		if err := cp.Append(SignalTraces, payload); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	key, data := waitForObject(t, server.store, cfg.Prefix+"/")
	records, err := decodeSegment(data)
	if err != nil {
		t.Fatalf("decode uploaded segment: %v", err)
	}
	if len(records) != len(payloads) {
		t.Fatalf("uploaded %d records, want %d", len(records), len(payloads))
	}
	for i := range payloads {
		if records[i].Signal != SignalTraces || !bytes.Equal(records[i].Payload, payloads[i]) {
			t.Fatalf("record %d mismatch", i)
		}
	}
	if !strings.HasPrefix(key, cfg.Prefix+"/") || !strings.HasSuffix(key, ".wal.gz") {
		t.Fatalf("unexpected object key %q", key)
	}

	waitForStats(t, cp, func(s Stats) bool {
		return s.SegmentsUploaded >= 1 && s.SegmentsSealed >= 1
	})
	stats := cp.Stats()
	if stats.RecordsAppended != 2 {
		t.Fatalf("RecordsAppended = %d, want 2", stats.RecordsAppended)
	}
	if stats.PayloadBytesAppended != 200 {
		t.Fatalf("PayloadBytesAppended = %d, want 200", stats.PayloadBytesAppended)
	}
	if stats.CompressedBytesUploaded == 0 || stats.RawBytesUploaded == 0 {
		t.Fatalf("upload byte counters are zero: %+v", stats)
	}

	if err := cp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !cp.Stats().Closed {
		t.Fatal("Stats().Closed = false after Close")
	}
	if err := cp.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestCheckpointRetriesUpload(t *testing.T) {
	server := newFakeS3(t)
	server.store.failPuts.Store(2)
	cfg := testCheckpointConfig(t, server.URL)
	cp, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()

	payload := bytes.Repeat([]byte{0x03}, 200)
	if err := cp.Append(SignalLogs, payload); err != nil {
		t.Fatal(err)
	}

	key, data := waitForObject(t, server.store, cfg.Prefix+"/")
	if len(data) == 0 {
		t.Fatalf("object %q is empty", key)
	}
	waitForStats(t, cp, func(s Stats) bool {
		return s.SegmentsUploaded >= 1 && s.UploadFailures >= 1
	})
}

func TestCheckpointLocalRecovery(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Dir:           dir,
		SealBytes:     64,
		SealInterval:  20 * time.Millisecond,
		MaxLocalBytes: 1 << 20,
		FlushTimeout:  time.Second,
		Logger:        discardLogger(),
	}

	cp, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Append(SignalMetrics, bytes.Repeat([]byte{0x04}, 60)); err != nil {
		t.Fatal(err)
	}
	if err := cp.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := cp.Stats().SealedSegments; got != 1 {
		t.Fatalf("SealedSegments = %d, want 1", got)
	}
	if err := cp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := reopened.Stats().SealedSegments; got != 1 {
		t.Fatalf("reopened SealedSegments = %d, want 1", got)
	}
	if got := reopened.Stats().RecordsAppended; got != 0 {
		t.Fatalf("reopened RecordsAppended = %d, want 0", got)
	}
}

func TestCheckpointBackpressure(t *testing.T) {
	cfg := Config{
		Dir:           t.TempDir(),
		SealBytes:     100,
		SealInterval:  time.Hour,
		MaxLocalBytes: 200,
		FlushTimeout:  time.Second,
		Logger:        discardLogger(),
	}
	cp, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()

	payload := bytes.Repeat([]byte{0x05}, 80)
	var backpressure error
	for i := 0; i < 10; i++ {
		if err := cp.Append(SignalLogs, payload); err != nil {
			backpressure = err
			break
		}
	}
	if !errors.Is(backpressure, ErrBackpressure) {
		t.Fatalf("append loop error = %v, want ErrBackpressure", backpressure)
	}
	if err := cp.Ready(); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("Ready = %v, want ErrBackpressure", err)
	}
}

func TestCheckpointValidation(t *testing.T) {
	tests := []Config{
		{Dir: t.TempDir(), SealBytes: 100, MaxLocalBytes: 50},
		{Dir: t.TempDir(), SealBytes: MaxSealBytes + 1, MaxLocalBytes: MaxSealBytes + 2},
		{Dir: t.TempDir(), SealInterval: -time.Second},
		{Dir: t.TempDir(), SealInterval: MinSealInterval / 2},
		{Dir: t.TempDir(), FlushTimeout: -time.Second},
		{Dir: t.TempDir(), S3: &S3Config{}},
	}
	for _, cfg := range tests {
		if _, err := Open(cfg); err == nil {
			t.Errorf("Open(%+v) succeeded, want error", cfg)
		}
	}
}

func waitForObject(t *testing.T, store *fakeS3, prefix string) (string, []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, key := range store.keys() {
			if strings.HasPrefix(key, prefix) {
				data, _ := store.get(key)
				return key, data
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no object appeared under %q", prefix)
	return "", nil
}

func waitForStats(t *testing.T, cp *Checkpoint, ok func(Stats) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok(cp.Stats()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stats condition not met: %+v", cp.Stats())
}
