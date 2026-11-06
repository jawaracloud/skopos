package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func testS3Client(t *testing.T, endpoint string) *S3 {
	t.Helper()
	client, err := NewS3(S3Config{
		Endpoint:  endpoint,
		Region:    "us-east-1",
		Bucket:    "bucket",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestS3PutGetDelete(t *testing.T) {
	server := newFakeS3(t)
	client := testS3Client(t, server.URL)
	ctx := context.Background()
	key := "prefix/a b+c.txt"
	payload := []byte("checkpoint payload")

	if err := client.Put(ctx, key, payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, ok := server.store.get(key); !ok || !bytes.Equal(got, payload) {
		t.Fatalf("stored object = %q, %v", got, ok)
	}

	auth, contentSHA := server.store.auth()
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=test-access-key/") {
		t.Fatalf("missing SigV4 authorization: %q", auth)
	}
	if contentSHA != sha256Hex(payload) {
		t.Fatalf("x-amz-content-sha256 = %q, want %q", contentSHA, sha256Hex(payload))
	}

	got, err := client.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Get = %q, want %q", got, payload)
	}

	if err := client.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := server.store.get(key); ok {
		t.Fatal("object still exists after Delete")
	}
	if err := client.Delete(ctx, key); err != nil {
		t.Fatalf("idempotent Delete: %v", err)
	}
}

func TestS3NotFound(t *testing.T) {
	server := newFakeS3(t)
	client := testS3Client(t, server.URL)

	_, err := client.Get(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsS3NotFound(err) {
		t.Fatalf("IsS3NotFound(%v) = false", err)
	}
	var s3err *S3Error
	if !errors.As(err, &s3err) || s3err.Code != "NoSuchKey" {
		t.Fatalf("error = %#v, want NoSuchKey", err)
	}
}

func TestS3ListPagination(t *testing.T) {
	server := newFakeS3(t)
	client := testS3Client(t, server.URL)
	for _, key := range []string{"prefix/a", "prefix/b", "prefix/c", "prefix/d", "other/e"} {
		server.store.put(key, []byte(key))
	}

	objects, err := client.List(context.Background(), "prefix/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 4 {
		t.Fatalf("List returned %d objects, want 4", len(objects))
	}
	for i, want := range []string{"prefix/a", "prefix/b", "prefix/c", "prefix/d"} {
		if objects[i].Key != want {
			t.Errorf("object %d key = %q, want %q", i, objects[i].Key, want)
		}
		if objects[i].Size != int64(len(want)) {
			t.Errorf("object %d size = %d, want %d", i, objects[i].Size, len(want))
		}
	}
}

func TestS3ObjectURL(t *testing.T) {
	t.Run("path style with base path", func(t *testing.T) {
		client, err := NewS3(S3Config{
			Endpoint: "https://s3.example.com/base",
			Bucket:   "my-bucket",
		})
		if err != nil {
			t.Fatal(err)
		}
		u := client.objectURL("a/b")
		if got, want := u.Host, "s3.example.com"; got != want {
			t.Errorf("host = %q, want %q", got, want)
		}
		if got, want := u.Path, "/base/my-bucket/a/b"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	})

	t.Run("virtual host", func(t *testing.T) {
		client, err := NewS3(S3Config{
			Endpoint:    "https://s3.example.com",
			Bucket:      "my-bucket",
			VirtualHost: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		u := client.objectURL("a/b")
		if got, want := u.Host, "my-bucket.s3.example.com"; got != want {
			t.Errorf("host = %q, want %q", got, want)
		}
		if got, want := u.Path, "/a/b"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	})
}

func TestS3ObjectURLMatchesSignedPath(t *testing.T) {
	client, err := NewS3(S3Config{
		Endpoint: "https://s3.example.com/base",
		Bucket:   "my-bucket",
	})
	if err != nil {
		t.Fatal(err)
	}

	keys := []string{
		"plain/a",
		"a b/c+d",
		"100%/x:y",
		"héllo/世界",
	}
	for _, key := range keys {
		u := client.objectURL(key)
		if got, want := u.EscapedPath(), canonicalURI(u); got != want {
			t.Errorf("key %q: escaped path %q != canonical URI %q", key, got, want)
		}
		// http.NewRequest re-parses the URL string. The path it would send
		// must still be the path the signer covers.
		req, err := http.NewRequest(http.MethodGet, u.String(), nil)
		if err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		if got, want := req.URL.EscapedPath(), canonicalURI(req.URL); got != want {
			t.Errorf("key %q: sent path %q != canonical URI %q", key, got, want)
		}
	}
}

func TestNewS3Validation(t *testing.T) {
	tests := []S3Config{
		{},
		{Endpoint: "ftp://example.com", Bucket: "b"},
		{Endpoint: "http://example.com?x=1", Bucket: "b"},
		{Endpoint: "http://example.com"},
		{Endpoint: "http://example.com", Bucket: "b", AccessKey: "access"},
		{Endpoint: "http://example.com", Bucket: "b", SessionToken: "token"},
	}
	for _, cfg := range tests {
		if _, err := NewS3(cfg); err == nil {
			t.Errorf("NewS3(%+v) succeeded, want error", cfg)
		}
	}
}

func TestS3KeyValidation(t *testing.T) {
	server := newFakeS3(t)
	client := testS3Client(t, server.URL)
	if err := client.Put(context.Background(), "", []byte("x")); err == nil {
		t.Error("expected empty-key error")
	}
	if err := client.Put(context.Background(), "/leading", []byte("x")); err == nil {
		t.Error("expected leading-slash error")
	}
}

func TestS3ListPrefixEncoding(t *testing.T) {
	server := newFakeS3(t)
	client := testS3Client(t, server.URL)
	server.store.put("a b/c", []byte("x"))

	objects, err := client.List(context.Background(), "a b/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 || objects[0].Key != "a b/c" {
		t.Fatalf("objects = %+v", objects)
	}

	// The request URL must be parseable and must not contain a literal space.
	u, err := url.Parse(server.URL + "/bucket?prefix=a%20b%2F&list-type=2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u.RawQuery, " ") {
		t.Fatalf("raw query contains a space: %q", u.RawQuery)
	}
}
