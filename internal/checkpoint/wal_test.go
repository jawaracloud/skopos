package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWALSealBySize(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(WALConfig{Dir: dir, SealBytes: 8, MaxLocalBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.Append([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if got := w.ActiveSize(); got != 10 {
		t.Fatalf("ActiveSize = %d, want 10", got)
	}

	name, err := w.SealIfDue(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if name == "" {
		t.Fatal("expected a sealed segment")
	}
	if !isSealedName(name) {
		t.Fatalf("unexpected segment name %q", name)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("sealed segment missing: %v", err)
	}
	if got := w.ActiveSize(); got != 0 {
		t.Fatalf("ActiveSize after seal = %d, want 0", got)
	}
	if got := w.SealedBytes(); got != 10 {
		t.Fatalf("SealedBytes = %d, want 10", got)
	}
	if got := w.Sealed(); len(got) != 1 || got[0] != name {
		t.Fatalf("Sealed() = %v, want [%s]", got, name)
	}

	if err := w.Remove(name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("segment still exists: %v", err)
	}
	if got := w.SealedBytes(); got != 0 {
		t.Fatalf("SealedBytes after remove = %d, want 0", got)
	}
	// Removing twice is a no-op, which makes upload retries safe.
	if err := w.Remove(name); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func TestWALSealByInterval(t *testing.T) {
	w, err := OpenWAL(WALConfig{Dir: t.TempDir(), SealBytes: 1 << 20, MaxLocalBytes: 2 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.Append([]byte("small")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	name, err := w.SealIfDue(20 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if name == "" {
		t.Fatal("expected interval seal")
	}
}

func TestWALBackpressure(t *testing.T) {
	w, err := OpenWAL(WALConfig{Dir: t.TempDir(), SealBytes: 4, MaxLocalBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.Append([]byte("123456")); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := w.Append([]byte("123456")); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("second append = %v, want ErrBackpressure", err)
	}
	if got := w.LocalBytes(); got != 6 {
		t.Fatalf("LocalBytes = %d, want 6", got)
	}
}

func TestWALReopenRecoversActiveSegment(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(WALConfig{Dir: dir, SealBytes: 1 << 20, MaxLocalBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]byte("recovered")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w2, err := OpenWAL(WALConfig{Dir: dir, SealBytes: 1 << 20, MaxLocalBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	if got := w2.SealedSegments(); got != 1 {
		t.Fatalf("SealedSegments = %d, want 1", got)
	}
	names := w2.Sealed()
	data, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "recovered" {
		t.Fatalf("recovered data = %q", data)
	}
	if got := w2.ActiveSize(); got != 0 {
		t.Fatalf("ActiveSize after recovery = %d, want 0", got)
	}
}

func TestWALReopenAfterTornTail(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(WALConfig{Dir: dir, SealBytes: 1 << 20, MaxLocalBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]byte("complete")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dir, activeFileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("torn"); err != nil {
		t.Fatal(err)
	}
	file.Close()

	w2, err := OpenWAL(WALConfig{Dir: dir, SealBytes: 1 << 20, MaxLocalBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if got := w2.SealedBytes(); got != int64(len("completetorn")) {
		t.Fatalf("SealedBytes = %d, want %d", got, len("completetorn"))
	}
}

func TestWALClosed(t *testing.T) {
	w, err := OpenWAL(WALConfig{Dir: t.TempDir(), SealBytes: 1 << 20, MaxLocalBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Append = %v, want ErrClosed", err)
	}
	if _, err := w.Seal(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Seal = %v, want ErrClosed", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOpenWALValidation(t *testing.T) {
	tests := []WALConfig{
		{},
		{Dir: t.TempDir(), SealBytes: 0, MaxLocalBytes: 10},
		{Dir: t.TempDir(), SealBytes: 10, MaxLocalBytes: 0},
		{Dir: t.TempDir(), SealBytes: 20, MaxLocalBytes: 10},
	}
	for _, cfg := range tests {
		if _, err := OpenWAL(cfg); err == nil {
			t.Errorf("OpenWAL(%+v) succeeded, want error", cfg)
		}
	}
}
