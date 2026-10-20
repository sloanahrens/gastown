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

// LedgerRecord is one line of the usage ledger: what one answered call spent,
// and nothing of the questions or answers.
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

// WithLedger makes the client append one line per answered Ask call to path,
// creating the directory 0700 and the file 0600, and setting both to those
// modes if they already exist.
func WithLedger(path string) Option { return func(c *Client) { c.ledgerPath = path } }

// WithCaller names this client in the usage ledger, for a reader that breaks
// spend down by caller; empty keeps defaultCaller.
func WithCaller(name string) Option { return func(c *Client) { c.caller = name } }

// WithWarnings sets where a ledger write failure is reported; nil discards it.
func WithWarnings(w io.Writer) Option { return func(c *Client) { c.warnings = w } }

// ledger appends one JSON line per answered call. The write is best-effort: a
// failure warns and never fails the call, so a full disk cannot cost the
// caller its answer.
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

// appendLine writes line and its newline to path under an exclusive lock, so
// lines from concurrent clients stay whole even when a write is split.
// MkdirAll and OpenFile keep the mode of an existing directory or file, so the
// modes are set here too.
func appendLine(path string, line []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	unlock, err := lockLedger(f)
	if err != nil {
		return err
	}
	defer unlock()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}
