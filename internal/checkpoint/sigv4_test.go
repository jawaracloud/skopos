package checkpoint

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Vectors from the AWS Signature Version 4 documentation. The signer's clock
// is pinned to the vector date.
func TestSignerGetObjectVector(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")

	s := newSigner(credentials{
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}, "us-east-1", "s3", func() time.Time {
		return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	})
	s.sign(req, emptyPayloadSHA)

	const wantSignature = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+wantSignature) {
		t.Fatalf("signature mismatch\n got: %s\nwant: Signature=%s", got, wantSignature)
	}
	if got, want := req.Header.Get("Authorization"), "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date"; !strings.Contains(got, want) {
		t.Fatalf("signed headers mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestSignerPutObjectVector(t *testing.T) {
	body := []byte("Welcome to Amazon S3.")
	req, err := http.NewRequest(http.MethodPut, "https://examplebucket.s3.amazonaws.com/test$file.text", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("x-amz-storage-class", "REDUCED_REDUNDANCY")

	s := newSigner(credentials{
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}, "us-east-1", "s3", func() time.Time {
		return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	})
	s.sign(req, sha256Hex(body))

	const wantSignature = "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+wantSignature) {
		t.Fatalf("signature mismatch\n got: %s\nwant: Signature=%s", got, wantSignature)
	}
}

func TestCanonicalQuery(t *testing.T) {
	u, err := url.Parse("https://example.com/?prefix=a%20b&list-type=2&continuation-token=x%2Fy")
	if err != nil {
		t.Fatal(err)
	}
	const want = "continuation-token=x%2Fy&list-type=2&prefix=a%20b"
	if got := canonicalQuery(u); got != want {
		t.Fatalf("canonicalQuery = %q, want %q", got, want)
	}
	values := u.Query()
	if got := encodeQueryValues(values); got != want {
		t.Fatalf("encodeQueryValues = %q, want %q", got, want)
	}
}

func TestURIEncode(t *testing.T) {
	tests := map[string]string{
		"simple":       "simple",
		"a b":          "a%20b",
		"a/b":          "a%2Fb",
		"a~b_c-d.e":    "a~b_c-d.e",
		"héllo":        "h%C3%A9llo",
		"100%":         "100%25",
		"a+b":          "a%2Bb",
		"segments/x/y": "segments%2Fx%2Fy",
	}
	for raw, want := range tests {
		if got := uriEncode(raw, true); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", raw, got, want)
		}
	}
	if got := uriEncode("/path/to/key", false); got != "/path/to/key" {
		t.Errorf("uriEncode path with slashes = %q", got)
	}
}
