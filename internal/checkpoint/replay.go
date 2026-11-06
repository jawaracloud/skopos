package checkpoint

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// maxSegmentBytes bounds a decompressed segment during replay. A normal
// segment is SealBytes plus at most one record; the guard catches a hostile or
// accidental gzip bomb.
const maxSegmentBytes = 256 << 20

// ReplayConfig restores checkpoint objects into the Collector's OTLP/HTTP
// receiver. Replay is at-least-once; deduplication is an explicit follow-up.
type ReplayConfig struct {
	S3          *S3
	Prefix      string
	ForwardURL  string
	DeleteAfter bool
	HTTPClient  *http.Client
	Timeout     time.Duration
}

// ReplayStats summarizes one replay run.
type ReplayStats struct {
	Objects        int   `json:"objects"`
	Records        int   `json:"records"`
	PayloadBytes   int64 `json:"payload_bytes"`
	DeletedObjects int   `json:"deleted_objects"`
	SkippedObjects int   `json:"skipped_objects"`
}

// Replay lists every checkpoint object under Prefix, decompresses it, and
// re-injects each original OTLP payload through the Collector. Using the
// Collector keeps one canonical write path into ClickHouse instead of
// duplicating the exporter's OTLP-to-schema mapping here.
func Replay(ctx context.Context, cfg ReplayConfig) (ReplayStats, error) {
	var stats ReplayStats
	if cfg.S3 == nil {
		return stats, errors.New("checkpoint: replay requires an S3 client")
	}
	if strings.TrimSpace(cfg.ForwardURL) == "" {
		return stats, errors.New("checkpoint: replay requires a forward URL")
	}
	base, err := url.Parse(cfg.ForwardURL)
	if err != nil {
		return stats, fmt.Errorf("checkpoint: replay forward URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return stats, fmt.Errorf("checkpoint: replay forward URL %q must use http or https", cfg.ForwardURL)
	}
	if base.Host == "" {
		return stats, fmt.Errorf("checkpoint: replay forward URL %q has no host", cfg.ForwardURL)
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return stats, fmt.Errorf("checkpoint: replay forward URL %q must not contain a query or fragment", cfg.ForwardURL)
	}
	base.Path = strings.TrimRight(base.Path, "/")

	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}

	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	objects, err := cfg.S3.List(ctx, prefix)
	if err != nil {
		return stats, err
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })

	for _, object := range objects {
		if !strings.HasSuffix(object.Key, sealedSuffix+".gz") {
			stats.SkippedObjects++
			continue
		}
		data, err := cfg.S3.Get(ctx, object.Key)
		if err != nil {
			return stats, err
		}
		records, err := decodeSegment(data)
		if err != nil {
			return stats, fmt.Errorf("checkpoint: replay %s: %w", object.Key, err)
		}
		for _, record := range records {
			if err := postRecord(ctx, client, base, record); err != nil {
				return stats, fmt.Errorf("checkpoint: replay %s (%s): %w", object.Key, record.Signal, err)
			}
			stats.Records++
			stats.PayloadBytes += int64(len(record.Payload))
		}
		stats.Objects++
		if cfg.DeleteAfter {
			if err := cfg.S3.Delete(ctx, object.Key); err != nil {
				return stats, fmt.Errorf("checkpoint: delete replayed object %s: %w", object.Key, err)
			}
			stats.DeletedObjects++
		}
	}
	return stats, nil
}

func postRecord(ctx context.Context, client *http.Client, base *url.URL, record Record) error {
	u := *base
	u.Path = joinURLPath(base.Path, record.Signal.Path())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(record.Payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxS3ErrorBytes))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := strings.TrimSpace(string(body))
		if message == "" {
			return fmt.Errorf("collector returned HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("collector returned HTTP %d: %s", resp.StatusCode, message)
	}
	return nil
}

func decodeSegment(data []byte) ([]Record, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer zr.Close()

	raw, err := io.ReadAll(io.LimitReader(zr, maxSegmentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	if len(raw) > maxSegmentBytes {
		return nil, fmt.Errorf("segment exceeds %d decompressed bytes", maxSegmentBytes)
	}
	return decodeRecords(raw)
}
