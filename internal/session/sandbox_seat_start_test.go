package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSeatLedger writes the seat-end ledger of town: each line as a removal
// would have appended it, then raw text after them.
func writeSeatLedger(t *testing.T, town string, lines []sandboxSeatEndLine, raw string) {
	t.Helper()
	path := SandboxSeatLedgerPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating the ledger directory: %v", err)
	}
	var text string
	for _, line := range lines {
		data, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("encoding a seat-end line: %v", err)
		}
		text += string(data) + "\n"
	}
	text += raw
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("writing the ledger: %v", err)
	}
}

// TestSandboxSeatStartFromLedgerMatchesTheRecordAliveAtSubmission: the ledger
// line of the seat's own record is the start a removal handed on. A line that
// ended before the submission is a predecessor's, a line created after it is a
// later seat's, and another session's line is nobody's here.
func TestSandboxSeatStartFromLedgerMatchesTheRecordAliveAtSubmission(t *testing.T) {
	t.Parallel()
	const name = "gt-gastown-box"
	created := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	submitted := created.Add(time.Hour)
	tests := []struct {
		name      string
		lines     []sandboxSeatEndLine
		raw       string
		submitted time.Time
		wantStart time.Time
		wantOK    bool
		wantErr   bool
	}{
		{
			name:      "the line alive at the submission gives its start",
			lines:     []sandboxSeatEndLine{{Session: name, CreatedAt: created, EndedAt: created.Add(2 * time.Hour)}},
			submitted: submitted,
			wantStart: created, wantOK: true,
		},
		{
			name:      "a line that ended before the submission is a predecessor's",
			lines:     []sandboxSeatEndLine{{Session: name, CreatedAt: created.Add(-2 * time.Hour), EndedAt: created.Add(30 * time.Minute)}},
			submitted: submitted,
		},
		{
			name:      "a line created after the submission is a later seat's",
			lines:     []sandboxSeatEndLine{{Session: name, CreatedAt: created.Add(2 * time.Hour), EndedAt: created.Add(3 * time.Hour)}},
			submitted: submitted,
		},
		{
			name:      "another session's line is not this seat's",
			lines:     []sandboxSeatEndLine{{Session: "gt-gastown-other", CreatedAt: created, EndedAt: created.Add(2 * time.Hour)}},
			submitted: submitted,
		},
		{
			name:      "two lines naming the same record give its one start",
			lines:     []sandboxSeatEndLine{{Session: name, CreatedAt: created, EndedAt: created.Add(2 * time.Hour)}, {Session: name, CreatedAt: created, EndedAt: created.Add(2 * time.Hour)}},
			submitted: submitted,
			wantStart: created, wantOK: true,
		},
		{
			name:      "two records alive at the submission are refused, not guessed",
			lines:     []sandboxSeatEndLine{{Session: name, CreatedAt: created, EndedAt: created.Add(2 * time.Hour)}, {Session: name, CreatedAt: created.Add(30 * time.Minute), EndedAt: created.Add(2 * time.Hour)}},
			submitted: submitted,
			wantErr:   true,
		},
		{
			name:      "a line that does not parse is an error, never an absent record",
			raw:       "not json\n",
			submitted: submitted,
			wantErr:   true,
		},
		{
			name:      "a submission with no stamp is refused",
			lines:     []sandboxSeatEndLine{{Session: name, CreatedAt: created, EndedAt: created.Add(2 * time.Hour)}},
			submitted: time.Time{},
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeSeatLedger(t, town, tt.lines, tt.raw)
			start, ok, err := SandboxSeatStartFromLedger(town, name, tt.submitted)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want an error: %v", err, tt.wantErr)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !start.Equal(tt.wantStart) {
				t.Errorf("start = %v, want %v", start, tt.wantStart)
			}
		})
	}
}

// TestSandboxSeatStartFromLedgerNoLedgerIsNoRecord: a town whose ledger was never
// written has no record of any seat to hand on.
func TestSandboxSeatStartFromLedgerNoLedgerIsNoRecord(t *testing.T) {
	t.Parallel()
	_, ok, err := SandboxSeatStartFromLedger(t.TempDir(), "gt-gastown-box", time.Now())
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v, want no record and no error", ok, err)
	}
}

// TestSandboxSeatStartSurvivesTheRemoval: the removal every teardown runs writes
// the line the start is read from, so the start of a record gone since the seat
// submitted comes back exactly.
func TestSandboxSeatStartSurvivesTheRemoval(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	const name = "gt-gastown-box"
	created := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	rec := SandboxRecord{Session: name, Rig: "gastown", Polecat: "box", Profile: "polecat-default", CreatedAt: created}
	if err := WriteSandboxRecord(town, rec); err != nil {
		t.Fatalf("writing the record: %v", err)
	}
	submitted := time.Now().Add(-time.Minute)
	if err := RemoveSandboxRecord(town, name); err != nil {
		t.Fatalf("removing the record: %v", err)
	}
	start, ok, err := SandboxSeatStartFromLedger(town, name, submitted)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want the removed record's start", ok, err)
	}
	if !start.Equal(created) {
		t.Errorf("start = %v, want the record's creation stamp %v", start, created)
	}
}
