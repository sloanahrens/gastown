package jev

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// requestsFile is the request log's name in the usage ledger's directory.
const requestsFile = "requests.jsonl"

// The status classes a request line carries in place of an HTTP status: no
// reply arrived, or a reply arrived whose body could not be read.
const (
	statusTimeout        = "timeout"
	statusTransport      = "transport"
	statusBodyReadFailed = "body_read_failed"
)

// RequestsPath is the town's default request log, a sibling of
// UsageLedgerPath.
func RequestsPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", requestsFile)
}

// WithRequests makes the client append one line per Ask that reaches the
// network to path; WithLedger's client writes requests.jsonl beside its
// ledger unless this sets another path.
func WithRequests(path string) Option { return func(c *Client) { c.requestsPath = path } }

// CutState is implemented by a state a battery cut before sending it: Cut
// reports the state's size before and after the cut, and whether it cut at
// all (gt-s0tsg).
type CutState interface {
	Cut() (before, after int, cut bool)
}

// RequestRecord is one line of the request log: proof of what one Ask sent,
// with none of the text it sent.
type RequestRecord struct {
	// TS is when the call finished.
	TS time.Time `json:"ts"`
	// Caller is the client's caller name.
	Caller string `json:"caller"`
	// QuestionIDs are the ids asked, sorted.
	QuestionIDs []string `json:"question_ids"`
	// RequestID is the response's X-Request-Id header, empty when the server
	// sent none.
	RequestID string `json:"request_id"`
	// StateBytes is the size of the state as the client marshaled it.
	StateBytes int `json:"state_bytes"`
	// BodyBytes is the exact size of the body posted.
	BodyBytes int `json:"body_bytes"`
	// BodySHA256 is the hex SHA-256 of the exact body posted.
	BodySHA256 string `json:"body_sha256"`
	// Status is the HTTP status the server answered, or one of the strings
	// "timeout" and "transport" when no reply arrived, or "body_read_failed"
	// when a reply arrived whose body could not be read.
	Status any `json:"status"`
	// HTTPStatus is the status a reply carried when Status names
	// "body_read_failed"; empty otherwise, since Status then holds it.
	HTTPStatus int `json:"http_status,omitempty"`
	// StateBytesBeforeCut is the state's size before a battery cut it, present
	// only when the state implements CutState.
	StateBytesBeforeCut *int `json:"state_bytes_before_cut,omitempty"`
	// Cut is whether a state implementing CutState was cut, present only then.
	Cut *bool `json:"cut,omitempty"`
}

// requestLog appends one JSON line per request that reached the network. The
// write is best-effort, as the usage ledger's is: a failure warns and never
// fails the call. A client holds one only when a request log is configured,
// so record is never called otherwise.
type requestLog struct {
	path     string
	warnings io.Writer
}

// record appends one line for r. It never returns an error.
func (l *requestLog) record(r *RequestRecord) {
	line, err := json.Marshal(r)
	if err != nil {
		l.warnf(err)
		return
	}
	if err := appendLine(l.path, line); err != nil {
		l.warnf(err)
	}
}

func (l *requestLog) warnf(err error) {
	if l.warnings != nil {
		fmt.Fprintf(l.warnings, "jev: request log %s: %v\n", l.path, err)
	}
}

// stateSize returns the size of state as the request body carries it, and
// zero when no request log is configured: the body marshal is then the only
// one a call pays for.
func (c *Client) stateSize(state any) (int, error) {
	if c.requests == nil {
		return 0, nil
	}
	b, err := json.Marshal(state)
	if err != nil {
		return 0, fmt.Errorf("jev: encode request: %w", err)
	}
	return len(b), nil
}

// requestRecord builds the log line for one posted request from what the
// client sent and how the call ended.
func (c *Client) requestRecord(state any, stateBytes int, body []byte, qs map[string]Question, got reply, err error) *RequestRecord {
	sum := sha256.Sum256(body)
	rec := &RequestRecord{
		TS:          time.Now().UTC(),
		Caller:      c.caller,
		QuestionIDs: questionIDs(qs),
		RequestID:   got.requestID,
		StateBytes:  stateBytes,
		BodyBytes:   len(body),
		BodySHA256:  hex.EncodeToString(sum[:]),
		Status:      requestStatus(got, err),
	}
	if got.bodyReadFailed {
		rec.HTTPStatus = got.status
	}
	if cs, ok := state.(CutState); ok {
		// after is the battery's own count of the state it passes in; the line
		// records the size the client actually marshaled.
		before, _, wasCut := cs.Cut()
		rec.StateBytesBeforeCut = &before
		rec.Cut = &wasCut
	}
	return rec
}

// questionIDs returns qs's ids in a stable order.
func questionIDs(qs map[string]Question) []string {
	ids := make([]string, 0, len(qs))
	for id := range qs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// requestStatus is the HTTP status of a reply, the class of the failure that
// kept one from arriving, or the class of a reply whose body could not be
// read.
func requestStatus(got reply, err error) any {
	if got.bodyReadFailed {
		return statusBodyReadFailed
	}
	if got.status != 0 {
		return got.status
	}
	if errors.Is(err, ErrTimeout) {
		return statusTimeout
	}
	return statusTransport
}

// ReadRequests returns up to recent request records from the log at path,
// oldest first, keeping only those whose caller is caller; an empty caller
// keeps every line. It also returns how many non-empty lines it could not
// read, so a caller can say the log holds lines it did not look at. A missing
// log is no records, not an error.
func ReadRequests(path, caller string, recent int) ([]RequestRecord, int, error) {
	if recent < 1 {
		return nil, 0, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("opening the request log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var (
		out     []RequestRecord
		skipped int
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var rec RequestRecord
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			skipped++
			continue
		}
		if caller != "" && rec.Caller != caller {
			continue
		}
		if len(out) == recent {
			out = out[1:]
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, skipped, fmt.Errorf("reading the request log: %w", err)
	}
	return out, skipped, nil
}
