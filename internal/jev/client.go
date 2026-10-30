// Package jev is the client for TypeSafe's Jev model: it asks a set of Choice,
// Noul and Score questions about one state in a single POST /v1/systemone and
// returns every answer with the call's token usage.
//
// A Choice picks one of named options, a Noul answers a yes/no statement with
// a probability, and a Score places the state on an ordered rubric. One Ask
// carries all three, paying the state's tokens and the output once.
//
// The API key comes from a 0600 key file (see KeyPaths) and appears in
// neither errors nor logs. The usage ledger (WithLedger) records token counts
// alone; the request log (WithRequests) records a hash and sizes per body
// sent, never its text, and which scrubbing rules fired (WithScrub).
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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/jev/scrub"
)

const (
	// Model is the pinned model; "jev-latest" would move under the caller.
	Model = "jev-1.13.0"

	defaultBaseURL = "https://api.typesafe.ai/v1/systemone"

	// DefaultTimeout bounds one call, connect to last body byte.
	DefaultTimeout = 30 * time.Second

	// questionID names the one question in each Choice request; the model
	// never sees it.
	questionID = "q"

	// The question types the vendor API accepts, and the Score rubric width.
	typeChoice     = "choice"
	typeNoul       = "noul"
	typeScore      = "score"
	minScoreLevels = 2
	maxScoreLevels = 10

	// maxBody caps how much of a response is read.
	maxBody = 1 << 20

	// maxMessage caps the server error text carried in a StatusError.
	maxMessage = 200

	// probabilityTolerance absorbs floating-point error in the sum of a
	// response's probabilities, which JSON may deliver as 0.9999999999999999.
	probabilityTolerance = 1e-6

	// roundingPerOption is how far one probability may sit from its true value
	// when Jev rounds it to two decimals: half of the last place.
	roundingPerOption = 0.005

	// mostProbableSlack is how far below the highest probability the chosen
	// option may sit and still count as a tie after rounding.
	mostProbableSlack = 0.01
)

// Client asks Choice questions of one Jev endpoint.
type Client struct {
	httpClient *http.Client
	baseURL    string
	key        string
	keyPaths   []string
	timeout    time.Duration

	// ledgerPath, caller and warnings configure the usage ledger; ledger is
	// built from them by NewClient, and nil when no path is set.
	ledgerPath string
	caller     string
	warnings   io.Writer
	ledger     *ledger

	// requestsPath and requests configure the request log; requests is built
	// from them by NewClient, and nil when no path is set.
	requestsPath string
	requests     *requestLog

	// scrub, when set, scrubs the state before the body is built; nil sends
	// the state as the caller passed it. scrubSet records that WithScrub was
	// given, so NewClient can refuse a nil scrubber.
	scrub    *scrub.Scrubber
	scrubSet bool

	// shadowPercent, shadowAllows and shadowSet configure the shadow scrub check
	// (WithShadowScrub). shadows tracks the background calls it starts, so a
	// test can wait for them.
	shadowPercent int
	shadowAllows  func() (bool, error)
	shadowSet     bool
	shadows       sync.WaitGroup
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
	if c.scrubSet && c.scrub == nil {
		return nil, errors.New("jev: WithScrub needs a scrubber; a nil one would send the state unscrubbed")
	}
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}
	if c.caller == "" {
		c.caller = defaultCaller
	}
	if c.ledgerPath != "" {
		c.ledger = &ledger{path: c.ledgerPath, caller: c.caller, warnings: c.warnings}
		if c.requestsPath == "" {
			c.requestsPath = filepath.Join(filepath.Dir(c.ledgerPath), requestsFile)
		}
	}
	if c.requestsPath != "" {
		c.requests = &requestLog{path: c.requestsPath, warnings: c.warnings}
	}
	if err := c.checkShadowOptions(); err != nil {
		return nil, err
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

// Question is one question in an Ask, of type "choice", "noul" or "score".
type Question struct {
	// Type is "choice" (asks one of Options), "noul" (asks whether a statement
	// holds) or "score" (asks which of Levels fits).
	Type string
	// Instructions tells the model what to judge.
	Instructions string
	// Options maps each choice option's name to its description; a choice
	// needs at least one.
	Options map[string]string
	// Levels describes a score's 2 to 10 rubric levels, low to high; the
	// answer's Score indexes this slice.
	Levels []string
	// True and False describe what a noul's 1 and 0 mean; both empty asks the
	// bare statement.
	True, False string
}

// Answers holds every answer to one Ask, keyed by question id; a map is empty
// when no question of its type was asked.
type Answers struct {
	// Model is the versioned model ID that answered.
	Model string
	// Usage is the token usage of the call.
	Usage Usage
	// Choice maps each choice id to its answer.
	Choice map[string]ChoiceAnswer
	// Noul maps each noul id to the probability that its statement holds.
	Noul map[string]float64
	// Score maps each score id to its answer.
	Score map[string]ScoreAnswer
}

// ChoiceAnswer is the answer to one choice question.
type ChoiceAnswer struct {
	// Choice is the option with the highest probability.
	Choice string
	// Probabilities maps every option asked to its probability.
	Probabilities map[string]float64
	// Confidence is 0 to 1, derived from how spread Probabilities is.
	Confidence float64
}

// ScoreAnswer is the answer to one score question.
type ScoreAnswer struct {
	// Score is the chosen level, 0 to len(Levels)-1.
	Score float64
	// Confidence is 0 to 1.
	Confidence float64
	// Probabilities maps each level label in Legend to its probability.
	Probabilities map[string]float64
	// Legend labels the levels Probabilities is keyed by.
	Legend map[string]string
}

type question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	// Criteria is a map for choice and noul, a []string for score, and absent
	// for a noul that describes neither outcome.
	Criteria any `json:"criteria,omitempty"`
}

type request struct {
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]question `json:"questions"`
}

// answer is the wire answer of any type; only the fields its Type names are
// read. Noul and Score are pointers so a missing value reads as missing, not
// as zero.
type answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
	Noul          *float64           `json:"noul"`
	Score         *float64           `json:"score"`
	Legend        map[string]string  `json:"legend"`
}

type response struct {
	Model   string            `json:"model"`
	Answers map[string]answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Ask sends one request carrying every question in qs about state and returns
// every answer keyed by question id. It makes one HTTP call and never retries:
// a deadline returns ErrTimeout, a 429 returns a *RateLimitError, a 401
// returns ErrUnauthorized, any other non-2xx returns a *StatusError, and a 2xx
// that does not answer every question as asked returns ErrBadResponse. The
// usage ledger records one line per call whose 2xx reply decodes, bad answers
// included; the request log records one line per call that sent a request,
// failed calls included, and none for a call that stopped before sending.
func (c *Client) Ask(ctx context.Context, state any, qs map[string]Question) (*Answers, error) {
	if len(qs) == 0 {
		return nil, errors.New("jev: Ask needs at least one question")
	}
	wire, err := wireQuestions(qs)
	if err != nil {
		return nil, err
	}
	sent, stateBytes, report, err := c.sentState(state)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(request{State: sent, Model: Model, Questions: wire})
	if err != nil {
		return nil, fmt.Errorf("jev: encode request: %w", err)
	}
	data, got, err := c.post(ctx, body)
	if c.requests != nil && got.sent {
		c.requests.record(c.requestRecord(state, stateBytes, body, qs, got, err, report))
	}
	if err != nil {
		return nil, err
	}
	out, err := parseAnswers(data, qs)
	if out == nil {
		return nil, err
	}
	c.ledger.record(out.Usage)
	if err != nil {
		return nil, err
	}
	c.shadowCheck(ctx, state, qs, wire, out)
	return out, nil
}

// Choice asks which of criteria's options fits state under instructions: one
// choice question, with Ask's errors and ledger line.
func (c *Client) Choice(ctx context.Context, state any, instructions string, criteria map[string]string) (*Answer, error) {
	if len(criteria) == 0 {
		return nil, errors.New("jev: a Choice needs at least one option")
	}
	got, err := c.Ask(ctx, state, map[string]Question{
		questionID: {Type: typeChoice, Instructions: instructions, Options: criteria},
	})
	if err != nil {
		return nil, err
	}
	a := got.Choice[questionID]
	return &Answer{
		Choice:        a.Choice,
		Probabilities: a.Probabilities,
		Confidence:    a.Confidence,
		Model:         got.Model,
		Usage:         got.Usage,
	}, nil
}

// reply is what an HTTP response carried beyond its body, for the request
// log.
type reply struct {
	// sent is whether the round trip was attempted; a request the client
	// could not build never left, so it writes no log line.
	sent bool
	// status is the HTTP status code, or zero when no response arrived.
	status int
	// requestID is the X-Request-Id header, empty when the server sent none.
	requestID string
	// bodyReadFailed is whether a response arrived whose body could not be
	// read, so status is not the call's outcome.
	bodyReadFailed bool
}

// post sends body to the endpoint and returns the 2xx response body, what the
// reply carried, and the error Ask documents for a failed round trip or a
// non-2xx status.
func (c *Client) post(ctx context.Context, body []byte) ([]byte, reply, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, reply{}, c.redact(fmt.Errorf("jev: build request: %w", err))
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, reply{sent: true}, c.transportErr(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got := reply{sent: true, status: resp.StatusCode, requestID: resp.Header.Get("X-Request-Id")}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		got.bodyReadFailed = true
		return nil, got, c.transportErr(ctx, err)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, got, &RateLimitError{RetryAfter: retryAfter(resp.Header)}
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, got, ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, got, &StatusError{Status: resp.StatusCode, Message: c.message(data)}
	}
	return data, got, nil
}

// wireQuestions converts qs to the vendor's shape, rejecting a question it
// cannot carry before any request leaves.
func wireQuestions(qs map[string]Question) (map[string]question, error) {
	out := make(map[string]question, len(qs))
	for id, q := range qs {
		w, err := wireQuestion(q)
		if err != nil {
			return nil, fmt.Errorf("jev: question %q: %w", id, err)
		}
		out[id] = w
	}
	return out, nil
}

func wireQuestion(q Question) (question, error) {
	w := question{Type: q.Type, Instructions: q.Instructions}
	switch q.Type {
	case typeChoice:
		if len(q.Options) == 0 {
			return question{}, errors.New("a choice needs at least one option")
		}
		w.Criteria = q.Options
	case typeNoul:
		if criteria := noulCriteria(q.True, q.False); criteria != nil {
			w.Criteria = criteria
		}
	case typeScore:
		if n := len(q.Levels); n < minScoreLevels || n > maxScoreLevels {
			return question{}, fmt.Errorf("a score needs %d to %d levels, got %d", minScoreLevels, maxScoreLevels, n)
		}
		w.Criteria = q.Levels
	default:
		return question{}, fmt.Errorf("unknown type %q", q.Type)
	}
	return w, nil
}

// noulCriteria describes a noul's outcomes, leaving out the empty one and
// returning nil when neither is described so the wire omits criteria.
func noulCriteria(trueDesc, falseDesc string) map[string]string {
	criteria := map[string]string{}
	if trueDesc != "" {
		criteria["true"] = trueDesc
	}
	if falseDesc != "" {
		criteria["false"] = falseDesc
	}
	if len(criteria) == 0 {
		return nil
	}
	return criteria
}

// parseAnswers decodes a 2xx body and returns ErrBadResponse unless it answers
// every question asked, with the type asked and a value that type documents.
// A body that decodes comes back with its Usage even when its answers are bad,
// so the call's tokens are still billed to the ledger; a body that does not
// decode returns nil.
func parseAnswers(data []byte, qs map[string]Question) (*Answers, error) {
	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, unusable(CheckDecode, nil, "decode: %v", err)
	}
	out := &Answers{
		Model:  r.Model,
		Usage:  r.Usage,
		Choice: make(map[string]ChoiceAnswer, len(qs)),
		Noul:   make(map[string]float64, len(qs)),
		Score:  make(map[string]ScoreAnswer, len(qs)),
	}
	for id, q := range qs {
		a, ok := r.Answers[id]
		if !ok {
			return out, answerErr(id, len(qs), unusable(CheckMissingAnswer, nil, "no %s answer", q.Type))
		}
		if a.Type != q.Type {
			return out, answerErr(id, len(qs), unusable(CheckWrongType, nil, "no %s answer", q.Type))
		}
		switch q.Type {
		case typeChoice:
			ca, bad := parseChoiceAnswer(a, q.Options)
			if bad != nil {
				return out, answerErr(id, len(qs), bad)
			}
			out.Choice[id] = ca
		case typeNoul:
			if a.Noul == nil {
				return out, answerErr(id, len(qs), unusable(CheckNoulMissing, nil, "no noul value"))
			}
			if *a.Noul < 0 || *a.Noul > 1 {
				v := *a.Noul
				return out, answerErr(id, len(qs), unusable(CheckNoulRange, &v, "noul %v is outside 0 to 1", v))
			}
			out.Noul[id] = *a.Noul
		case typeScore:
			sa, bad := parseScoreAnswer(a, len(q.Levels))
			if bad != nil {
				return out, answerErr(id, len(qs), bad)
			}
			out.Score[id] = sa
		}
	}
	return out, nil
}

// answerErr returns bad for question id. The id is named only when Ask asked
// several questions, where it says which answer is bad; a lone question, which
// is all Choice asks, keeps the text Choice has always returned.
func answerErr(id string, asked int, bad *BadResponse) error {
	if asked > 1 {
		bad.Question = id
	}
	return bad
}

// parseChoiceAnswer returns a choice answer that names one of options, whose
// probabilities cover exactly those options and sum to 1 within rounding, and
// whose named option is the most probable or ties it after rounding.
func parseChoiceAnswer(a answer, options map[string]string) (ChoiceAnswer, *BadResponse) {
	if _, ok := options[a.Choice]; !ok {
		return ChoiceAnswer{}, unusable(CheckChoiceNotAnOption, nil, "choice %q is not an option asked", a.Choice)
	}
	if a.Confidence < 0 || a.Confidence > 1 {
		c := a.Confidence
		return ChoiceAnswer{}, unusable(CheckConfidenceRange, &c, "confidence %v is outside 0 to 1", c)
	}
	if bad := checkProbabilities(a.Probabilities, options); bad != nil {
		return ChoiceAnswer{}, bad
	}
	if highest(a.Probabilities)-a.Probabilities[a.Choice] > mostProbableSlack+probabilityTolerance {
		return ChoiceAnswer{}, unusable(CheckNotMostProbable, nil, "choice %q is not the most probable option", a.Choice)
	}
	return ChoiceAnswer{
		Choice:        a.Choice,
		Probabilities: a.Probabilities,
		Confidence:    a.Confidence,
	}, nil
}

// parseScoreAnswer checks that a score answer names a level on the rubric and
// labels every level once, with a probability and a legend entry under the
// same label.
func parseScoreAnswer(a answer, levels int) (ScoreAnswer, *BadResponse) {
	if a.Score == nil {
		return ScoreAnswer{}, unusable(CheckScoreMissing, nil, "no score value")
	}
	if *a.Score < 0 || *a.Score > float64(levels-1) {
		s := *a.Score
		return ScoreAnswer{}, unusable(CheckScoreRange, &s, "score %v is outside 0 to %d", s, levels-1)
	}
	if a.Confidence < 0 || a.Confidence > 1 {
		c := a.Confidence
		return ScoreAnswer{}, unusable(CheckConfidenceRange, &c, "confidence %v is outside 0 to 1", c)
	}
	if len(a.Probabilities) != levels {
		return ScoreAnswer{}, unusable(CheckProbabilitiesCount, nil, "probabilities cover %d levels, %d were asked", len(a.Probabilities), levels)
	}
	if len(a.Legend) != levels {
		return ScoreAnswer{}, unusable(CheckLegendCount, nil, "legend labels %d levels, %d were asked", len(a.Legend), levels)
	}
	for label := range a.Legend {
		if _, ok := a.Probabilities[label]; !ok {
			return ScoreAnswer{}, unusable(CheckLegendUnknownLabel, nil, "legend label %q has no probability", label)
		}
	}
	if bad := checkSum(a.Probabilities, levels); bad != nil {
		return ScoreAnswer{}, bad
	}
	return ScoreAnswer{
		Score:         *a.Score,
		Confidence:    a.Confidence,
		Probabilities: a.Probabilities,
		Legend:        a.Legend,
	}, nil
}

// checkProbabilities checks that probabilities name every option asked and no
// other, each within 0 to 1, and sum to 1 within rounding.
func checkProbabilities(probabilities map[string]float64, criteria map[string]string) *BadResponse {
	if len(probabilities) != len(criteria) {
		return unusable(CheckProbabilitiesCount, nil, "probabilities cover %d options, %d were asked", len(probabilities), len(criteria))
	}
	for name := range probabilities {
		if _, ok := criteria[name]; !ok {
			return unusable(CheckProbabilitiesUnknownOption, nil, "probability for %q, which was not asked", name)
		}
	}
	return checkSum(probabilities, len(criteria))
}

// checkSum checks that each probability is within 0 to 1 and the total is 1,
// allowing each of the count options its share of Jev's two-decimal rounding.
func checkSum(probabilities map[string]float64, count int) *BadResponse {
	if len(probabilities) == 0 {
		return unusable(CheckProbabilitiesCount, nil, "no probabilities")
	}
	var sum float64
	for name, p := range probabilities {
		if p < 0 || p > 1 {
			return unusable(CheckProbabilityRange, &p, "probability for %q is %v, outside 0 to 1", name, p)
		}
		sum += p
	}
	if math.Abs(sum-1) > float64(count)*roundingPerOption+probabilityTolerance {
		return unusable(CheckProbabilitiesSum, &sum, "probabilities sum to %v, not 1", sum)
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
