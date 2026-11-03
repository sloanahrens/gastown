package specdispatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// gt-hgt9j.7: a manual lint run left no record, so a bead's lint outcome could
// not be joined to the bead-change table.

// lintLogRecord is the outcome line as a reader decodes it.
type lintLogRecord struct {
	At     string   `json:"at"`
	Bead   string   `json:"bead"`
	Route  string   `json:"route"`
	Clean  bool     `json:"clean"`
	Fields []string `json:"fields"`
}

func decodeOutcome(t *testing.T, line []byte) lintLogRecord {
	t.Helper()
	var rec lintLogRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("OutcomeLine returned invalid JSON %q: %v", line, err)
	}
	return rec
}

// A refusal writes clean false and every refused field name in check order.
func TestLintLogOutcomeLineFields(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, time.November, 3, 20, 30, 0, 0, time.FixedZone("CET", 3600))
	v := LintVerdict{Route: RouteRefuse, Refusals: []Refusal{
		{Field: "## Goal", Reason: "section missing"},
		{Field: "acceptance", Reason: "no acceptance criteria"},
	}}

	rec := decodeOutcome(t, OutcomeLine(at, "gt-abc", v))

	if rec.Clean {
		t.Error("clean = true, want false for a refusal")
	}
	if want := []string{"## Goal", "acceptance"}; !slices.Equal(rec.Fields, want) {
		t.Errorf("fields = %v, want %v", rec.Fields, want)
	}
	if want := "2026-11-03T19:30:00Z"; rec.At != want {
		t.Errorf("at = %q, want %q (RFC 3339 UTC)", rec.At, want)
	}
	if rec.Bead != "gt-abc" {
		t.Errorf("bead = %q, want gt-abc", rec.Bead)
	}
	if want := string(RouteRefuse); rec.Route != want {
		t.Errorf("route = %q, want %q", rec.Route, want)
	}
}

// A clean verdict writes clean true and an empty fields array, not null.
func TestLintLogCleanLine(t *testing.T) {
	t.Parallel()
	line := OutcomeLine(time.Now(), "gt-ok", LintVerdict{Route: RouteDispatch})

	rec := decodeOutcome(t, line)
	if !rec.Clean {
		t.Error("clean = false, want true for a dispatchable verdict")
	}
	if !bytes.Contains(line, []byte(`"fields":[]`)) {
		t.Errorf("fields is not an empty array: %s", line)
	}
}

// Only the field names are written: a reason holding a newline cannot split
// the record across two lines.
func TestLintLogOneLineNoEmbeddedNewline(t *testing.T) {
	t.Parallel()
	reason := "the gate says:\n  run the tests first"
	v := LintVerdict{
		Route:    RouteRefuse,
		Field:    "## Gate",
		Reason:   reason,
		Refusals: []Refusal{{Field: "## Gate", Reason: reason}},
	}

	line := OutcomeLine(time.Now(), "gt-multi", v)

	if got := bytes.Count(line, []byte("\n")); got != 1 {
		t.Errorf("line holds %d newlines, want 1: %q", got, line)
	}
	if bytes.Contains(line, []byte("run the tests first")) {
		t.Errorf("the refusal reason reached the line: %q", line)
	}
	if rec := decodeOutcome(t, line); !slices.Equal(rec.Fields, []string{"## Gate"}) {
		t.Errorf("fields = %v, want [## Gate]", rec.Fields)
	}
}

func TestLintLogAppendWritesLine(t *testing.T) {
	t.Parallel()
	line := []byte("{\"bead\":\"gt-ok\"}\n")
	var gotPath string
	var gotLine []byte
	write := func(path string, p []byte) (int, error) {
		gotPath, gotLine = path, p
		return len(p), nil
	}

	if err := AppendOutcome("/town/.runtime/spec-lint/outcomes.jsonl", line, write); err != nil {
		t.Fatalf("AppendOutcome = %v, want nil", err)
	}
	if gotPath != "/town/.runtime/spec-lint/outcomes.jsonl" {
		t.Errorf("write path = %q", gotPath)
	}
	if !bytes.Equal(gotLine, line) {
		t.Errorf("write line = %q, want %q", gotLine, line)
	}
}

func TestLintLogAppendWriteError(t *testing.T) {
	t.Parallel()
	write := func(string, []byte) (int, error) { return 0, errors.New("disk full") }

	err := AppendOutcome("outcomes.jsonl", []byte("{}\n"), write)
	if err == nil {
		t.Fatal("AppendOutcome = nil, want the write's error")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error = %q, want the write's error named", err)
	}
}

func TestLintLogAppendShortWrite(t *testing.T) {
	t.Parallel()
	line := []byte("{\"bead\":\"gt-ok\"}\n")
	write := func(string, []byte) (int, error) { return len(line) - 1, nil }

	err := AppendOutcome("outcomes.jsonl", line, write)
	if err == nil {
		t.Fatal("AppendOutcome = nil, want a short write error")
	}
	if !strings.Contains(err.Error(), "short write") {
		t.Errorf("error = %q, want it to name the short write", err)
	}
}

// OutcomePath is the town runtime directory's spec-lint ledger.
func TestLintLogOutcomePath(t *testing.T) {
	t.Parallel()
	want := "/town/.runtime/spec-lint/outcomes.jsonl"
	if got := OutcomePath("/town"); got != want {
		t.Errorf("OutcomePath = %q, want %q", got, want)
	}
}
