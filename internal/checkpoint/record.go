// Package checkpoint implements the tiered write-ahead log that gives Skopos a
// recent-window durability checkpoint independent of ClickHouse.
//
// The design is deliberately stdlib-only (ADR 0001): the S3 client signs
// requests with AWS Signature Version 4 using crypto/hmac and crypto/sha256,
// segments are compressed with compress/gzip, and batches are uploaded with
// net/http. No third-party module is linked.
//
// A Checkpoint writes every OTLP/HTTP request body it receives into an
// append-only local segment. A background uploader seals a segment when it
// reaches SealBytes or SealInterval, gzips it, and PUTs it to an S3-compatible
// bucket. The local segment is deleted only after the PUT succeeds, so the
// checkpoint is the crash-consistency point for node loss, not the query path.
package checkpoint

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Signal identifies an OTLP/HTTP signal carried by a checkpoint record.
type Signal uint8

const (
	SignalUnknown Signal = 0
	SignalTraces  Signal = 1
	SignalMetrics Signal = 2
	SignalLogs    Signal = 3
)

// Valid reports whether s is a known OTLP signal.
func (s Signal) Valid() bool {
	switch s {
	case SignalTraces, SignalMetrics, SignalLogs:
		return true
	default:
		return false
	}
}

// Path returns the OTLP/HTTP path for the signal.
func (s Signal) Path() string {
	switch s {
	case SignalTraces:
		return "/v1/traces"
	case SignalMetrics:
		return "/v1/metrics"
	case SignalLogs:
		return "/v1/logs"
	default:
		return ""
	}
}

// String returns the lower-case signal name.
func (s Signal) String() string {
	switch s {
	case SignalTraces:
		return "traces"
	case SignalMetrics:
		return "metrics"
	case SignalLogs:
		return "logs"
	default:
		return "unknown"
	}
}

// ParseSignalPath maps an OTLP/HTTP path to a signal.
func ParseSignalPath(path string) (Signal, bool) {
	switch path {
	case "/v1/traces":
		return SignalTraces, true
	case "/v1/metrics":
		return SignalMetrics, true
	case "/v1/logs":
		return SignalLogs, true
	default:
		return SignalUnknown, false
	}
}

// MaxRecordBytes bounds one OTLP payload stored in a segment. The bound keeps a
// malformed or hostile request from exhausting memory during replay.
const MaxRecordBytes = 16 << 20

const (
	recordMagic       uint32 = 0x534B4350 // "SKCP"
	recordVersion     uint8  = 1
	recordHeaderSize         = 12
	recordTrailerSize        = 4
)

// recordCRCTable selects CRC32-C (Castagnoli): the standard library uses the
// hardware instruction on amd64 and arm64, and the polynomial detects the
// common burst errors better than IEEE CRC32.
var recordCRCTable = crc32.MakeTable(crc32.Castagnoli)

// ErrCorruptRecord reports a damaged segment payload.
var ErrCorruptRecord = errors.New("checkpoint: corrupt record")

// Record is one decoded OTLP payload.
type Record struct {
	Signal  Signal
	Payload []byte
}

// encodeRecord frames one payload:
//
//	[magic 4][version 1][signal 1][reserved 2][payload length 4][payload][crc32 4]
//
// All integers are big-endian. The CRC32-C covers the header and payload.
func encodeRecord(signal Signal, payload []byte) ([]byte, error) {
	if !signal.Valid() {
		return nil, fmt.Errorf("checkpoint: invalid signal %d", signal)
	}
	if len(payload) == 0 {
		return nil, errors.New("checkpoint: empty payload")
	}
	if len(payload) > MaxRecordBytes {
		return nil, fmt.Errorf("checkpoint: payload %d exceeds %d bytes", len(payload), MaxRecordBytes)
	}
	buf := make([]byte, recordHeaderSize+len(payload)+recordTrailerSize)
	binary.BigEndian.PutUint32(buf[0:4], recordMagic)
	buf[4] = recordVersion
	buf[5] = byte(signal)
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(payload)))
	copy(buf[recordHeaderSize:], payload)
	crc := crc32.Checksum(buf[:recordHeaderSize+len(payload)], recordCRCTable)
	binary.BigEndian.PutUint32(buf[recordHeaderSize+len(payload):], crc)
	return buf, nil
}

// decodeRecords parses a segment. A truncated final record is tolerated because
// a crash can tear the tail of an append; any other inconsistency is an error.
func decodeRecords(data []byte) ([]Record, error) {
	var out []Record
	for len(data) > 0 {
		if len(data) < recordHeaderSize {
			break // torn header at the tail
		}
		if magic := binary.BigEndian.Uint32(data[0:4]); magic != recordMagic {
			return nil, fmt.Errorf("%w: bad magic %#x", ErrCorruptRecord, magic)
		}
		if version := data[4]; version != recordVersion {
			return nil, fmt.Errorf("%w: unsupported version %d", ErrCorruptRecord, version)
		}
		signal := Signal(data[5])
		if !signal.Valid() {
			return nil, fmt.Errorf("%w: invalid signal %d", ErrCorruptRecord, signal)
		}
		length := int(binary.BigEndian.Uint32(data[8:12]))
		if length <= 0 || length > MaxRecordBytes {
			return nil, fmt.Errorf("%w: invalid payload length %d", ErrCorruptRecord, length)
		}
		total := recordHeaderSize + length + recordTrailerSize
		if len(data) < total {
			break // torn payload at the tail
		}
		want := binary.BigEndian.Uint32(data[recordHeaderSize+length : total])
		got := crc32.Checksum(data[:recordHeaderSize+length], recordCRCTable)
		if want != got {
			return nil, fmt.Errorf("%w: crc mismatch", ErrCorruptRecord)
		}
		payload := make([]byte, length)
		copy(payload, data[recordHeaderSize:recordHeaderSize+length])
		out = append(out, Record{Signal: signal, Payload: payload})
		data = data[total:]
	}
	return out, nil
}
