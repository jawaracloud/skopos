package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults chosen from the seal policy derivation in docs/checkpoint.md:
//
//	B* = min(B_max, lambda * W)
//	T* = B* / lambda <= W
//
// B_max is 8 MiB (one S3 object), W is the 15 s loss-window ceiling, and the
// 1 GiB local budget bounds an upload outage to MaxLocalBytes/lambda before
// backpressure. At 1 MiB/s that is about 17 minutes, with no silent drops.
const (
	DefaultSealBytes     int64 = 8 << 20
	DefaultSealInterval        = 15 * time.Second
	DefaultMaxLocalBytes int64 = 1 << 30
	DefaultFlushTimeout        = 15 * time.Second

	// MinSealInterval keeps the polling sealer able to check in time for the
	// documented bound. A smaller interval would force a sub-millisecond
	// ticker, which is neither useful for a disk-backed WAL nor kind to the
	// scheduler.
	MinSealInterval = time.Millisecond

	// MaxSealBytes keeps one segment plus its worst-case record well inside
	// the replay object bound, including gzip expansion for incompressible
	// data. The default is far below it.
	MaxSealBytes int64 = 200 << 20

	// minFramedRecordBytes is the smallest record the WAL can ever accept:
	// the 12-byte header, at least one payload byte, and the 4-byte CRC.
	minFramedRecordBytes = recordHeaderSize + 1 + recordTrailerSize
)

// Config configures a Checkpoint. S3 may be nil for local-only tests; a
// deployed checkpoint requires it, which the service validates.
type Config struct {
	Dir           string
	SealBytes     int64
	SealInterval  time.Duration
	MaxLocalBytes int64
	FlushTimeout  time.Duration
	S3            *S3Config
	Prefix        string
	Logger        *slog.Logger
}

// Checkpoint is the tiered WAL: a fast local append path plus a background
// gzip uploader to S3.
type Checkpoint struct {
	cfg      Config
	wal      *WAL
	uploader *uploader
	logger   *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	sealWake   chan struct{}
	uploadWake chan struct{}

	// lifecycle keeps Close from tearing down the WAL while an Append is
	// still writing. closeOnce makes concurrent Close calls wait for the same
	// final flush.
	lifecycle sync.RWMutex
	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
	stats     counters
}

type counters struct {
	records          atomic.Uint64
	payloadBytes     atomic.Uint64
	segmentsSealed   atomic.Uint64
	segmentsUploaded atomic.Uint64
	rawBytesUploaded atomic.Uint64
	compressedBytes  atomic.Uint64
	uploadFailures   atomic.Uint64
}

// Stats is a point-in-time snapshot for /stats and /readyz.
type Stats struct {
	RecordsAppended         uint64 `json:"records_appended"`
	PayloadBytesAppended    uint64 `json:"payload_bytes_appended"`
	SegmentsSealed          uint64 `json:"segments_sealed"`
	SegmentsUploaded        uint64 `json:"segments_uploaded"`
	RawBytesUploaded        uint64 `json:"raw_bytes_uploaded"`
	CompressedBytesUploaded uint64 `json:"compressed_bytes_uploaded"`
	UploadFailures          uint64 `json:"upload_failures"`
	ActiveBytes             int64  `json:"active_bytes"`
	SealedBytes             int64  `json:"sealed_bytes"`
	LocalBytes              int64  `json:"local_bytes"`
	SealedSegments          int    `json:"sealed_segments"`
	MaxLocalBytes           int64  `json:"max_local_bytes"`
	S3Enabled               bool   `json:"s3_enabled"`
	Closed                  bool   `json:"closed"`
}

// Open validates cfg, recovers any local segments, and starts the maintenance
// goroutines.
func Open(cfg Config) (*Checkpoint, error) {
	if cfg.SealBytes == 0 {
		cfg.SealBytes = DefaultSealBytes
	}
	if cfg.SealBytes < 0 {
		return nil, errors.New("checkpoint: SealBytes must be positive")
	}
	if cfg.SealBytes > MaxSealBytes {
		return nil, fmt.Errorf("checkpoint: SealBytes must not exceed %d", MaxSealBytes)
	}
	if cfg.SealInterval == 0 {
		cfg.SealInterval = DefaultSealInterval
	}
	if cfg.SealInterval < MinSealInterval {
		return nil, fmt.Errorf("checkpoint: SealInterval must be at least %s", MinSealInterval)
	}
	if cfg.MaxLocalBytes == 0 {
		cfg.MaxLocalBytes = DefaultMaxLocalBytes
	}
	if cfg.MaxLocalBytes < 0 {
		return nil, errors.New("checkpoint: MaxLocalBytes must be positive")
	}
	if cfg.FlushTimeout == 0 {
		cfg.FlushTimeout = DefaultFlushTimeout
	}
	if cfg.FlushTimeout < 0 {
		return nil, errors.New("checkpoint: FlushTimeout must be positive")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	var s3 *S3
	if cfg.S3 != nil {
		var err error
		s3, err = NewS3(*cfg.S3)
		if err != nil {
			return nil, err
		}
	}

	wal, err := OpenWAL(WALConfig{
		Dir:           cfg.Dir,
		SealBytes:     cfg.SealBytes,
		MaxLocalBytes: cfg.MaxLocalBytes,
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Checkpoint{
		cfg:        cfg,
		wal:        wal,
		logger:     cfg.Logger,
		ctx:        ctx,
		cancel:     cancel,
		sealWake:   make(chan struct{}, 1),
		uploadWake: make(chan struct{}, 1),
	}
	if s3 != nil {
		c.uploader = newUploader(wal, s3, cfg.Prefix)
	}

	c.wg.Add(1)
	go c.runSealer()
	if c.uploader != nil {
		c.wg.Add(1)
		go c.runUploader()
	}
	return c, nil
}

// Append records one OTLP/HTTP payload. The payload is acknowledged after the
// local write; S3 is the crash-consistency point, with the loss window
// documented in docs/checkpoint.md. ErrBackpressure must be surfaced to the
// sender, never swallowed by dropping the payload.
func (c *Checkpoint) Append(signal Signal, payload []byte) error {
	c.lifecycle.RLock()
	defer c.lifecycle.RUnlock()
	if c.closed.Load() {
		return ErrClosed
	}

	record, err := encodeRecord(signal, payload)
	if err != nil {
		return err
	}
	if err := c.wal.Append(record); err != nil {
		return err
	}
	c.stats.records.Add(1)
	c.stats.payloadBytes.Add(uint64(len(payload)))

	if c.wal.ActiveSize() >= c.cfg.SealBytes {
		select {
		case c.sealWake <- struct{}{}:
		default:
		}
	}
	return nil
}

// Flush seals the active segment and uploads every sealed segment. It is used
// by Close and is safe to call concurrently with the maintenance goroutines.
func (c *Checkpoint) Flush(ctx context.Context) error {
	name, sealErr := c.wal.Seal()
	if name != "" {
		c.stats.segmentsSealed.Add(1)
	}
	// A seal can succeed and still return an error if reopening the next
	// active segment failed. Upload the sealed segment before propagating the
	// error; otherwise the final flush would leave recoverable data local.
	if c.uploader == nil || errors.Is(sealErr, ErrClosed) {
		return sealErr
	}
	result, drainErr := c.uploader.drain(ctx)
	c.recordUpload(result)
	return errors.Join(sealErr, drainErr)
}

// Stats returns a snapshot of counters and local WAL usage.
func (c *Checkpoint) Stats() Stats {
	return Stats{
		RecordsAppended:         c.stats.records.Load(),
		PayloadBytesAppended:    c.stats.payloadBytes.Load(),
		SegmentsSealed:          c.stats.segmentsSealed.Load(),
		SegmentsUploaded:        c.stats.segmentsUploaded.Load(),
		RawBytesUploaded:        c.stats.rawBytesUploaded.Load(),
		CompressedBytesUploaded: c.stats.compressedBytes.Load(),
		UploadFailures:          c.stats.uploadFailures.Load(),
		ActiveBytes:             c.wal.ActiveSize(),
		SealedBytes:             c.wal.SealedBytes(),
		LocalBytes:              c.wal.LocalBytes(),
		SealedSegments:          c.wal.SealedSegments(),
		MaxLocalBytes:           c.cfg.MaxLocalBytes,
		S3Enabled:               c.uploader != nil,
		Closed:                  c.closed.Load(),
	}
}

// Ready reports whether the checkpoint can accept more telemetry.
func (c *Checkpoint) Ready() error {
	if c.closed.Load() {
		return ErrClosed
	}
	// Report not-ready as soon as no minimum-sized record could fit, not only
	// after the byte counter reaches the limit exactly.
	if c.wal.LocalBytes()+minFramedRecordBytes > c.cfg.MaxLocalBytes {
		return ErrBackpressure
	}
	return nil
}

// Close stops accepting telemetry, seals the active segment, makes one bounded
// attempt to upload everything, and closes the WAL. Segments that could not be
// uploaded stay on disk and are retried the next time the checkpoint opens.
// Concurrent callers wait for the same close and see the same error.
func (c *Checkpoint) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.close()
	})
	return c.closeErr
}

func (c *Checkpoint) close() error {
	c.closed.Store(true)
	c.cancel()
	c.wg.Wait()

	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.FlushTimeout)
	defer cancel()
	flushErr := c.Flush(ctx)
	if flushErr != nil && !errors.Is(flushErr, ErrClosed) {
		c.logger.Warn("checkpoint: final upload incomplete; segments remain on disk",
			"err", flushErr,
			"sealed_segments", c.wal.SealedSegments(),
			"local_bytes", c.wal.LocalBytes(),
		)
	}
	closeErr := c.wal.Close()
	return errors.Join(flushErr, closeErr)
}

func (c *Checkpoint) runSealer() {
	defer c.wg.Done()

	tick := c.cfg.SealInterval / 4
	if tick < time.Millisecond {
		tick = time.Millisecond
	}
	if tick > time.Second {
		tick = time.Second
	}
	// A polling sealer fires at most tick after its deadline. Checking one
	// tick early keeps the actual segment age at or below SealInterval, so
	// the documented loss window is still W rather than W+tick.
	sealAfter := c.cfg.SealInterval - tick
	if sealAfter <= 0 {
		sealAfter = c.cfg.SealInterval / 2
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.sealWake:
		case <-ticker.C:
		}

		name, err := c.wal.SealIfDue(sealAfter)
		if name != "" {
			c.stats.segmentsSealed.Add(1)
			c.wakeUpload()
		}
		if err != nil && !errors.Is(err, ErrClosed) && c.ctx.Err() == nil {
			c.logger.Error("checkpoint: seal failed", "err", err)
		}
	}
}

func (c *Checkpoint) runUploader() {
	defer c.wg.Done()

	backoff := time.Duration(0)
	first := true
	for {
		if first {
			first = false
		} else {
			select {
			case <-c.ctx.Done():
				return
			case <-c.uploadWake:
			}
		}

		for {
			if c.ctx.Err() != nil {
				return
			}
			result, err := c.uploader.drain(c.ctx)
			c.recordUpload(result)
			if err == nil {
				backoff = 0
				break
			}
			if c.ctx.Err() != nil {
				return
			}
			c.stats.uploadFailures.Add(1)
			backoff = nextBackoff(backoff)
			c.logger.Warn("checkpoint: upload failed; retaining local segments",
				"err", err,
				"retry_in", backoff,
				"sealed_segments", c.wal.SealedSegments(),
				"local_bytes", c.wal.LocalBytes(),
			)
			if !c.waitRetry(backoff) {
				return
			}
		}
	}
}

// waitRetry sleeps for d while draining upload wake-ups, so a burst of seals
// during an outage does not bypass the backoff and hammer S3.
func (c *Checkpoint) waitRetry(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-c.uploadWake:
		}
	}
}

func (c *Checkpoint) wakeUpload() {
	select {
	case c.uploadWake <- struct{}{}:
	default:
	}
}

func (c *Checkpoint) recordUpload(result uploadResult) {
	if result.segments == 0 {
		return
	}
	c.stats.segmentsUploaded.Add(uint64(result.segments))
	c.stats.rawBytesUploaded.Add(uint64(result.rawBytes))
	c.stats.compressedBytes.Add(uint64(result.compressedBytes))
}
