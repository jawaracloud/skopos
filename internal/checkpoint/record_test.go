package checkpoint

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestRecordRoundTrip(t *testing.T) {
	payloads := []struct {
		signal  Signal
		payload []byte
	}{
		{SignalTraces, []byte("trace-payload")},
		{SignalMetrics, bytes.Repeat([]byte{0x01, 0x02, 0x03}, 1000)},
		{SignalLogs, []byte("log payload")},
	}

	var framed []byte
	for _, item := range payloads {
		record, err := encodeRecord(item.signal, item.payload)
		if err != nil {
			t.Fatalf("encodeRecord(%s): %v", item.signal, err)
		}
		framed = append(framed, record...)
	}

	got, err := decodeRecords(framed)
	if err != nil {
		t.Fatalf("decodeRecords: %v", err)
	}
	if len(got) != len(payloads) {
		t.Fatalf("got %d records, want %d", len(got), len(payloads))
	}
	for i, want := range payloads {
		if got[i].Signal != want.signal {
			t.Errorf("record %d signal = %s, want %s", i, got[i].Signal, want.signal)
		}
		if !bytes.Equal(got[i].Payload, want.payload) {
			t.Errorf("record %d payload mismatch", i)
		}
	}
}

func TestDecodeRecordsToleratesTornTail(t *testing.T) {
	record, err := encodeRecord(SignalTraces, []byte("complete"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := encodeRecord(SignalLogs, []byte("torn"))
	if err != nil {
		t.Fatal(err)
	}

	for cut := 1; cut < len(second); cut++ {
		data := append(append([]byte(nil), record...), second[:cut]...)
		got, err := decodeRecords(data)
		if err != nil {
			t.Fatalf("cut %d: decodeRecords: %v", cut, err)
		}
		if len(got) != 1 || string(got[0].Payload) != "complete" {
			t.Fatalf("cut %d: got %d records, want only the complete one", cut, len(got))
		}
	}
}

func TestDecodeRecordsRejectsCorruption(t *testing.T) {
	record, err := encodeRecord(SignalTraces, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("crc", func(t *testing.T) {
		corrupt := append([]byte(nil), record...)
		corrupt[recordHeaderSize] ^= 0xff
		if _, err := decodeRecords(corrupt); !errors.Is(err, ErrCorruptRecord) {
			t.Fatalf("got %v, want ErrCorruptRecord", err)
		}
	})

	t.Run("magic", func(t *testing.T) {
		corrupt := append([]byte(nil), record...)
		corrupt[0] = 0
		if _, err := decodeRecords(corrupt); !errors.Is(err, ErrCorruptRecord) {
			t.Fatalf("got %v, want ErrCorruptRecord", err)
		}
	})

	t.Run("version", func(t *testing.T) {
		corrupt := append([]byte(nil), record...)
		corrupt[4] = 99
		if _, err := decodeRecords(corrupt); !errors.Is(err, ErrCorruptRecord) {
			t.Fatalf("got %v, want ErrCorruptRecord", err)
		}
	})

	t.Run("signal", func(t *testing.T) {
		corrupt := append([]byte(nil), record...)
		corrupt[5] = 99
		if _, err := decodeRecords(corrupt); !errors.Is(err, ErrCorruptRecord) {
			t.Fatalf("got %v, want ErrCorruptRecord", err)
		}
	})

	t.Run("length", func(t *testing.T) {
		header := make([]byte, recordHeaderSize)
		binary.BigEndian.PutUint32(header[0:4], recordMagic)
		header[4] = recordVersion
		header[5] = byte(SignalTraces)
		binary.BigEndian.PutUint32(header[8:12], MaxRecordBytes+1)
		if _, err := decodeRecords(header); !errors.Is(err, ErrCorruptRecord) {
			t.Fatalf("got %v, want ErrCorruptRecord", err)
		}
	})
}

func TestEncodeRecordValidation(t *testing.T) {
	if _, err := encodeRecord(SignalUnknown, []byte("x")); err == nil {
		t.Error("expected unknown-signal error")
	}
	if _, err := encodeRecord(SignalTraces, nil); err == nil {
		t.Error("expected empty-payload error")
	}
	if _, err := encodeRecord(SignalTraces, make([]byte, MaxRecordBytes+1)); err == nil {
		t.Error("expected oversized-payload error")
	}
}

func TestParseSignalPath(t *testing.T) {
	for _, signal := range []Signal{SignalTraces, SignalMetrics, SignalLogs} {
		got, ok := ParseSignalPath(signal.Path())
		if !ok || got != signal {
			t.Errorf("ParseSignalPath(%q) = %s, %v", signal.Path(), got, ok)
		}
	}
	if _, ok := ParseSignalPath("/v1/profiles"); ok {
		t.Error("unexpected signal for /v1/profiles")
	}
}

func FuzzDecodeRecords(f *testing.F) {
	valid, _ := encodeRecord(SignalTraces, []byte("seed"))
	f.Add(valid)
	f.Add([]byte{})
	f.Add([]byte("garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		// The decoder must never panic and must either return records or an
		// explicit corruption error.
		_, _ = decodeRecords(data)
	})
}
