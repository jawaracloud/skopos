package checkpoint

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeS3 is a minimal path-style S3 double: one bucket named "bucket", page
// size two, and an optional PUT failure budget for retry tests.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	lastAuth string
	lastSHA  string

	failPuts atomic.Int32
	puts     atomic.Int64
}

type fakeS3Server struct {
	*httptest.Server
	store *fakeS3
}

func newFakeS3(t *testing.T) *fakeS3Server {
	t.Helper()
	store := &fakeS3{objects: make(map[string][]byte)}
	server := httptest.NewServer(http.HandlerFunc(store.handle))
	t.Cleanup(server.Close)
	return &fakeS3Server{Server: server, store: store}
}

func (f *fakeS3) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	f.lastSHA = r.Header.Get("x-amz-content-sha256")
	f.mu.Unlock()

	key := strings.TrimPrefix(r.URL.Path, "/bucket")
	key = strings.TrimPrefix(key, "/")

	if r.URL.Query().Get("list-type") == "2" {
		f.list(w, r)
		return
	}

	switch r.Method {
	case http.MethodPut:
		if remaining := f.failPuts.Load(); remaining > 0 {
			f.failPuts.Add(-1)
			writeS3Error(w, http.StatusInternalServerError, "InternalError", "injected failure")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeS3Error(w, http.StatusBadRequest, "InvalidRequest", err.Error())
			return
		}
		f.mu.Lock()
		f.objects[key] = body
		f.mu.Unlock()
		f.puts.Add(1)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		f.mu.Lock()
		data, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey", "the specified key does not exist")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

type fakeListResult struct {
	XMLName               xml.Name         `xml:"ListBucketResult"`
	IsTruncated           bool             `xml:"IsTruncated"`
	NextContinuationToken string           `xml:"NextContinuationToken,omitempty"`
	Contents              []fakeListObject `xml:"Contents"`
}

type fakeListObject struct {
	Key          string `xml:"Key"`
	Size         int    `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	start := 0
	if token := r.URL.Query().Get("continuation-token"); token != "" {
		parsed, err := strconv.Atoi(token)
		if err != nil || parsed < 0 {
			writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "bad continuation token")
			return
		}
		start = parsed
	}

	f.mu.Lock()
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	end := start + 2
	if end > len(keys) {
		end = len(keys)
	}
	if start > len(keys) {
		start = len(keys)
	}

	result := fakeListResult{IsTruncated: end < len(keys)}
	if result.IsTruncated {
		result.NextContinuationToken = strconv.Itoa(end)
	}
	for _, key := range keys[start:end] {
		result.Contents = append(result.Contents, fakeListObject{
			Key:          key,
			Size:         len(f.objects[key]),
			LastModified: time.Now().UTC().Format(time.RFC3339),
		})
	}
	f.mu.Unlock()

	body, err := xml.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	body := fmt.Sprintf("<Error><Code>%s</Code><Message>%s</Message></Error>", code, message)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *fakeS3) put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), data...)
}

func (f *fakeS3) get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	return append([]byte(nil), data...), ok
}

func (f *fakeS3) delete(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
}

func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (f *fakeS3) auth() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth, f.lastSHA
}
