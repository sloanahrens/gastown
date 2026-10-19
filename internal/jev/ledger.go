package jev

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// defaultCaller names a client that sets no caller, so a reader can still
// group its lines.
const defaultCaller = "jev"

// LedgerRecord is one line of the usage ledger: what one answered Choice call
// spent, and nothing of the question or answer.
type LedgerRecord struct {
	// TS is when the call returned.
	TS           time.Time `json:"ts"`
	Caller       string    `json:"caller"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
}

// UsageLedgerPath is the town's default usage ledger, which the spend reader
// and the replay share.
func UsageLedgerPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "usage.jsonl")
}

// WithLedger makes the client append one line per answered Choice call to
// path, creating the directory 0700 and the file 0600.
func WithLedger(path string) Option { return func(c *Client) { c.ledgerPath = path } }

// WithCaller names this client in the usage ledger, for a reader that breaks
// spend down by caller; empty keeps defaultCaller.
func WithCaller(name string) Option { return func(c *Client) { c.caller = name } }

// WithWarnings sets where a ledger write failure is reported; nil discards it.
func WithWarnings(w io.Writer) Option { return func(c *Client) { c.warnings = w } }

// ledger appends one JSON line per answered call. The write is best-effort: a
// failure warns and leaves the Choice result alone, because a full disk must
// not cost the caller its answer.
type ledger struct {
	path     string
	caller   string
	warnings io.Writer
}

// record appends one line for u. It never returns an error.
func (l *ledger) record(u Usage) {
	if l == nil {
		return
	}
	line, err := json.Marshal(LedgerRecord{
		TS:           time.Now().UTC(),
		Caller:       l.caller,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
	})
	if err != nil {
		l.warnf(err)
		return
	}
	if err := appendLine(l.path, line); err != nil {
		l.warnf(err)
	}
}

func (l *ledger) warnf(err error) {
	if l.warnings != nil {
		fmt.Fprintf(l.warnings, "jev: usage ledger %s: %v\n", l.path, err)
	}
}

// appendLine writes line and its newline to path in one write, so lines from
// concurrent clients stay whole.
func appendLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
