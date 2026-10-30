package land

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

// appendRawLine adds one line to the events log, creating it when absent.
func appendRawLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening events log: %v", err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		_ = f.Close()
		t.Fatalf("appending to events log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing events log: %v", err)
	}
}

// appendEventFor writes the done event a seat logs for itself: its ts is when
// the seat wrote it, and it carries no at, because the seat's own gt done
// leaves the event time alone.
func appendEventFor(t *testing.T, path, actor, bead, branch string, at time.Time) {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"ts":      at.UTC().Format(time.RFC3339),
		"source":  "gt",
		"type":    events.TypeDone,
		"actor":   actor,
		"payload": map[string]any{"bead": bead, "branch": branch},
	})
	if err != nil {
		t.Fatalf("encoding event: %v", err)
	}
	appendRawLine(t, path, string(line))
}

// doneWork is a sandbox seat's submission: everything the seat writes into the
// bead's notes, as the landing worker reads it.
func doneWork() Work {
	return Work{
		BeadID: "gt-sbox1", Rig: "gastown", Branch: "polecat/opal/gt-sbox1",
		Target: "main", Worker: "opal", Submitted: recordAt.Add(-time.Minute),
	}
}

// slungTo answers the assignee the host's sling wrote for the work's seat.
func slungTo(w Work) func(string) (string, error) {
	return func(string) (string, error) { return w.Rig + "/polecats/" + w.Worker, nil }
}

func newDoneRecorder(t *testing.T, w Work) (*DoneEventRecorder, string) {
	t.Helper()
	townRoot := t.TempDir()
	return &DoneEventRecorder{
		TownRoot: townRoot,
		Assignee: slungTo(w),
		Now:      func() time.Time { return recordAt },
	}, filepath.Join(townRoot, events.EventsFile)
}

func readEventLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading events log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func decodeDoneEvent(t *testing.T, line string) (actor string, payload map[string]any) {
	t.Helper()
	var ev struct {
		Type    string         `json:"type"`
		Actor   string         `json:"actor"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("decoding event %q: %v", line, err)
	}
	if ev.Type != events.TypeDone {
		t.Fatalf("event type = %q, want %q", ev.Type, events.TypeDone)
	}
	return ev.Actor, ev.Payload
}

// TestDoneEventRecorderWritesTheSandboxSubmission: the host writes the done
// event a sandbox seat could not, in the town's own log, with the seat's own
// stamp as the time its turn ended.
func TestDoneEventRecorderWritesTheSandboxSubmission(t *testing.T) {
	t.Parallel()
	w := doneWork()
	rec, path := newDoneRecorder(t, w)

	wrote, err := rec.Record(w)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !wrote {
		t.Fatal("Record = false; want the sandbox submission's done event written")
	}
	lines := readEventLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("events = %d lines, want 1", len(lines))
	}
	actor, payload := decodeDoneEvent(t, lines[0])
	if want := w.Rig + "/polecats/" + w.Worker; actor != want {
		t.Errorf("actor = %q, want %q", actor, want)
	}
	if got, _ := payload["bead"].(string); got != w.BeadID {
		t.Errorf("payload bead = %q, want %q", got, w.BeadID)
	}
	if got, _ := payload["branch"].(string); got != w.Branch {
		t.Errorf("payload branch = %q, want %q", got, w.Branch)
	}
	if got, _ := payload["at"].(string); got != w.Submitted.UTC().Format(time.RFC3339) {
		t.Errorf("payload at = %q, want the seat's stamp %q", got, w.Submitted.UTC().Format(time.RFC3339))
	}
}

// The per-pass call writes one event per submission, and a done event the seat
// wrote itself just before its note (a host seat) means the host writes none.
func TestDoneEventRecorderWritesOnceAndSkipsAHostSeatsOwnEvent(t *testing.T) {
	t.Parallel()
	t.Run("twice", func(t *testing.T) {
		t.Parallel()
		w := doneWork()
		rec, path := newDoneRecorder(t, w)
		if _, err := rec.Record(w); err != nil {
			t.Fatalf("first Record: %v", err)
		}
		wrote, err := rec.Record(w)
		if err != nil {
			t.Fatalf("second Record: %v", err)
		}
		if wrote {
			t.Fatal("second Record = true; the same submission wrote twice")
		}
		if lines := readEventLines(t, path); len(lines) != 1 {
			t.Fatalf("events = %d lines after two Record calls, want 1", len(lines))
		}
	})
	t.Run("host seat", func(t *testing.T) {
		t.Parallel()
		w := doneWork()
		rec, path := newDoneRecorder(t, w)
		own := recordAt.Add(-2 * time.Minute) // the seat's own event, inside the window
		appendEventFor(t, path, "gastown/polecats/opal", w.BeadID, w.Branch, own)

		wrote, err := rec.Record(w)
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		if wrote {
			t.Fatal("Record = true for a bead whose seat already wrote a done event; want no duplicate")
		}
		if lines := readEventLines(t, path); len(lines) != 1 {
			t.Fatalf("events = %d lines, want only the seat's own", len(lines))
		}
	})
}

// The seat's stamp may pull the end no later than the host's first sight, and
// a crew submission, which has no seat to end, writes nothing at all.
func TestDoneEventRecorderCapsTheStampAndSkipsCrew(t *testing.T) {
	t.Parallel()
	t.Run("future stamp", func(t *testing.T) {
		t.Parallel()
		w := doneWork()
		w.Submitted = recordAt.Add(time.Hour) // the seat's clock ran ahead
		rec, path := newDoneRecorder(t, w)
		if _, err := rec.Record(w); err != nil {
			t.Fatalf("Record: %v", err)
		}
		lines := readEventLines(t, path)
		if len(lines) != 1 {
			t.Fatalf("events = %d lines, want 1", len(lines))
		}
		_, payload := decodeDoneEvent(t, lines[0])
		if got, want := payload["at"], recordAt.UTC().Format(time.RFC3339); got != want {
			t.Errorf("payload at = %v, want the host's first sight %q", got, want)
		}
	})
	t.Run("crew", func(t *testing.T) {
		t.Parallel()
		w := doneWork()
		w.Worker = ""
		called := false
		rec := &DoneEventRecorder{
			TownRoot: t.TempDir(),
			Assignee: func(string) (string, error) { called = true; return "", nil },
			Now:      func() time.Time { return recordAt },
		}
		wrote, err := rec.Record(w)
		if err != nil || wrote {
			t.Fatalf("Record = %v, %v for a crew submission; want false, nil", wrote, err)
		}
		if called {
			t.Fatal("the assignee was read for a crew submission; a crew Work must not reach the seat checks")
		}
	})
}

// A seat may name any Worker in its READY TO LAND block, so the host writes
// the event only for the seat it actually slung: a block naming another
// polecat is refused with nothing appended.
func TestDoneEventRecorderRefusesASeatTheHostDidNotSling(t *testing.T) {
	t.Parallel()
	w := doneWork()
	rec, path := newDoneRecorder(t, w)
	rec.Assignee = func(string) (string, error) { return "gastown/polecats/other", nil }

	wrote, err := rec.Record(w)
	if err == nil {
		t.Fatal("Record = nil error when the submission names a seat the bead was not slung to")
	}
	if wrote {
		t.Fatal("Record = true for a forged seat")
	}
	if lines := readEventLines(t, path); len(lines) != 0 {
		t.Fatalf("events = %d lines, want none for a forged seat", len(lines))
	}
}

// A seat name that would restyle the actor is refused before any log read.
func TestDoneEventRecorderRefusesAMalformedSeat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, worker string }{
		{"separator", "opal/../../mayor"},
		{"space", "opal caretaker"},
		{"empty rig", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := doneWork()
			w.Worker = tc.worker
			if tc.name == "empty rig" {
				w.Rig = ""
				w.Worker = "opal"
			}
			rec, path := newDoneRecorder(t, w)
			if _, err := rec.Record(w); err == nil {
				t.Fatalf("Record = nil error for worker %q; want it refused", tc.worker)
			}
			if lines := readEventLines(t, path); len(lines) != 0 {
				t.Fatalf("events = %d lines, want none for a malformed seat", len(lines))
			}
		})
	}
}

// A done event for the bead whose time cannot be read leaves the bead
// unjudged, and the recorder fails closed rather than risk a duplicate.
func TestDoneEventRecorderRefusesAnUnreadableDoneEventTime(t *testing.T) {
	t.Parallel()
	w := doneWork()
	rec, path := newDoneRecorder(t, w)
	appendRawLine(t, path, `{"ts":"","type":"done","actor":"gastown/polecats/opal","payload":{"bead":"`+w.BeadID+`"}}`)

	wrote, err := rec.Record(w)
	if err == nil {
		t.Fatal("Record = nil error over a done event with no readable time; want it refused")
	}
	if wrote {
		t.Fatal("Record = true over a done event with no readable time")
	}
	if lines := readEventLines(t, path); len(lines) != 1 {
		t.Fatalf("events = %d lines, want only the unreadable one", len(lines))
	}
}

// A host seat's own event written between two passes is seen by the next one:
// the reader follows the log's tail rather than a snapshot taken at start.
func TestDoneEventRecorderSeesAHostSeatsEventWrittenAfterTheFirstRead(t *testing.T) {
	t.Parallel()
	first := doneWork()
	rec, path := newDoneRecorder(t, first)
	if _, err := rec.Record(first); err != nil {
		t.Fatalf("first Record: %v", err)
	}

	second := doneWork()
	second.BeadID = "gt-sbox2"
	second.Branch = "polecat/opal/gt-sbox2"
	second.Worker = first.Worker
	rec.Assignee = slungTo(second)
	appendEventFor(t, path, "gastown/polecats/opal", second.BeadID, second.Branch, recordAt.Add(-2*time.Minute))

	wrote, err := rec.Record(second)
	if err != nil {
		t.Fatalf("second Record: %v", err)
	}
	if wrote {
		t.Fatal("Record = true for a bead whose seat's event was appended after the first read; the reader kept a stale snapshot")
	}
}

// A log replaced by a shorter one (the prune renames its replacement into
// place) is read again from its start, so the bead's record in the new file
// still stops the write.
func TestDoneEventRecorderReadsALogReplacedUnderIt(t *testing.T) {
	t.Parallel()
	w := doneWork()
	rec, path := newDoneRecorder(t, w)
	if _, err := rec.Record(w); err != nil {
		t.Fatalf("Record: %v", err)
	}

	replacement := filepath.Join(filepath.Dir(path), "replacement.jsonl")
	appendEventFor(t, replacement, "gastown/polecats/opal", w.BeadID, w.Branch, recordAt.Add(-time.Minute))
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replacing the events log: %v", err)
	}

	wrote, err := rec.Record(w)
	if err != nil {
		t.Fatalf("Record over a replaced log: %v", err)
	}
	if wrote {
		t.Fatal("Record = true over a replaced log that already holds the bead's done event")
	}
}

// A failed write is the error the worker logs; nothing is recorded as written.
func TestDoneEventRecorderReturnsAFailedWrite(t *testing.T) {
	t.Parallel()
	w := doneWork()
	rec, _ := newDoneRecorder(t, w)
	boom := errors.New("log is read-only")
	rec.Append = func(string, string, string, map[string]interface{}) error { return boom }

	if _, err := rec.Record(w); !errors.Is(err, boom) {
		t.Fatalf("Record error = %v, want the append failure", err)
	}
}
