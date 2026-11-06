package checkpoint

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxObjectBytes bounds one object read during replay. A sealed segment is
	// normally SealBytes plus at most one record, so this is a generous guard
	// against a malformed bucket entry exhausting memory.
	maxObjectBytes = 256 << 20
	// maxListedObjects bounds pagination so a misconfigured prefix cannot keep
	// the replay running forever.
	maxListedObjects = 1 << 20
	// maxS3ErrorBytes bounds the diagnostic body read from a failed request.
	maxS3ErrorBytes = 4 << 10
	// listPageSize is the S3 maximum for ListObjectsV2.
	listPageSize = 1000
)

// S3Config configures a small S3-compatible client. Only static credentials
// are supported: Skopos deploys with an explicit secret, and avoiding an
// identity-provider dependency keeps the checkpoint stdlib-only.
type S3Config struct {
	Endpoint     string
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	SessionToken string
	VirtualHost  bool
	HTTPClient   *http.Client
	Timeout      time.Duration
}

// S3 is an S3-compatible object store client. Path-style addressing is the
// default because MinIO, RustFS, and most test doubles use it.
type S3 struct {
	endpoint    *url.URL
	region      string
	bucket      string
	virtualHost bool
	signer      *signer
	client      *http.Client
}

// S3Object describes one listed object.
type S3Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// S3Error is a non-2xx response from the object store.
type S3Error struct {
	Status  int
	Code    string
	Message string
}

func (e *S3Error) Error() string {
	if e.Code == "" && e.Message == "" {
		return fmt.Sprintf("checkpoint: s3: HTTP %d", e.Status)
	}
	if e.Code == "" {
		return fmt.Sprintf("checkpoint: s3: HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("checkpoint: s3: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

// IsS3NotFound reports whether err is an S3 404 response.
func IsS3NotFound(err error) bool {
	var s3err *S3Error
	return errors.As(err, &s3err) && s3err.Status == http.StatusNotFound
}

// NewS3 validates cfg and builds a client. Credentials are optional so a
// public bucket or a test double can be used without signing.
func NewS3(cfg S3Config) (*S3, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("checkpoint: s3 endpoint is required")
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: s3 endpoint: %w", err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("checkpoint: s3 endpoint %q must use http or https", cfg.Endpoint)
	}
	if endpoint.Host == "" {
		return nil, fmt.Errorf("checkpoint: s3 endpoint %q has no host", cfg.Endpoint)
	}
	if endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("checkpoint: s3 endpoint %q must not contain a query or fragment", cfg.Endpoint)
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("checkpoint: s3 bucket is required")
	}
	if (cfg.AccessKey == "") != (cfg.SecretKey == "") {
		return nil, errors.New("checkpoint: s3 access key and secret key must be set together")
	}
	if cfg.SessionToken != "" && cfg.AccessKey == "" {
		return nil, errors.New("checkpoint: s3 session token requires an access key")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")

	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: cfg.Timeout,
			// S3 answers a wrong region with a redirect. Following it would
			// turn the PUT into a GET and hide the real configuration error.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	s := &S3{
		endpoint:    endpoint,
		region:      cfg.Region,
		bucket:      cfg.Bucket,
		virtualHost: cfg.VirtualHost,
		client:      client,
	}
	if cfg.AccessKey != "" || cfg.SecretKey != "" {
		s.signer = newSigner(credentials{
			accessKey: cfg.AccessKey,
			secretKey: cfg.SecretKey,
			token:     cfg.SessionToken,
		}, cfg.Region, "s3", nil)
	}
	return s, nil
}

// Put stores data at key. The operation is idempotent: uploading the same key
// and bytes again overwrites the object.
func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	if err := validKey(key); err != nil {
		return err
	}
	u := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("checkpoint: s3 put: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	s.sign(req, sha256Hex(data))
	return s.do(req, http.StatusOK, http.StatusCreated, http.StatusNoContent)
}

// Get reads the object at key.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	u := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: s3 get: %w", err)
	}
	s.sign(req, emptyPayloadSHA)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: s3 get %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, s.responseError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("checkpoint: s3 get %q: %w", key, err)
	}
	if len(data) > maxObjectBytes {
		return nil, fmt.Errorf("checkpoint: s3 object %q exceeds %d bytes", key, maxObjectBytes)
	}
	return data, nil
}

// Delete removes key. A missing object is not an error, which makes retries
// after a partially completed upload safe.
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	u := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return fmt.Errorf("checkpoint: s3 delete: %w", err)
	}
	s.sign(req, emptyPayloadSHA)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("checkpoint: s3 delete %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return s.responseError(resp)
	}
	return nil
}

// List returns every object whose key starts with prefix, following
// ListObjectsV2 continuation tokens.
func (s *S3) List(ctx context.Context, prefix string) ([]S3Object, error) {
	var out []S3Object
	token := ""
	for {
		u := s.objectURL("")
		query := url.Values{}
		query.Set("list-type", "2")
		query.Set("max-keys", fmt.Sprintf("%d", listPageSize))
		if prefix != "" {
			query.Set("prefix", prefix)
		}
		if token != "" {
			query.Set("continuation-token", token)
		}
		u.RawQuery = encodeQueryValues(query)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("checkpoint: s3 list: %w", err)
		}
		s.sign(req, emptyPayloadSHA)

		resp, err := s.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("checkpoint: s3 list: %w", err)
		}
		page, err := decodeListResponse(resp)
		if err != nil {
			return nil, err
		}
		out = append(out, page.objects...)
		if len(out) > maxListedObjects {
			return nil, fmt.Errorf("checkpoint: s3 list %q exceeds %d objects", prefix, maxListedObjects)
		}
		if !page.truncated {
			return out, nil
		}
		if page.nextToken == "" {
			return nil, fmt.Errorf("checkpoint: s3 list %q: truncated page without continuation token", prefix)
		}
		token = page.nextToken
	}
}

func (s *S3) sign(req *http.Request, payloadSHA string) {
	if s.signer == nil {
		return
	}
	s.signer.sign(req, payloadSHA)
}

func (s *S3) do(req *http.Request, okStatus ...int) error {
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("checkpoint: s3 %s %q: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	for _, status := range okStatus {
		if resp.StatusCode == status {
			io.Copy(io.Discard, io.LimitReader(resp.Body, maxS3ErrorBytes))
			return nil
		}
	}
	return s.responseError(resp)
}

func (s *S3) responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxS3ErrorBytes))
	var parsed struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &parsed) == nil && (parsed.Code != "" || parsed.Message != "") {
		return &S3Error{Status: resp.StatusCode, Code: parsed.Code, Message: parsed.Message}
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		return &S3Error{Status: resp.StatusCode}
	}
	return &S3Error{Status: resp.StatusCode, Message: message}
}

// objectURL builds the URL for one object. Path-style is the default; virtual
// hosting moves the bucket into the host name.
func (s *S3) objectURL(key string) *url.URL {
	u := *s.endpoint
	if s.virtualHost {
		u.Host = s.bucket + "." + u.Host
		u.Path = joinURLPath(u.Path, key)
	} else {
		u.Path = joinURLPath(u.Path, s.bucket, key)
	}
	// Go leaves some reserved characters (for example '+', '$', ':') unescaped
	// in URL paths. SigV4 canonicalizes the decoded path with full RFC 3986
	// encoding, so pin RawPath to the signed form. The request then sends the
	// exact bytes that were signed, including keys that are not produced by the
	// uploader but are still legal S3 keys.
	u.RawPath = uriEncode(u.Path, false)
	return &u
}

func validKey(key string) error {
	if key == "" {
		return errors.New("checkpoint: s3 key is required")
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("checkpoint: s3 key %q must not start with a slash", key)
	}
	return nil
}

// joinURLPath appends parts without path.Clean: S3 keys are opaque and must
// not be normalized.
func joinURLPath(base string, parts ...string) string {
	out := strings.TrimRight(base, "/")
	for _, part := range parts {
		part = strings.TrimLeft(part, "/")
		if part == "" {
			continue
		}
		if out == "" {
			out = "/" + part
		} else {
			out += "/" + part
		}
	}
	if out == "" {
		return "/"
	}
	return out
}

type listPage struct {
	objects   []S3Object
	truncated bool
	nextToken string
}

func decodeListResponse(resp *http.Response) (listPage, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxS3ErrorBytes))
		var parsed struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if xml.Unmarshal(body, &parsed) == nil && (parsed.Code != "" || parsed.Message != "") {
			return listPage{}, &S3Error{Status: resp.StatusCode, Code: parsed.Code, Message: parsed.Message}
		}
		return listPage{}, &S3Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(body))}
	}

	var parsed struct {
		IsTruncated           bool   `xml:"IsTruncated"`
		NextContinuationToken string `xml:"NextContinuationToken"`
		Contents              []struct {
			Key          string `xml:"Key"`
			Size         int64  `xml:"Size"`
			LastModified string `xml:"LastModified"`
		} `xml:"Contents"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return listPage{}, fmt.Errorf("checkpoint: s3 list response: %w", err)
	}
	page := listPage{
		truncated: parsed.IsTruncated,
		nextToken: parsed.NextContinuationToken,
	}
	page.objects = make([]S3Object, 0, len(parsed.Contents))
	for _, item := range parsed.Contents {
		obj := S3Object{Key: item.Key, Size: item.Size}
		if item.LastModified != "" {
			if ts, err := time.Parse(time.RFC3339, item.LastModified); err == nil {
				obj.LastModified = ts
			}
		}
		page.objects = append(page.objects, obj)
	}
	return page, nil
}
