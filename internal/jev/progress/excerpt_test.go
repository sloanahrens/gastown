package progress

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTranscriptTailReadsInsideTown checks the exported reader returns the
// tail of a transcript under the town, in order, capped at the rune count.
func TestTranscriptTailReadsInsideTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, "session.jsonl")
	lines := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-10-21T17:00:01Z","message":{"content":[{"type":"text","text":"first line"}]}}`,
		`{"type":"assistant","timestamp":"2026-10-21T17:00:02Z","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git push origin main"}}]}}`,
		`{"type":"user","timestamp":"2026-10-21T17:00:03Z","message":{"content":[{"type":"tool_result","content":"pushed","is_error":false}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := TranscriptTail(town, town, path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first line", "[tool Bash] ", "git push origin main", "[result] pushed"} {
		if !strings.Contains(got, want) {
			t.Errorf("tail is missing %q: %q", want, got)
		}
	}
	if strings.Index(got, "first line") > strings.Index(got, "[result] pushed") {
		t.Errorf("tail is out of order: %q", got)
	}

	short, err := TranscriptTail(town, town, path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(short)); n != 10 {
		t.Errorf("tail of 10 runes has %d runes", n)
	}
	if !strings.HasSuffix(got, short) {
		t.Errorf("short tail %q is not the end of the full tail %q", short, got)
	}
}

// TestTranscriptTailRefusesOutsideTown checks a transcript outside the town and
// the seat's projects directories is refused with ErrOutsideTown, unread.
func TestTranscriptTailRefusesOutsideTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	elsewhere := t.TempDir()
	path := filepath.Join(elsewhere, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"assistant"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := TranscriptTail(town, filepath.Join(town, "no-worktree"), path, 100); !errors.Is(err, ErrOutsideTown) {
		t.Fatalf("err = %v, want ErrOutsideTown", err)
	}
}
