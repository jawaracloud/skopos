package checkpoint

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type uploadResult struct {
	segments        int
	rawBytes        int64
	compressedBytes int64
}

// uploader seals nothing: it only ships segments the WAL already sealed. It
// uploads oldest-first and is safe to call from the maintenance goroutine and
// from Flush concurrently.
type uploader struct {
	wal    *WAL
	s3     *S3
	prefix string

	drainMu sync.Mutex
}

func newUploader(wal *WAL, s3 *S3, prefix string) *uploader {
	return &uploader{wal: wal, s3: s3, prefix: prefix}
}

// drain uploads every currently sealed segment. It stops at the first failure
// so the retry loop preserves order and does not fan out requests during an
// outage.
func (u *uploader) drain(ctx context.Context) (uploadResult, error) {
	u.drainMu.Lock()
	defer u.drainMu.Unlock()

	var result uploadResult
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		names := u.wal.Sealed()
		if len(names) == 0 {
			return result, nil
		}
		name := names[0]
		rawBytes, compressedBytes, err := u.upload(ctx, name)
		if err != nil {
			return result, err
		}
		if err := u.wal.Remove(name); err != nil {
			return result, err
		}
		result.segments++
		result.rawBytes += rawBytes
		result.compressedBytes += compressedBytes
	}
}

func (u *uploader) upload(ctx context.Context, name string) (int64, int64, error) {
	path := filepath.Join(u.wal.dir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("checkpoint: read sealed segment %s: %w", name, err)
	}
	if len(raw) == 0 {
		// An empty sealed segment should not exist, but deleting it is safer
		// than retrying forever.
		return 0, 0, nil
	}

	var compressed bytes.Buffer
	compressed.Grow(len(raw)/2 + 64)
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(raw); err != nil {
		return 0, 0, fmt.Errorf("checkpoint: compress segment %s: %w", name, err)
	}
	if err := zw.Close(); err != nil {
		return 0, 0, fmt.Errorf("checkpoint: compress segment %s: %w", name, err)
	}

	if err := u.s3.Put(ctx, u.objectKey(name), compressed.Bytes()); err != nil {
		return 0, 0, fmt.Errorf("checkpoint: upload segment %s: %w", name, err)
	}
	return int64(len(raw)), int64(compressed.Len()), nil
}

// objectKey is deterministic for a segment name so a retry after a successful
// PUT but failed local delete overwrites the same object instead of creating a
// duplicate.
func (u *uploader) objectKey(name string) string {
	timestamp, ok := segmentTimestamp(name)
	if !ok {
		timestamp = time.Now().UTC()
	}

	parts := make([]string, 0, 3)
	if prefix := strings.Trim(u.prefix, "/"); prefix != "" {
		parts = append(parts, prefix)
	}
	parts = append(parts, timestamp.Format("2006/01/02"), name+".gz")
	return strings.Join(parts, "/")
}

func nextBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return 250 * time.Millisecond
	}
	next := current * 2
	if next > 30*time.Second {
		return 30 * time.Second
	}
	return next
}
