package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// gt-hgt9j.7: `gt spec lint` records each manual run so a bead's lint outcome
// can be joined to the bead-change table.

// TestSpecLintOutcomeAppends runs the command twice against a temp town root
// and expects one outcome line per run.
func TestSpecLintOutcomeAppends(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, time.November, 3, 19, 30, 0, 0, time.UTC)
	spec := cleanSpec("gt-ok", 2, "2026-01-01")

	for run := 1; run <= 2; run++ {
		var out, errOut bytes.Buffer
		if err := runSpecLintAt(&out, &errOut, town, spec.ID, spec, nil, "", nil, false, now); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if errOut.Len() != 0 {
			t.Errorf("run %d wrote to stderr: %q", run, errOut.String())
		}
	}

	data, err := os.ReadFile(specdispatch.OutcomePath(town))
	if err != nil {
		t.Fatalf("read outcomes file: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("outcome lines = %d, want 2:\n%s", len(lines), data)
	}
	for i, line := range lines {
		var rec struct {
			Bead  string `json:"bead"`
			Clean bool   `json:"clean"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line %d is not JSON: %v", i+1, err)
			continue
		}
		if rec.Bead != "gt-ok" || !rec.Clean {
			t.Errorf("line %d = %+v, want gt-ok and clean", i+1, rec)
		}
	}
}

// TestSpecLintOutcomeWriteFailureKeepsExit expects an unwritable runtime
// directory to print one warning and leave the verdict's exit code alone.
func TestSpecLintOutcomeWriteFailureKeepsExit(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A regular file where the runtime directory belongs: creating the ledger's
	// directory fails.
	if err := os.WriteFile(constants.TownRuntimePath(town), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := cleanSpec("gt-no", 2, "2026-01-01")
	spec.Description = "" // every required section is missing: the lint refuses

	var out, errOut bytes.Buffer
	err := runSpecLintAt(&out, &errOut, town, spec.ID, spec, nil, "", nil, false, time.Now())

	code, silent := IsSilentExit(err)
	if !silent || code != specLintExitRefused {
		t.Fatalf("error = %v, want a silent exit %d", err, specLintExitRefused)
	}
	if !strings.Contains(errOut.String(), "lint outcome not recorded") {
		t.Errorf("stderr is missing the recording warning:\n%s", errOut.String())
	}
}

// TestSpecLintOutcomeAppendOpenFailure drives the branch where the ledger file
// cannot be opened.
func TestSpecLintOutcomeAppendOpenFailure(t *testing.T) {
	t.Parallel()
	open := func(string) (io.WriteCloser, error) { return nil, errors.New("open refused") }

	_, err := appendOutcomeFileWith(open, filepath.Join(t.TempDir(), "outcomes.jsonl"), []byte("{}\n"))

	if err == nil || !strings.Contains(err.Error(), "open refused") {
		t.Errorf("appendOutcomeFileWith = %v, want the open error", err)
	}
}

// TestSpecLintOutcomeAppendWriteFailure drives the branch where the ledger
// write fails after the file is open.
func TestSpecLintOutcomeAppendWriteFailure(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write refused")
	open := func(string) (io.WriteCloser, error) {
		return &failingWriteCloser{err: writeErr}, nil
	}

	_, err := appendOutcomeFileWith(open, filepath.Join(t.TempDir(), "outcomes.jsonl"), []byte("{}\n"))

	if !errors.Is(err, writeErr) {
		t.Errorf("appendOutcomeFileWith = %v, want the write error", err)
	}
}

// failingWriteCloser is an opened ledger file whose write fails.
type failingWriteCloser struct{ err error }

func (f *failingWriteCloser) Write([]byte) (int, error) { return 0, f.err }
func (f *failingWriteCloser) Close() error              { return nil }
