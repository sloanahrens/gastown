package cmd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/outbox"
	"github.com/steveyegge/gastown/internal/taskfile"
)

// fileModeSeat is a seat in file mode: a temp .gt directory holding the task
// files, and an escalateRun whose every bead-facing seam records the call
// instead of answering it, so a test can show the command made none.
type fileModeSeat struct {
	r   escalateRun
	gt  string // the seat's .gt directory
	out *strings.Builder
	// beadCalls names each seam that reached for a bead, in call order.
	beadCalls []string
}

// newFileModeSeat stages a seat whose TASK.md is taskText and whose MODE marker
// makes it file mode.
func newFileModeSeat(t *testing.T, taskText string) *fileModeSeat {
	t.Helper()
	gt := filepath.Join(t.TempDir(), ".gt")
	writeSeatFile(t, filepath.Join(gt, "task", taskfile.ModeFile), taskfile.ModeFiles+"\n")
	writeSeatFile(t, filepath.Join(gt, "task", "TASK.md"), taskText)

	seat := &fileModeSeat{gt: gt, out: &strings.Builder{}}
	record := func(name string) { seat.beadCalls = append(seat.beadCalls, name) }
	seat.r = escalateRun{
		severity: "high",
		in:       strings.NewReader(""),
		out:      seat.out,
		errOut:   &strings.Builder{},
		gtDir:    func() string { return gt },
		townRoot: func() (string, error) {
			record("townRoot")
			return "", errors.New("file mode asked for the town root")
		},
		sender: func() string {
			record("sender")
			return "gastown/polecats/rictus"
		},
		hookedBead: func(string, string) string {
			record("hookedBead")
			return ""
		},
		raise: func(notify.EscalationRequest, *config.EscalationConfig) (*notify.RaiseResult, error) {
			record("raise")
			return nil, errors.New("file mode filed an escalation bead")
		},
		beadsClient: func(string) beads.Client {
			record("beadsClient")
			return nil
		},
	}
	return seat
}

// resultPath is where the seat's escalation request goes.
func (s *fileModeSeat) resultPath() string {
	return filepath.Join(s.gt, "outbox", "result.json")
}

// written is result.json's bytes; the test fails when the seat wrote none.
func (s *fileModeSeat) written(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(s.resultPath())
	if err != nil {
		t.Fatalf("the seat wrote no result.json: %v", err)
	}
	return raw
}

func writeSeatFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const fileModeTask = "# TASK gt-abc1: the seat's task\n\n## Description\n\nA task.\n"

// TestNoBeadAccessFileModeEscalateWritesTheOutboxRequest: a file-mode seat's
// escalation lands in result.json's escalation field and validates as the
// outbox the host applies, and no seam reached for a bead.
func TestNoBeadAccessFileModeEscalateWritesTheOutboxRequest(t *testing.T) {
	t.Parallel()
	seat := newFileModeSeat(t, fileModeTask)
	seat.r.reason = "the acceptance names a missing file"

	if err := seat.r.escalate([]string{"HELP:", "the", "build", "is", "missing", "a", "credential"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if len(seat.beadCalls) != 0 {
		t.Errorf("a file-mode escalation reached for a bead: %v", seat.beadCalls)
	}

	raw := seat.written(t)
	res, err := outbox.Validate(raw, nil, "gt-abc1")
	if err != nil {
		t.Fatalf("the written outbox does not validate: %v\n%s", err, raw)
	}
	if res.Status != outbox.StatusDeferred {
		t.Errorf("result.json status = %q, want %q: a seat that escalates has not finished", res.Status, outbox.StatusDeferred)
	}
	want := "HELP: the build is missing a credential\n\nSeverity: high\nReason: the acceptance names a missing file"
	if res.Escalation == nil || res.Escalation.Text != want {
		t.Errorf("escalation = %+v, want text %q", res.Escalation, want)
	}
	if !strings.Contains(seat.out.String(), seat.resultPath()) {
		t.Errorf("output does not name the outbox it wrote:\n%s", seat.out.String())
	}
}

// TestNoBeadAccessFileModeEscalateKeepsTheSeatsEarlierResult: the request is
// added to the result.json the seat already wrote, so its ticks and findings
// survive and the host still applies them.
func TestNoBeadAccessFileModeEscalateKeepsTheSeatsEarlierResult(t *testing.T) {
	t.Parallel()
	seat := newFileModeSeat(t, fileModeTask)
	writeSeatFile(t, seat.resultPath(), `{
  "bead_id": "gt-abc1",
  "status": "deferred",
  "ticked_boxes": [0, 2],
  "findings": ["the retry never resets its counter"],
  "escalation": null
}
`)

	if err := seat.r.escalate([]string{"HELP:", "blocked"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	res, err := outbox.Validate(seat.written(t), nil, "gt-abc1")
	if err != nil {
		t.Fatalf("the written outbox does not validate: %v", err)
	}
	if len(res.Ticked) != 2 || res.Ticked[0] != 0 || res.Ticked[1] != 2 {
		t.Errorf("ticked boxes = %v, want the seat's [0 2]", res.Ticked)
	}
	if len(res.Findings) != 1 || res.Findings[0] != "the retry never resets its counter" {
		t.Errorf("findings = %v, want the seat's one finding", res.Findings)
	}
	if res.Escalation == nil || !strings.HasPrefix(res.Escalation.Text, "HELP: blocked") {
		t.Errorf("escalation = %+v, want the new request", res.Escalation)
	}
}

// TestNoBeadAccessFileModeEscalateRefusesAnInvalidSeverity: the severity is
// checked before anything is written, so a refused severity leaves no outbox
// behind and reaches no bead.
func TestNoBeadAccessFileModeEscalateRefusesAnInvalidSeverity(t *testing.T) {
	t.Parallel()
	seat := newFileModeSeat(t, fileModeTask)
	seat.r.severity = "emergency"

	err := seat.r.escalate([]string{"HELP: the build is missing a credential"})
	if err == nil || !strings.Contains(err.Error(), "invalid severity") {
		t.Fatalf("err = %v, want the invalid severity refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(seat.gt, "outbox")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the outbox directory exists after a refused severity (stat err %v)", statErr)
	}
	if len(seat.beadCalls) != 0 {
		t.Errorf("a refused severity reached for a bead: %v", seat.beadCalls)
	}
}

// TestNoBeadAccessFileModeEscalateRefusesASecondRequest: a request the host has
// not raised yet is the seat's only one; a second is refused and the first is
// left as it was.
func TestNoBeadAccessFileModeEscalateRefusesASecondRequest(t *testing.T) {
	t.Parallel()
	seat := newFileModeSeat(t, fileModeTask)
	if err := seat.r.escalate([]string{"HELP: the first request"}); err != nil {
		t.Fatalf("first escalate: %v", err)
	}
	first := seat.written(t)

	err := seat.r.escalate([]string{"HELP: the second request"})
	if err == nil || !strings.Contains(err.Error(), "already carries the escalation request") {
		t.Fatalf("err = %v, want the second-request refusal", err)
	}
	if got := seat.written(t); string(got) != string(first) {
		t.Errorf("the refused request rewrote result.json:\nfirst:\n%s\nsecond:\n%s", first, got)
	}
	if len(seat.beadCalls) != 0 {
		t.Errorf("a refused request reached for a bead: %v", seat.beadCalls)
	}
}

// TestNoBeadAccessFileModeEscalateDryRunWritesNothing: --dry-run reports the
// request it would write and writes nothing, as the dolt-mode dry run files
// nothing.
func TestNoBeadAccessFileModeEscalateDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	seat := newFileModeSeat(t, fileModeTask)
	seat.r.dryRun = true

	if err := seat.r.escalate([]string{"HELP: the dry run request"}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(seat.gt, "outbox")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("a dry run wrote the outbox (stat err %v)", statErr)
	}
	if !strings.Contains(seat.out.String(), "Would write the escalation request") {
		t.Errorf("dry run output:\n%s", seat.out.String())
	}
}

// TestNoBeadAccessFileModeEscalateRefusesAnUnreadableSeat: what the seat's own
// files cannot answer is refused with the path named and nothing written,
// rather than written for the host to drop.
func TestNoBeadAccessFileModeEscalateRefusesAnUnreadableSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		corrupt func(t *testing.T, seat *fileModeSeat)
		wantErr string
	}{
		{
			name:    "no task file",
			corrupt: func(t *testing.T, seat *fileModeSeat) { deleteSeatFile(t, filepath.Join(seat.gt, "task", "TASK.md")) },
			wantErr: "reading the task file",
		},
		{
			name: "a task file with no heading",
			corrupt: func(t *testing.T, seat *fileModeSeat) {
				writeSeatFile(t, filepath.Join(seat.gt, "task", "TASK.md"), "## Description\n\nA task with no heading.\n")
			},
			wantErr: "does not start with",
		},
		{
			name: "a heading that names no bead",
			corrupt: func(t *testing.T, seat *fileModeSeat) {
				writeSeatFile(t, filepath.Join(seat.gt, "task", "TASK.md"), "# TASK: the seat's task\n")
			},
			wantErr: "names no bead id",
		},
		{
			name: "a result.json the decoder refuses",
			corrupt: func(t *testing.T, seat *fileModeSeat) {
				writeSeatFile(t, seat.resultPath(), `{"bead_id": "gt-abc1", "status": "in_progress"}`)
			},
			wantErr: "does not validate",
		},
		{
			name: "a notes.md over the decoder's cap",
			corrupt: func(t *testing.T, seat *fileModeSeat) {
				writeSeatFile(t, filepath.Join(seat.gt, "outbox", "notes.md"), strings.Repeat("n", 257<<10))
			},
			wantErr: "does not validate",
		},
		{
			name: "a result.json that cannot be read",
			corrupt: func(t *testing.T, seat *fileModeSeat) {
				if err := os.MkdirAll(seat.resultPath(), 0o755); err != nil {
					t.Fatalf("make a directory where result.json goes: %v", err)
				}
			},
			wantErr: "reading",
		},
		{
			name: "a notes.md that cannot be read",
			corrupt: func(t *testing.T, seat *fileModeSeat) {
				if err := os.MkdirAll(filepath.Join(seat.gt, "outbox", "notes.md"), 0o755); err != nil {
					t.Fatalf("make a directory where notes.md goes: %v", err)
				}
			},
			wantErr: "reading",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			seat := newFileModeSeat(t, fileModeTask)
			tt.corrupt(t, seat)

			err := seat.r.escalate([]string{"HELP: the request"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to name %q", err, tt.wantErr)
			}
			if len(seat.beadCalls) != 0 {
				t.Errorf("a refused request reached for a bead: %v", seat.beadCalls)
			}
			// Nothing was written over what the seat had: whatever sits at
			// result.json is the row's own file, not a request.
			if raw, readErr := os.ReadFile(seat.resultPath()); readErr == nil && strings.Contains(string(raw), "the request") {
				t.Errorf("the refused request was written to result.json:\n%s", raw)
			}
		})
	}
}

// TestNoBeadAccessFileModeEscalateRefusesAFailedOutboxWrite: an outbox that
// cannot be created or written fails the command with the cause named, rather
// than exiting 0 and leaving the seat to believe a request is pending.
func TestNoBeadAccessFileModeEscalateRefusesAFailedOutboxWrite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   seatFiles
		wantErr string
	}{
		{
			name:    "the outbox cannot be created",
			files:   seatFiles{mkdirAll: func(string, fs.FileMode) error { return errors.New("the disk is read-only") }},
			wantErr: "creating the outbox",
		},
		{
			name:    "the request cannot be written",
			files:   seatFiles{writeFile: func(string, []byte, fs.FileMode) error { return errors.New("the disk is full") }},
			wantErr: "writing the outbox",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			seat := newFileModeSeat(t, fileModeTask)
			seat.r.files = tt.files

			err := seat.r.escalate([]string{"HELP: the request"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to name %q", err, tt.wantErr)
			}
			if len(seat.beadCalls) != 0 {
				t.Errorf("a failed write reached for a bead: %v", seat.beadCalls)
			}
		})
	}
}

func deleteSeatFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}
