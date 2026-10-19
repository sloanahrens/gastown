// Package jev is the client for TypeSafe's Jev model: it asks one Choice
// question of POST /v1/systemone and returns the chosen option, the option
// probabilities, the confidence and the token usage of the call.
//
// The API key comes from a 0600 key file (see KeyPaths) and appears in
// neither errors nor logs; the client logs nothing, and request bodies are
// never echoed.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// Model is the pinned model; "jev-latest" would move under the caller.
	Model = "jev-1.13.0"

	defaultBaseURL = "https://api.typesafe.ai/v1/systemone"

	// DefaultTimeout bounds one call, connect to last body byte.
	DefaultTimeout = 30 * time.Second

	// questionID names the one question in each request; the model never
	// sees it.
	questionID = "q"

	// maxBody caps how much of a response is read.
	maxBody = 1 << 20

	// maxMessage caps the server error text carried in a StatusError.
	maxMessage = 200

	// probabilityTolerance absorbs decimal rounding in the sum of a response's
	// probabilities, which JSON may deliver as 0.9999999999999999.
	probabilityTolerance = 1e-6
)

// Client asks Choice questions of one Jev endpoint.
type Client struct {
	httpClient *http.Client
	baseURL    string
	key        string
	keyPaths   []string
	timeout    time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client the requests go through; nil keeps the
// default client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithBaseURL sets the full endpoint URL, for tests.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithTimeout sets the per-call timeout; a value of zero or less keeps
// DefaultTimeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithKey sets the API key directly, skipping the key files.
func WithKey(key string) Option { return func(c *Client) { c.key = key } }

// WithKeyPaths sets the files the key is searched in, in place of KeyPaths.
func WithKeyPaths(paths ...string) Option { return func(c *Client) { c.keyPaths = paths } }

// NewClient builds a client, reading the key from the key files unless
// WithKey supplies one.
func NewClient(opts ...Option) (*Client, error) {
	c := &Client{httpClient: http.DefaultClient, baseURL: defaultBaseURL, timeout: DefaultTimeout}
	for _, o := range opts {
		o(c)
	}
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}
	if c.key != "" {
		return c, nil
	}
	var (
		key string
		err error
	)
	if c.keyPaths != nil {
		key, err = readKeyFrom(c.keyPaths)
	} else {
		key, err = ReadKey()
	}
	if err != nil {
		return nil, err
	}
	c.key = key
	return c, nil
}

// Usage is the token count the API reports for one call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Answer is the result of one Choice question.
type Answer struct {
	// Choice is the option with the highest probability.
	Choice string
	// Probabilities maps every option to its probability; the values sum to 1.
	Probabilities map[string]float64
	// Confidence is 0 to 1, derived from how spread Probabilities is.
	Confidence float64
	// Model is the versioned model ID that answered.
	Model string
	// Usage is the token usage of this call.
	Usage Usage
}

type question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type request struct {
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]question `json:"questions"`
}

type choiceAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

type response struct {
	Model   string                  `json:"model"`
	Answers map[string]choiceAnswer `json:"answers"`
	Usage   Usage                   `json:"usage"`
}

// Choice asks which of criteria's options fits state under instructions. The
// criteria map is option name to description. It makes one HTTP call and
// never retries: a deadline returns ErrTimeout, a 429 returns a
// *RateLimitError, a 401 returns ErrUnauthorized, any other non-2xx returns a
// *StatusError, and a 2xx without a usable answer returns ErrBadResponse.
func (c *Client) Choice(ctx context.Context, state any, instructions string, criteria map[string]string) (*Answer, error) {
	if len(criteria) == 0 {
		return nil, errors.New("jev: a Choice needs at least one option")
	}
	body, err := json.Marshal(request{
		State: state,
		Model: Model,
		Questions: map[string]question{
			questionID: {Type: "choice", Instructions: instructions, Criteria: criteria},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("jev: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, c.redact(fmt.Errorf("jev: build request: %w", err))
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, c.transportErr(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, c.transportErr(ctx, err)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &RateLimitError{RetryAfter: retryAfter(resp.Header)}
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &StatusError{Status: resp.StatusCode, Message: c.message(data)}
	}
	return parseAnswer(data, criteria)
}

// parseAnswer decodes a 2xx body and returns ErrBadResponse when it does not
// answer the question asked with the values Answer documents.
func parseAnswer(data []byte, criteria map[string]string) (*Answer, error) {
	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrBadResponse, err)
	}
	a, ok := r.Answers[questionID]
	if !ok || a.Type != "choice" {
		return nil, fmt.Errorf("%w: no choice answer", ErrBadResponse)
	}
	if _, ok := criteria[a.Choice]; !ok {
		return nil, fmt.Errorf("%w: choice %q is not an option asked", ErrBadResponse, a.Choice)
	}
	if a.Confidence < 0 || a.Confidence > 1 {
		return nil, fmt.Errorf("%w: confidence %v is outside 0 to 1", ErrBadResponse, a.Confidence)
	}
	if err := checkProbabilities(a.Probabilities, criteria); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	if a.Probabilities[a.Choice] < highest(a.Probabilities) {
		return nil, fmt.Errorf("%w: choice %q is not the most probable option", ErrBadResponse, a.Choice)
	}
	return &Answer{
		Choice:        a.Choice,
		Probabilities: a.Probabilities,
		Confidence:    a.Confidence,
		Model:         r.Model,
		Usage:         r.Usage,
	}, nil
}

// checkProbabilities checks that probabilities name every option asked and no
// other, each within 0 to 1, and sum to 1.
func checkProbabilities(probabilities map[string]float64, criteria map[string]string) error {
	if len(probabilities) != len(criteria) {
		return fmt.Errorf("probabilities cover %d options, %d were asked", len(probabilities), len(criteria))
	}
	var sum float64
	for name, p := range probabilities {
		if _, ok := criteria[name]; !ok {
			return fmt.Errorf("probability for %q, which was not asked", name)
		}
		if p < 0 || p > 1 {
			return fmt.Errorf("probability for %q is %v, outside 0 to 1", name, p)
		}
		sum += p
	}
	if math.Abs(sum-1) > probabilityTolerance {
		return fmt.Errorf("probabilities sum to %v, not 1", sum)
	}
	return nil
}

// highest returns the largest probability in probabilities.
func highest(probabilities map[string]float64) float64 {
	best := math.Inf(-1)
	for _, p := range probabilities {
		if p > best {
			best = p
		}
	}
	return best
}

// transportErr maps a failed round trip to ErrTimeout when the call ran out
// of time, and otherwise returns it with the key scrubbed. A canceled
// caller context passes through as context.Canceled.
func (c *Client) transportErr(ctx context.Context, err error) error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
		return ErrTimeout
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return c.redact(fmt.Errorf("jev: request failed: %w", err))
}

// redact returns err with the key removed from its text, for errors built
// from a library message that could quote a header.
func (c *Client) redact(err error) error {
	if c.key == "" || !strings.Contains(err.Error(), c.key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.key, "[redacted]"))
}

// message extracts a short error text from an error body: the "message",
// "error" or "detail" field of a JSON object when present, else the raw text,
// with the key redacted and the text cut to at most maxMessage bytes.
func (c *Client) message(data []byte) string {
	text := strings.TrimSpace(string(data))
	var obj map[string]any
	if json.Unmarshal(data, &obj) == nil {
		for _, k := range []string{"message", "error", "detail"} {
			if s, ok := obj[k].(string); ok && s != "" {
				text = s
				break
			}
		}
	}
	if c.key != "" {
		text = strings.ReplaceAll(text, c.key, "[redacted]")
	}
	if len(text) > maxMessage {
		text = strings.ToValidUTF8(text[:maxMessage], "")
	}
	return text
}

// retryAfter reads the delay a 429 asks for from retry-after-ms or
// Retry-After (whole seconds); zero when neither is a usable number.
func retryAfter(h http.Header) time.Duration {
	if d, ok := parseDelay(h.Get("retry-after-ms"), time.Millisecond); ok {
		return d
	}
	d, _ := parseDelay(h.Get("Retry-After"), time.Second)
	return d
}

// parseDelay reads a non-negative duration written as a count of unit. It
// reports false for a value that is not finite or that overflows
// time.Duration, so a malformed header reads as no delay rather than a
// negative one.
func parseDelay(s string, unit time.Duration) (time.Duration, bool) {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, false
	}
	if n >= float64(math.MaxInt64)/float64(unit) {
		return 0, false
	}
	return time.Duration(n * float64(unit)), true
}
