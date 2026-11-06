// Package chclient is a small ClickHouse HTTP client built on net/http. It
// intentionally does not use a database driver: the gateway only needs
// parameterized read queries and JSON responses.
package chclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jawaracloud/skopos/internal/plan"
)

// Config configures a Client. All limits are sent to ClickHouse on every
// query and are also intended to be set on the read-only ClickHouse user.
type Config struct {
	BaseURL          string
	Database         string
	User             string
	Password         string
	Timeout          time.Duration
	MaxExecutionTime time.Duration
	MaxMemoryUsage   uint64
	MaxRowsToRead    uint64
	MaxResultRows    uint64
	MaxResponseBytes int64
	HTTPClient       *http.Client
}

// Client executes read-only SQL against ClickHouse over HTTP.
type Client struct {
	base             *url.URL
	database         string
	user             string
	password         string
	maxExecutionTime time.Duration
	maxMemoryUsage   uint64
	maxRowsToRead    uint64
	maxResultRows    uint64
	maxResponseBytes int64
	http             *http.Client
}

// Column is a ClickHouse result column.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Result is a parsed ClickHouse JSON response.
type Result struct {
	Columns []Column
	Rows    []map[string]any
}

// Len returns the number of rows.
func (r *Result) Len() int {
	if r == nil {
		return 0
	}
	return len(r.Rows)
}

// String returns a string cell.
func (r *Result) String(row int, column string) (string, bool) {
	if r == nil || row < 0 || row >= len(r.Rows) {
		return "", false
	}
	v, ok := r.Rows[row][column]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// Float64 returns a numeric cell as float64.
func (r *Result) Float64(row int, column string) (float64, bool) {
	if r == nil || row < 0 || row >= len(r.Rows) {
		return 0, false
	}
	v, ok := r.Rows[row][column]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	default:
		return 0, false
	}
}

// Int64 returns a numeric cell as int64.
func (r *Result) Int64(row int, column string) (int64, bool) {
	if r == nil || row < 0 || row >= len(r.Rows) {
		return 0, false
	}
	v, ok := r.Rows[row][column]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case json.Number:
		i, err := x.Int64()
		return i, err == nil
	case float64:
		if x != float64(int64(x)) {
			return 0, false
		}
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	default:
		return 0, false
	}
}

// Error is a ClickHouse error response.
type Error struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("clickhouse: status %d code %s: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("clickhouse: status %d: %s", e.StatusCode, e.Message)
}

// New validates cfg and returns a client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("chclient: base URL is required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("chclient: parse base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("chclient: unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("chclient: base URL has no host")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("chclient: timeout must not be negative")
	}
	if cfg.MaxResponseBytes < 0 {
		return nil, errors.New("chclient: max response bytes must not be negative")
	}
	if cfg.Database == "" {
		cfg.Database = "default"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxExecutionTime == 0 {
		cfg.MaxExecutionTime = 30 * time.Second
	}
	if cfg.MaxMemoryUsage == 0 {
		cfg.MaxMemoryUsage = 1 << 30
	}
	if cfg.MaxRowsToRead == 0 {
		cfg.MaxRowsToRead = 10_000_000
	}
	if cfg.MaxResultRows == 0 {
		cfg.MaxResultRows = 100_000
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 64 << 20
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{
		base:             u,
		database:         cfg.Database,
		user:             cfg.User,
		password:         cfg.Password,
		maxExecutionTime: cfg.MaxExecutionTime,
		maxMemoryUsage:   cfg.MaxMemoryUsage,
		maxRowsToRead:    cfg.MaxRowsToRead,
		maxResultRows:    cfg.MaxResultRows,
		maxResponseBytes: cfg.MaxResponseBytes,
		http:             cfg.HTTPClient,
	}, nil
}

// Close releases idle HTTP connections.
func (c *Client) Close() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

// Ping checks that ClickHouse answers a trivial read.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Query(ctx, "SELECT 1", nil)
	return err
}

// Query executes one parameterized read statement. It appends FORMAT JSON and
// parses the response. A statement whose first keyword is not SELECT or WITH is
// rejected before any network call.
func (c *Client) Query(ctx context.Context, sql string, params []plan.Param) (*Result, error) {
	if !isReadOnlySQL(sql) {
		return nil, errors.New("chclient: only SELECT and WITH statements are allowed")
	}

	u := *c.base
	q := u.Query()
	q.Set("database", c.database)
	if c.maxExecutionTime > 0 {
		seconds := int64(c.maxExecutionTime / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		q.Set("max_execution_time", strconv.FormatInt(seconds, 10))
	}
	if c.maxMemoryUsage > 0 {
		q.Set("max_memory_usage", strconv.FormatUint(c.maxMemoryUsage, 10))
	}
	if c.maxRowsToRead > 0 {
		q.Set("max_rows_to_read", strconv.FormatUint(c.maxRowsToRead, 10))
	}
	if c.maxResultRows > 0 {
		q.Set("max_result_rows", strconv.FormatUint(c.maxResultRows, 10))
		q.Set("result_overflow_mode", "throw")
	}

	seen := make(map[string]struct{}, len(params))
	for _, p := range params {
		// The emitter reserves the "__" prefix for generated parameters such
		// as __start, __end, and __limit. Validate types and values here; the
		// plan builders are responsible for rejecting user-supplied reserved
		// names.
		if err := plan.ValidateParamValue(p); err != nil {
			return nil, fmt.Errorf("chclient: parameter: %w", err)
		}
		if _, exists := seen[p.Name]; exists {
			return nil, fmt.Errorf("chclient: duplicate parameter %q", p.Name)
		}
		seen[p.Name] = struct{}{}
		encoded, err := EncodeParam(p)
		if err != nil {
			return nil, fmt.Errorf("chclient: parameter %q: %w", p.Name, err)
		}
		q.Set("param_"+p.Name, encoded)
	}
	u.RawQuery = q.Encode()

	body := stripSQL(sql) + "\nFORMAT JSON\n"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("chclient: build request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "skopos-gateway")
	if c.user != "" || c.password != "" {
		req.SetBasicAuth(c.user, c.password)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chclient: request: %w", err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, c.maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("chclient: read response: %w", err)
	}
	if int64(len(data)) > c.maxResponseBytes {
		return nil, fmt.Errorf("chclient: response exceeds %d bytes", c.maxResponseBytes)
	}

	if resp.StatusCode != http.StatusOK {
		queryErr := parseError(resp.StatusCode, data)
		// FINAL is rejected by plain MergeTree. The chart migrates the signal
		// tables to ReplacingMergeTree, but a gateway can start against an
		// older schema while the init Job is still running. Retry without
		// FINAL so reads stay available; only read-time deduplication is lost.
		if isIllegalFinal(queryErr) {
			if fallback, ok := withoutFinal(sql); ok {
				return c.Query(ctx, fallback, params)
			}
		}
		return nil, queryErr
	}

	var raw struct {
		Meta      []Column                     `json:"meta"`
		Data      []map[string]json.RawMessage `json:"data"`
		Exception string                       `json:"exception"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("chclient: decode response: %w", err)
	}
	if raw.Exception != "" {
		return nil, parseError(resp.StatusCode, data)
	}

	result := &Result{Columns: raw.Meta, Rows: make([]map[string]any, 0, len(raw.Data))}
	for _, rawRow := range raw.Data {
		row := make(map[string]any, len(rawRow))
		for k, v := range rawRow {
			decoded, err := decodeValue(v)
			if err != nil {
				return nil, fmt.Errorf("chclient: decode column %q: %w", k, err)
			}
			row[k] = decoded
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func decodeValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// EncodeParam returns the ClickHouse text-format value for a parameter.
func EncodeParam(p plan.Param) (string, error) {
	if err := plan.ValidateParamValue(p); err != nil {
		return "", err
	}
	switch p.Type {
	case plan.TypeString:
		return p.Value.(string), nil
	case plan.TypeStrings:
		return encodeStringArray(p.Value.([]string)), nil
	case plan.TypeDateTime64:
		return p.Value.(time.Time).UTC().Format("2006-01-02 15:04:05.000000000"), nil
	case plan.TypeUInt64:
		switch v := p.Value.(type) {
		case uint64:
			return strconv.FormatUint(v, 10), nil
		case uint:
			return strconv.FormatUint(uint64(v), 10), nil
		case int:
			return strconv.Itoa(v), nil
		case int64:
			return strconv.FormatInt(v, 10), nil
		}
	case plan.TypeInt64:
		switch v := p.Value.(type) {
		case int64:
			return strconv.FormatInt(v, 10), nil
		case int:
			return strconv.Itoa(v), nil
		}
	case plan.TypeFloat64:
		switch v := p.Value.(type) {
		case float64:
			return strconv.FormatFloat(v, 'g', -1, 64), nil
		case float32:
			return strconv.FormatFloat(float64(v), 'g', -1, 64), nil
		case int:
			return strconv.Itoa(v), nil
		case int64:
			return strconv.FormatInt(v, 10), nil
		}
	case plan.TypeBool:
		if p.Value.(bool) {
			return "1", nil
		}
		return "0", nil
	}
	return "", fmt.Errorf("cannot encode parameter type %q", p.Type)
}

func encodeStringArray(values []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('\'')
		b.WriteString(escapeClickHouseString(v))
		b.WriteByte('\'')
	}
	b.WriteByte(']')
	return b.String()
}

func escapeClickHouseString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 0:
			b.WriteString(`\0`)
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteString(`\x`)
				var tmp [2]byte
				hex.Encode(tmp[:], []byte{byte(r)})
				b.WriteString(string(tmp[:]))
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func stripSQL(sql string) string {
	s := strings.TrimSpace(sql)
	for strings.HasSuffix(s, ";") {
		s = strings.TrimSpace(strings.TrimSuffix(s, ";"))
	}
	return s
}

func isReadOnlySQL(sql string) bool {
	s := strings.TrimSpace(sql)
	for {
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return false
			}
			s = strings.TrimSpace(s[i+1:])
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s, "*/")
			if i < 0 {
				return false
			}
			s = strings.TrimSpace(s[i+2:])
		default:
			i := 0
			for i < len(s) && ((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')) {
				i++
			}
			if i == 0 {
				return false
			}
			keyword := strings.ToUpper(s[:i])
			if keyword != "SELECT" && keyword != "WITH" {
				return false
			}
			return !containsWriteKeyword(s)
		}
	}
}

var writeKeywords = map[string]struct{}{
	"INSERT": {}, "ALTER": {}, "DROP": {}, "CREATE": {}, "TRUNCATE": {},
	"RENAME": {}, "ATTACH": {}, "DETACH": {}, "OPTIMIZE": {}, "GRANT": {},
	"REVOKE": {}, "KILL": {}, "EXCHANGE": {}, "MOVE": {}, "UNDROP": {},
	"DELETE": {}, "UPDATE": {},
}

// containsWriteKeyword scans tokens outside strings, quoted identifiers, and
// comments. It is a defence-in-depth check on top of the read-only ClickHouse
// user; it is not a full SQL parser.
func containsWriteKeyword(sql string) bool {
	for i := 0; i < len(sql); {
		switch sql[i] {
		case '\'':
			i = skipQuoted(sql, i, '\'')
		case '"':
			i = skipQuoted(sql, i, '"')
		case '`':
			i = skipQuoted(sql, i, '`')
		case '{':
			// ClickHouse query parameter placeholder: {name:Type}. The type
			// can contain parentheses but not a closing brace.
			if n := strings.IndexByte(sql[i:], '}'); n >= 0 {
				i += n + 1
			} else {
				i++
			}
		case '-':
			if i+1 < len(sql) && sql[i+1] == '-' {
				if n := strings.IndexByte(sql[i+2:], '\n'); n >= 0 {
					i += 2 + n + 1
				} else {
					return false
				}
			} else {
				i++
			}
		case '/':
			if i+1 < len(sql) && sql[i+1] == '*' {
				if n := strings.Index(sql[i+2:], "*/"); n >= 0 {
					i += 2 + n + 2
				} else {
					return false
				}
			} else {
				i++
			}
		default:
			if isWordStart(sql[i]) {
				start := i
				for i < len(sql) && isWordPart(sql[i]) {
					i++
				}
				if _, ok := writeKeywords[strings.ToUpper(sql[start:i])]; ok {
					return true
				}
				continue
			}
			i++
		}
	}
	return false
}

func skipQuoted(s string, start int, quote byte) int {
	for i := start + 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == quote {
			if quote != '`' && i+1 < len(s) && s[i+1] == quote {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(s)
}

func isWordStart(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '_'
}

func isWordPart(b byte) bool {
	return isWordStart(b) || (b >= '0' && b <= '9')
}

var codePattern = regexp.MustCompile(`(?i)code:\s*([0-9]+)`)

// illegalFinalCode is ClickHouse's ILLEGAL_FINAL code. Plain MergeTree tables
// reject FINAL; ReplacingMergeTree tables accept it.
const illegalFinalCode = "181"

func isIllegalFinal(err error) bool {
	var chErr *Error
	return errors.As(err, &chErr) && chErr.Code == illegalFinalCode
}

// withoutFinal removes the first bare FINAL marker the emitter placed after a
// table name. User values are query parameters and identifiers are backticked,
// so this cannot touch a column or table named FINAL.
func withoutFinal(sql string) (string, bool) {
	const marker = " FINAL"
	i := strings.Index(sql, marker)
	if i < 0 {
		return "", false
	}
	return sql[:i] + sql[i+len(marker):], true
}

func parseError(status int, data []byte) error {
	message := strings.TrimSpace(string(data))
	var raw struct {
		Exception string `json:"exception"`
	}
	if err := json.Unmarshal(data, &raw); err == nil && raw.Exception != "" {
		message = raw.Exception
	}
	if len(message) > 4096 {
		message = message[:4096]
	}
	code := ""
	if m := codePattern.FindStringSubmatch(message); len(m) == 2 {
		code = m[1]
	}
	return &Error{StatusCode: status, Code: code, Message: message}
}
