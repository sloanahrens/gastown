package daemon

import (
	"log"
	"strings"
	"testing"
)

// TestSchedulerLogWriterSplitsLines: in-process dispatch writes to a writer
// rather than to a child process the daemon reads, so each line of the
// scheduler's narration has to become one log line — including a line split
// across two writes, and a final line with no newline yet.
func TestSchedulerLogWriterSplitsLines(t *testing.T) {
	t.Parallel()
	var logged []string
	logger := log.New(writerFunc(func(p []byte) (int, error) {
		logged = append(logged, strings.TrimRight(string(p), "\n"))
		return len(p), nil
	}), "", 0)
	w := newSchedulerLogWriter(logger)

	if _, err := w.Write([]byte("Dispatched 2, failed 0")); err != nil {
		t.Fatal(err)
	}
	if len(logged) != 0 {
		t.Fatalf("an unterminated write was logged: %v", logged)
	}

	if _, err := w.Write([]byte(" (reason: ready)\n\nWarn")); err != nil {
		t.Fatal(err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "Scheduler dispatch: Dispatched 2, failed 0 (reason: ready)") {
		t.Fatalf("logged = %v, want one complete line carrying the whole message", logged)
	}

	// A blank line carries nothing and must not become a log entry.
	if _, err := w.Write([]byte("ing\n")); err != nil {
		t.Fatal(err)
	}
	if len(logged) != 2 || !strings.Contains(logged[1], "Scheduler dispatch: Warning") {
		t.Fatalf("logged = %v, want the second line to carry the continuation", logged)
	}
}
