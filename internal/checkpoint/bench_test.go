package checkpoint

import (
	"bytes"
	"compress/gzip"
	"testing"
)

func BenchmarkEncodeRecord(b *testing.B) {
	payload := bytes.Repeat([]byte("telemetry"), 512) // 4 KiB
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := encodeRecord(SignalLogs, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeRecords(b *testing.B) {
	payload := bytes.Repeat([]byte("telemetry"), 512)
	framed, err := encodeRecord(SignalLogs, payload)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(framed)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := decodeRecords(framed); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGzipSegment(b *testing.B) {
	// A representative 8 MiB sealed segment with pseudo-text telemetry.
	raw := bytes.Repeat([]byte("0123456789abcdef"), (8<<20)/16)
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var compressed bytes.Buffer
		compressed.Grow(len(raw) / 4)
		zw := gzip.NewWriter(&compressed)
		if _, err := zw.Write(raw); err != nil {
			b.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
