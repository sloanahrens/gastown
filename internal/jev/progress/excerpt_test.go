package progress

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	got, err := TranscriptTail(town, town, path, transcriptEnd, 1000)
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

	short, err := TranscriptTail(town, town, path, transcriptEnd, 10)
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
	if _, err := TranscriptTail(town, filepath.Join(town, "no-worktree"), path, transcriptEnd, 100); !errors.Is(err, ErrOutsideTown) {
		t.Fatalf("err = %v, want ErrOutsideTown", err)
	}
}

// transcriptEnd is later than every event the tests write, so a tail with it
// holds the whole transcript.
var transcriptEnd = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)

// TestTranscriptTailStopsAtSubmission checks the excerpt ends at the submission:
// events stamped after it are left out, and two submissions over one transcript
// read two different excerpts.
func TestTranscriptTailStopsAtSubmission(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, "session.jsonl")
	lines := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-10-21T17:00:01Z","message":{"content":[{"type":"text","text":"before the first submit"}]}}`,
		`{"type":"assistant","timestamp":"2026-10-21T17:00:05Z","message":{"content":[{"type":"text","text":"after the first submit"}]}}`,
		`{"type":"assistant","timestamp":"2026-10-21T17:00:09Z","message":{"content":[{"type":"text","text":"after the second submit"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	first := time.Date(2026, 10, 21, 17, 0, 3, 0, time.UTC)
	got, err := TranscriptTail(town, town, path, first, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "before the first submit") {
		t.Errorf("tail to the first submit is missing the earlier event: %q", got)
	}
	if strings.Contains(got, "after the first submit") || strings.Contains(got, "after the second submit") {
		t.Errorf("tail to the first submit holds events after it: %q", got)
	}

	second := time.Date(2026, 10, 21, 17, 0, 6, 0, time.UTC)
	later, err := TranscriptTail(town, town, path, second, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(later, "after the first submit") || strings.Contains(later, "after the second submit") {
		t.Errorf("tail to the second submit = %q, want the first-submit event and not the second", later)
	}
	if got == later {
		t.Errorf("two submissions over one transcript read the same excerpt: %q", got)
	}
}

// TestTranscriptStartReadsFirstTimestamp checks the start is the first
// timestamped line's, read past a line with no timestamp, and that a transcript
// with none reads as the zero time.
func TestTranscriptStartReadsFirstTimestamp(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, "session.jsonl")
	lines := strings.Join([]string{
		`{"type":"summary","summary":"earlier work"}`,
		`{"type":"assistant","timestamp":"2026-10-21T17:00:01Z","message":{"content":[{"type":"text","text":"hi"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := TranscriptStart(town, town, path)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 21, 17, 0, 1, 0, time.UTC); !got.Equal(want) {
		t.Errorf("start = %v, want %v", got, want)
	}

	bare := filepath.Join(town, "bare.jsonl")
	if err := os.WriteFile(bare, []byte(`{"type":"summary"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := TranscriptStart(town, town, bare); err != nil || !got.IsZero() {
		t.Errorf("start of a transcript with no timestamp = %v, %v; want the zero time", got, err)
	}
}
