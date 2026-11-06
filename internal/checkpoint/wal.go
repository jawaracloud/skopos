package checkpoint

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	activeFileName = "active.wal"
	sealedPrefix   = "seg-"
	sealedSuffix   = ".wal"
)

var (
	// ErrBackpressure means appending would exceed MaxLocalBytes. The caller
	// must stop accepting telemetry rather than drop it silently.
	ErrBackpressure = errors.New("checkpoint: local WAL is full")
	// ErrClosed means the WAL has been closed and no longer accepts appends.
	ErrClosed = errors.New("checkpoint: WAL is closed")
)

// WALConfig configures the local append log.
type WALConfig struct {
	Dir           string
	SealBytes     int64
	MaxLocalBytes int64
}

// WAL is an append-only local log split into sealed segments. A segment is
// sealed when it reaches SealBytes or when SealIfDue sees an expired interval;
// a background owner (Checkpoint) is responsible for calling SealIfDue.
//
// Append does not fsync: the acknowledgement point is the local write, which
// survives a process crash through the kernel page cache. Seal fsyncs before
// rename so an uploaded segment is complete.
type WAL struct {
	dir           string
	sealBytes     int64
	maxLocalBytes int64
	nonce         string

	mu          sync.Mutex
	file        *os.File
	size        int64
	activeSince time.Time
	sealed      map[string]int64
	sealedBytes int64
	closed      bool
	failed      error
}

// OpenWAL opens or creates the WAL directory. An active segment left by a
// previous process is sealed so a torn tail can never become a corrupt middle
// record after restart.
func OpenWAL(cfg WALConfig) (*WAL, error) {
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, errors.New("checkpoint: WAL directory is required")
	}
	if cfg.SealBytes <= 0 {
		return nil, errors.New("checkpoint: SealBytes must be positive")
	}
	if cfg.MaxLocalBytes <= 0 {
		return nil, errors.New("checkpoint: MaxLocalBytes must be positive")
	}
	if cfg.SealBytes > cfg.MaxLocalBytes {
		return nil, errors.New("checkpoint: SealBytes must not exceed MaxLocalBytes")
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("checkpoint: create WAL directory: %w", err)
	}

	nonce, err := randomNonce()
	if err != nil {
		return nil, err
	}
	w := &WAL{
		dir:           cfg.Dir,
		sealBytes:     cfg.SealBytes,
		maxLocalBytes: cfg.MaxLocalBytes,
		nonce:         nonce,
		sealed:        make(map[string]int64),
	}

	entries, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: read WAL directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !isSealedName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("checkpoint: inspect %s: %w", entry.Name(), err)
		}
		w.sealed[entry.Name()] = info.Size()
		w.sealedBytes += info.Size()
	}

	activePath := filepath.Join(cfg.Dir, activeFileName)
	info, err := os.Stat(activePath)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return nil, fmt.Errorf("checkpoint: active segment is not a regular file")
	case err == nil && info.Size() > 0:
		name, nameErr := w.uniqueSealedName(time.Now())
		if nameErr != nil {
			return nil, nameErr
		}
		if err := os.Rename(activePath, filepath.Join(cfg.Dir, name)); err != nil {
			return nil, fmt.Errorf("checkpoint: recover active segment: %w", err)
		}
		w.sealed[name] = info.Size()
		w.sealedBytes += info.Size()
	case err == nil:
		if err := os.Remove(activePath); err != nil {
			return nil, fmt.Errorf("checkpoint: remove empty active segment: %w", err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// Fresh directory.
	default:
		return nil, fmt.Errorf("checkpoint: inspect active segment: %w", err)
	}

	if err := w.openActiveLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

// Append writes one framed record. The record must already be encoded.
func (w *WAL) Append(record []byte) error {
	if len(record) == 0 {
		return errors.New("checkpoint: empty WAL append")
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if w.failed != nil {
		return w.failed
	}
	if w.sealedBytes+w.size+int64(len(record)) > w.maxLocalBytes {
		return ErrBackpressure
	}
	if w.file == nil {
		if err := w.openActiveLocked(); err != nil {
			return err
		}
	}

	n, writeErr := w.file.Write(record)
	if writeErr == nil && n == len(record) {
		w.size += int64(n)
		return nil
	}
	if writeErr == nil {
		writeErr = io.ErrShortWrite
	}

	// Roll the file back so a failed append cannot leave a partial frame in
	// front of records that are appended later. If the rollback itself fails,
	// refuse further appends rather than risk an unexplained corrupt middle.
	if truncateErr := w.file.Truncate(w.size); truncateErr != nil {
		w.failed = fmt.Errorf("checkpoint: WAL write failed and rollback failed: %w",
			errors.Join(writeErr, truncateErr))
		return w.failed
	}
	return fmt.Errorf("checkpoint: append WAL: %w", writeErr)
}

// Seal closes the active segment and renames it to a sealed name. It returns
// the empty string when there is nothing to seal.
func (w *WAL) Seal() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return "", ErrClosed
	}
	return w.sealLocked()
}

// SealIfDue seals when the active segment reached SealBytes or has been open
// for at least interval. The caller must invoke it periodically and after a
// threshold-sized append.
func (w *WAL) SealIfDue(interval time.Duration) (string, error) {
	if interval <= 0 {
		return "", errors.New("checkpoint: seal interval must be positive")
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return "", ErrClosed
	}
	if w.file == nil || w.size == 0 {
		return "", nil
	}
	if w.size < w.sealBytes && time.Since(w.activeSince) < interval {
		return "", nil
	}
	return w.sealLocked()
}

// Sealed returns the sealed segment names, oldest first. Names embed a
// Unix-nanosecond timestamp, so lexical order is chronological order.
func (w *WAL) Sealed() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	names := make([]string, 0, len(w.sealed))
	for name := range w.sealed {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Remove deletes a sealed segment after it has been uploaded. Removing a
// segment that is already gone is not an error.
func (w *WAL) Remove(name string) error {
	if !isSealedName(name) {
		return fmt.Errorf("checkpoint: invalid sealed segment name %q", name)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	size, ok := w.sealed[name]
	if !ok {
		return nil
	}
	if err := os.Remove(filepath.Join(w.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checkpoint: remove sealed segment %s: %w", name, err)
	}
	delete(w.sealed, name)
	w.sealedBytes -= size
	if w.sealedBytes < 0 {
		w.sealedBytes = 0
	}
	return nil
}

// ActiveSize returns the current unsealed segment size.
func (w *WAL) ActiveSize() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

// SealedBytes returns the total size of segments waiting for upload.
func (w *WAL) SealedBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sealedBytes
}

// LocalBytes returns the durable local footprint: active plus sealed segments.
func (w *WAL) LocalBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size + w.sealedBytes
}

// SealedSegments returns the number of segments waiting for upload.
func (w *WAL) SealedSegments() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.sealed)
}

// Close syncs and closes the active file. It does not delete anything; a later
// OpenWAL recovers the active segment.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	syncErr := w.file.Sync()
	closeErr := w.file.Close()
	w.file = nil
	return errors.Join(syncErr, closeErr)
}

func (w *WAL) sealLocked() (string, error) {
	if w.file == nil || w.size == 0 {
		return "", nil
	}

	syncErr := w.file.Sync()
	closeErr := w.file.Close()
	w.file = nil
	if syncErr != nil || closeErr != nil {
		// Reopen so appends can continue; the segment stays active and the
		// next seal attempt retries.
		reopenErr := w.openActiveLocked()
		return "", errors.Join(syncErr, closeErr, reopenErr)
	}

	name, err := w.uniqueSealedName(time.Now())
	if err != nil {
		return "", errors.Join(err, w.openActiveLocked())
	}
	activePath := filepath.Join(w.dir, activeFileName)
	if err := os.Rename(activePath, filepath.Join(w.dir, name)); err != nil {
		return "", errors.Join(
			fmt.Errorf("checkpoint: seal segment: %w", err),
			w.openActiveLocked(),
		)
	}

	size := w.size
	w.sealed[name] = size
	w.sealedBytes += size
	w.size = 0

	if err := w.openActiveLocked(); err != nil {
		// The segment is sealed and uploadable; only new appends are blocked
		// until the active file can be reopened.
		return name, err
	}
	return name, nil
}

func (w *WAL) openActiveLocked() error {
	file, err := os.OpenFile(
		filepath.Join(w.dir, activeFileName),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("checkpoint: open active segment: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("checkpoint: inspect active segment: %w", err)
	}
	w.file = file
	w.size = info.Size()
	w.activeSince = time.Now()
	return nil
}

func (w *WAL) uniqueSealedName(now time.Time) (string, error) {
	base := now.UTC().UnixNano()
	for offset := int64(0); offset < 1000; offset++ {
		// The per-process nonce makes names unique across replicas that share
		// an S3 prefix; the timestamp keeps lexical order chronological.
		name := fmt.Sprintf("%s%d-%s%s", sealedPrefix, base+offset, w.nonce, sealedSuffix)
		_, err := os.Stat(filepath.Join(w.dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			return name, nil
		}
		if err != nil {
			return "", fmt.Errorf("checkpoint: allocate segment name: %w", err)
		}
	}
	return "", errors.New("checkpoint: cannot allocate a unique segment name")
}

func randomNonce() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("checkpoint: generate WAL nonce: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// segmentTimestamp reads the Unix-nanosecond component of a segment name.
func segmentTimestamp(name string) (time.Time, bool) {
	stem := strings.TrimSuffix(name, sealedSuffix)
	stem = strings.TrimPrefix(stem, sealedPrefix)
	stamp, _, _ := strings.Cut(stem, "-")
	nanos, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, nanos).UTC(), true
}

func isSealedName(name string) bool {
	if !strings.HasPrefix(name, sealedPrefix) || !strings.HasSuffix(name, sealedSuffix) {
		return false
	}
	return len(name) > len(sealedPrefix)+len(sealedSuffix)
}
