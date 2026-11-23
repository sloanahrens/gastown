package seatship

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

// transcriptLine is one assistant API call, as usageattrib's session_test.go writes it.
const transcriptLine = `{"type":"assistant","timestamp":"2026-11-13T10:05:00Z","message":{"id":"%s","model":"claude-sonnet-5-5","usage":{"input_tokens":%d,"output_tokens":%d}}}` + "\n"

func writeTranscript(t *testing.T, path string, mtime time.Time, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestSeatShipUsageFrom(t *testing.T) {
	t.Parallel()
	t0, t1 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 11, 13, 11, 0, 0, 0, time.UTC)
	type file struct {
		at   time.Time
		body string
	}
	tests := []struct {
		name                       string
		files                      map[string]file
		start, end                 time.Time
		wantIDs                    []string
		wantCalls, wantIn, wantOut int64
		wantStart, wantEnd         time.Time
		wantComplete               bool
	}{
		{"two nested transcripts are summed and bound the record", map[string]file{"a/s1.jsonl": {t1, fmt.Sprintf(transcriptLine, "s1", 10, 20)}, "s2.jsonl": {t0, fmt.Sprintf(transcriptLine, "s2", 5, 6)}}, time.Time{}, time.Time{}, []string{"s1", "s2"}, 2, 15, 26, t0, t1, true},
		{"a session's subagents count once, with their session", map[string]file{"proj/sess-1.jsonl": {t1, fmt.Sprintf(transcriptLine, "own", 10, 20)}, "proj/sess-1/subagents/agent-x.jsonl": {t0, fmt.Sprintf(transcriptLine, "sub", 5, 6)}}, time.Time{}, time.Time{}, []string{"sess-1"}, 2, 15, 26, t1, t1, true},
		{"a message repeated by a resumed session counts once", map[string]file{"a.jsonl": {t0, fmt.Sprintf(transcriptLine, "dup", 10, 20)}, "b.jsonl": {t1, fmt.Sprintf(transcriptLine, "dup", 10, 20) + fmt.Sprintf(transcriptLine, "uniq", 5, 6)}}, time.Time{}, time.Time{}, []string{"a", "b"}, 2, 15, 26, t0, t1, true},
		{"an empty directory is incomplete and not an error", nil, time.Time{}, time.Time{}, nil, 0, 0, 0, time.Time{}, time.Time{}, false},
		{"a supplied bound is kept", map[string]file{"s1.jsonl": {t1, fmt.Sprintf(transcriptLine, "s1", 10, 20)}}, t0, t1.Add(time.Hour), []string{"s1"}, 1, 10, 20, t0, t1.Add(time.Hour), true},
		{"a line that does not parse marks the record incomplete", map[string]file{"s1.jsonl": {t1, fmt.Sprintf(transcriptLine, "s1", 10, 20) + "{not json\n"}}, time.Time{}, time.Time{}, []string{"s1"}, 1, 10, 20, t1, t1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for name, f := range tt.files {
				writeTranscript(t, filepath.Join(dir, name), f.at, f.body)
			}
			rec, ids, err := UsageFrom(dir, usageattrib.Facts{Bead: "gt-1", Kind: dashboard.UsageKindWork,
				Class: dashboard.UsageClassPolecat, Rig: "gastown", Start: tt.start, End: tt.end, Complete: true})
			if err != nil {
				t.Fatalf("UsageFrom: %v", err)
			}
			if got, want := fmt.Sprintf("%v | %d/%d/%d | %v..%v | %t", ids, rec.Totals.Calls, rec.Totals.Input, rec.Totals.Output, rec.Start.UTC(), rec.End.UTC(), rec.Complete),
				fmt.Sprintf("%v | %d/%d/%d | %v..%v | %t", tt.wantIDs, tt.wantCalls, tt.wantIn, tt.wantOut, tt.wantStart, tt.wantEnd, tt.wantComplete); got != want {
				t.Errorf("record = %s, want %s", got, want)
			}
		})
	}
}

func TestSeatShipUsageFromUnreadableDirectory(t *testing.T) {
	t.Parallel()
	if _, _, err := UsageFrom(filepath.Join(t.TempDir(), "absent"), usageattrib.Facts{}); err == nil {
		t.Fatal("UsageFrom accepted a directory that cannot be read")
	}
}
