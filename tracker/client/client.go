// Package client is the Go client of the tracker API. It is the only tracker
// package gt links, so it depends on the standard library and package api
// alone (no database driver).
//
// Every attempt has a bounded timeout. Only reads are retried, only when the
// failure is api.KindUnavailable, and only up to MaxRetries: a write that
// failed may have landed, so the caller decides whether to repeat it.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
)

const (
	defaultTimeout    = 10 * time.Second
	defaultMaxRetries = 3
	backoffBase       = 100 * time.Millisecond
	backoffCap        = 2 * time.Second
	maxResponseBytes  = 32 << 20
)

// Config configures a Client. Exactly one of Token and TokenFile is set.
type Config struct {
	// BaseURL is the service address. Plain http is accepted only to a
	// literal loopback IP; anything else must be https.
	BaseURL string
	// Token is the bearer token itself, for callers that took it from their
	// environment. TokenFile names a file holding it instead.
	Token     string
	TokenFile string
	// SchemaLevel defaults to api.SchemaLevel.
	SchemaLevel int
	// Timeout bounds each attempt; default 10 s.
	Timeout time.Duration
	// MaxRetries is how many times a read is retried after an unavailable
	// answer; 0 means the default 3, a negative value means none.
	MaxRetries int
	// Sleep waits between retries; tests inject one that records and returns.
	Sleep func(ctx context.Context, d time.Duration) error
	// HTTP overrides the transport.
	HTTP *http.Client
}

// FromEnv builds a Config from TRACKER_URL and either TRACKER_TOKEN_FILE or
// TRACKER_TOKEN. It validates nothing; New does.
func FromEnv(getenv func(string) string) Config {
	cfg := Config{BaseURL: getenv("TRACKER_URL")}
	if f := getenv("TRACKER_TOKEN_FILE"); f != "" {
		cfg.TokenFile = f
	} else {
		cfg.Token = getenv("TRACKER_TOKEN")
	}
	return cfg
}

// Client talks to trackerd. Safe for concurrent use.
type Client struct {
	base       string
	token      string
	schema     string
	timeout    time.Duration
	maxRetries int
	sleep      func(ctx context.Context, d time.Duration) error
	http       *http.Client
}

// New validates cfg and returns a Client. A token read from a file has its
// surrounding whitespace trimmed; an empty one is an error. Errors name the
// file, never its contents.
func New(cfg Config) (*Client, error) {
	if err := checkBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	token := cfg.Token
	switch {
	case cfg.Token != "" && cfg.TokenFile != "":
		return nil, errors.New("client: set a token or a token file, not both")
	case cfg.TokenFile != "":
		b, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("client: read token file %s: %w", cfg.TokenFile, err)
		}
		token = strings.TrimSpace(string(b))
		if token == "" {
			return nil, fmt.Errorf("client: token file %s is empty", cfg.TokenFile)
		}
	case token == "":
		return nil, errors.New("client: no token: set Token or TokenFile")
	}
	c := &Client{
		base: strings.TrimRight(cfg.BaseURL, "/"), token: token,
		schema:  fmt.Sprint(orInt(cfg.SchemaLevel, api.SchemaLevel)),
		timeout: orDuration(cfg.Timeout, defaultTimeout), sleep: cfg.Sleep, http: cfg.HTTP,
	}
	switch {
	case cfg.MaxRetries < 0:
		c.maxRetries = 0
	case cfg.MaxRetries == 0:
		c.maxRetries = defaultMaxRetries
	default:
		c.maxRetries = cfg.MaxRetries
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	return c, nil
}

func orInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

func orDuration(v, d time.Duration) time.Duration {
	if v == 0 {
		return d
	}
	return v
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// checkBaseURL accepts https anywhere and plain http only to a literal
// loopback IP, so a token never crosses a network in the clear.
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("client: base URL %q is not a URL with a host", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("client: base URL %q is plain http to a non-loopback host; use https", raw)
	}
	return fmt.Errorf("client: base URL %q must be http or https", raw)
}

type tagKey struct{}

// WithTag names the caller's unit of work (a dispatch tick id); the server
// counts calls per tag.
func WithTag(ctx context.Context, tag string) context.Context {
	return context.WithValue(ctx, tagKey{}, tag)
}

// Whoami returns the actor the client's token was issued to.
func (c *Client) Whoami(ctx context.Context) (string, error) {
	var out struct {
		Actor string `json:"actor"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/whoami", nil, &out, true); err != nil {
		return "", err
	}
	return out.Actor, nil
}

// do runs one logical call. idempotent calls are retried on an unavailable
// answer; others get exactly one attempt.
//
//nolint:unparam // idempotent is false for the write routes, which arrive with their own bead
func (c *Client) do(ctx context.Context, method, path string, in, out any, idempotent bool) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("client: encode request: %w", err)
		}
	}
	attempts := 1
	if idempotent {
		attempts += c.maxRetries
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if err := c.sleep(ctx, backoff(i)); err != nil {
				return last
			}
		}
		last = c.once(ctx, method, path, body, out)
		if last == nil || !errors.Is(last, api.ErrUnavailable) {
			return last
		}
	}
	return last
}

// backoff is the wait before retry n (n >= 1): 100 ms doubling, capped.
func backoff(n int) time.Duration {
	d := backoffBase << (n - 1)
	if d <= 0 || d > backoffCap {
		return backoffCap
	}
	return d
}

func (c *Client) once(parent context.Context, method, path string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return api.Invalid("client: build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(api.SchemaHeader, c.schema)
	if tag, _ := ctx.Value(tagKey{}).(string); tag != "" {
		req.Header.Set(api.RequestTagHeader, tag)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The caller gave up (canceled, or its own deadline passed): that is
		// theirs, not a failure of the service, and never worth a retry.
		if perr := parent.Err(); perr != nil {
			return perr
		}
		// A transport failure (refused, reset, this attempt timed out) is
		// "unavailable": a read may be repeated; a write may or may not have
		// been applied, so it is never retried here.
		return api.Unavailable("tracker request failed: %v", urlErrorOnly(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return api.Unavailable("tracker response unreadable: %v", urlErrorOnly(err))
	}
	if resp.StatusCode/100 == 2 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			// The server answered success, so the call may have been applied:
			// this is a protocol error, never a retryable "unavailable".
			return fmt.Errorf("client: tracker answered %d with a body that is not the expected JSON", resp.StatusCode)
		}
		return nil
	}
	return c.decodeError(resp.StatusCode, data)
}

// urlErrorOnly drops the request URL wrapper's detail down to its cause.
func urlErrorOnly(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// maxServerMessage bounds how much of a server's error text is kept.
const maxServerMessage = 500

// decodeError turns a non-2xx answer into an *api.Error. A body that is the
// API's error JSON keeps its kind and a cleaned message; any other body is
// never echoed, since a misbehaving server could include what it was sent.
func (c *Client) decodeError(status int, data []byte) error {
	var e api.Error
	if json.Unmarshal(data, &e) == nil && e.Message != "" && knownKind(e.Kind) {
		e.Message = c.cleanMessage(e.Message)
		return &e
	}
	if status >= 500 {
		return api.Unavailable("tracker answered %d", status)
	}
	return api.Invalid("tracker answered %d", status)
}

// cleanMessage makes server text safe to put in an error or a log: control
// characters become spaces, the token (should a server echo it) is redacted,
// and the length is bounded.
func (c *Client) cleanMessage(m string) string {
	m = strings.ReplaceAll(m, c.token, "[redacted]")
	m = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, m)
	if len(m) > maxServerMessage {
		m = m[:maxServerMessage] + "..."
	}
	return m
}

func knownKind(k api.Kind) bool {
	switch k {
	case api.KindNotFound, api.KindGuardFailed, api.KindConflict, api.KindInvalid, api.KindForbidden, api.KindUnavailable:
		return true
	}
	return false
}
