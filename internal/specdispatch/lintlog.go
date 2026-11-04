// Package specdispatch — the manual lint outcome ledger (gt-hgt9j.7).
//
// A manual `gt spec lint` run left no record, so "does a bead that passes lint
// first time land faster" could not be asked: the events archive's bead-change
// table can join on a bead id only when that bead's lint outcome exists. This
// file renders one record per run and appends it; reading the file and joining
// it belong to the mango dataset, not the command.
package specdispatch

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// outcomeDir is the subdirectory of the town runtime directory holding the
// manual lint ledger.
const outcomeDir = "spec-lint"

// OutcomePath is outcomes.jsonl in townRoot's spec-lint directory.
func OutcomePath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), outcomeDir, "outcomes.jsonl")
}

// outcomeRecord is one manual lint run as the ledger stores it.
type outcomeRecord struct {
	At     string   `json:"at"`
	Bead   string   `json:"bead"`
	Route  Route    `json:"route"`
	Clean  bool     `json:"clean"`
	Fields []string `json:"fields"`
}

// OutcomeLine renders one manual lint outcome as a JSON object and a newline:
// at (RFC 3339, UTC), bead, route, clean, and fields, the refused field names
// in check order. A refusal's reason is never written — it can hold a newline,
// and the ledger is one record per line (gt-hgt9j.7). A clean verdict writes an
// empty fields array rather than null, so a reader can range it.
func OutcomeLine(now time.Time, id string, v LintVerdict) []byte {
	fields := make([]string, 0, len(v.Refusals))
	for _, r := range v.Refusals {
		fields = append(fields, r.Field)
	}
	// The record holds only strings and a bool, so its JSON cannot fail to encode.
	line, _ := json.Marshal(outcomeRecord{
		At:     now.UTC().Format(time.RFC3339),
		Bead:   id,
		Route:  v.Route,
		Clean:  v.Clean(),
		Fields: fields,
	})
	return append(line, '\n')
}

// AppendOutcome appends line to the file at path. write performs the append —
// os-level in gt spec lint, a recorder in a test — so this function opens
// nothing and its test needs no filesystem.
func AppendOutcome(path string, line []byte, write func(path string, p []byte) (int, error)) error {
	n, err := write(path, line)
	if err != nil {
		return fmt.Errorf("append lint outcome to %s: %w", path, err)
	}
	if n != len(line) {
		return fmt.Errorf("append lint outcome to %s: short write (%d of %d bytes)", path, n, len(line))
	}
	return nil
}
