// Package cloud owns the ClickHouse HTTP protocol and resumable synchronization.
package cloud

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DengNaichen/agent-archive/internal/fault"
	"github.com/DengNaichen/agent-archive/internal/history"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed schema.sql
var schemas embed.FS

type Config struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Database string `json:"database"`
	Secure   bool   `json:"secure"`
}
type Client struct {
	config   Config
	endpoint string
	http     *http.Client
}

func Load(path string) (_ *Client, err error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, fmt.Errorf("open clickhouse config: %w", e)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	var c Config
	if e = d.Decode(&c); e != nil {
		return nil, fault.New("invalid clickhouse configuration")
	}
	if p, ok := os.LookupEnv("AGENT_ARCHIVE_CLICKHOUSE_PASSWORD"); ok {
		c.Password = p
	}
	return New(c)
}
func New(c Config) (*Client, error) {
	if !c.Secure || c.Host == "" || strings.ContainsAny(c.Host, "/:@?#\\\r\n\t ") || c.Port < 0 || c.Port > 65535 {
		return nil, fault.New("invalid clickhouse host, port or tls setting")
	}
	if c.Port == 0 {
		c.Port = 8443
	}
	if c.Username == "" {
		c.Username = "default"
	}
	if c.Database == "" {
		c.Database = "default"
	}
	u := url.URL{Scheme: "https", Host: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), Path: "/"}
	return &Client{config: c, endpoint: u.String(), http: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Request(ctx context.Context, sql string, data []byte, compress bool) (_ []byte, err error) {
	q := url.Values{"database": {c.config.Database}, "wait_end_of_query": {"1"}}
	body := []byte(sql)
	if data != nil {
		q.Set("query", sql)
		body = data
	}
	if compress {
		var b bytes.Buffer
		z := gzip.NewWriter(&b)
		if _, e := z.Write(body); e != nil {
			return nil, fmt.Errorf("compress upload: %w", e)
		}
		if e := z.Close(); e != nil {
			return nil, fmt.Errorf("finish compressed upload: %w", e)
		}
		body = b.Bytes()
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"?"+q.Encode(), bytes.NewReader(body))
	if e != nil {
		return nil, fault.New("construct clickhouse request")
	}
	req.SetBasicAuth(c.config.Username, c.config.Password)
	req.Header.Set("Content-Type", "application/octet-stream")
	if compress {
		req.Header.Set("Content-Encoding", "gzip")
	}
	res, e := c.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, transient("clickhouse connection failed (details withheld)")
	}
	defer func() { err = errors.Join(err, res.Body.Close()) }()
	if res.StatusCode != http.StatusOK {
		message := fmt.Sprintf("clickhouse http status %d (response withheld)", res.StatusCode)
		if res.StatusCode == 408 || res.StatusCode == 429 || res.StatusCode >= 500 {
			return nil, transient(message)
		}
		return nil, fault.New(message)
	}
	raw, e := io.ReadAll(io.LimitReader(res.Body, 128<<20+1))
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, transient("read clickhouse response (details withheld)")
	}
	if len(raw) > 128<<20 {
		return nil, fault.New("clickhouse response exceeds 128 MiB")
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("Code:")) {
		return nil, fault.New("clickhouse query failed (response withheld)")
	}
	return raw, nil
}
func (c *Client) Rows(ctx context.Context, sql string, out any) error {
	b, e := c.Request(ctx, sql+" FORMAT JSON", nil, false)
	if e != nil {
		return e
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if e = json.Unmarshal(b, &envelope); e != nil {
		return fault.New("decode clickhouse response")
	}
	if e = json.Unmarshal(envelope.Data, out); e != nil {
		return fault.New("decode clickhouse rows")
	}
	return nil
}
func (c *Client) Ping(ctx context.Context) error {
	b, e := c.Request(ctx, "SELECT 1", nil, false)
	if e != nil {
		return e
	}
	if strings.TrimSpace(string(b)) != "1" {
		return fault.New("unexpected ping response")
	}
	return nil
}
func (c *Client) Init(ctx context.Context) error {
	b, e := schemas.ReadFile("schema.sql")
	if e != nil {
		return fmt.Errorf("read embedded schema: %w", e)
	}
	for _, statement := range strings.Split(string(b), ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, e = c.Request(ctx, statement, nil, false); e != nil {
			return e
		}
	}
	return nil
}
func literal(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'") + "'"
}
func Scope(source, sid string) string {
	return "source=" + literal(source) + " AND session_id=" + literal(sid)
}

var ErrConflict = fault.New("remote snapshot conflicts with local data; no overwrite attempted")

// Destination binds resumable state to the target, without credentials.
func (c *Client) Destination() string {
	return history.SHA([]byte(c.endpoint + "\x00" + c.config.Database + "\x00" + c.config.Username))
}

type temporaryError struct{ error }

func transient(s string) error { return &temporaryError{fault.New(s)} }
func Retryable(err error) bool { var e *temporaryError; return errors.As(err, &e) }
