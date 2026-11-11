package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/usageattrib"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// writeSeatRecordAt writes the sandbox record the seat's registry keeps for the
// seat, created at start.
func (f *seatEndFake) writeSeatRecordAt(start time.Time) {
	f.t.Helper()
	rec := session.SandboxRecord{
		Session: f.host.sessionName(endRig, endPolecat),
		Rig:     endRig,
		Polecat: endPolecat,
		Gateway: "gw1",
		Profile: "polecat-default",
		Image:   "gastown-polecat:dev",
		// The seat's start is the stamp the usage record counts from.
		CreatedAt: start,
	}
	if err := session.WriteSandboxRecord(f.town, rec); err != nil {
		f.t.Fatal(err)
	}
}

// seatCallLine is one assistant call in a transcript: its tokens, stamped inside
// the seat's window.
func seatCallLine(id string, input, cacheRead, output int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-11-10T11:00:00Z","message":{"id":"msg_%s","model":"claude-sonnet-5-5","usage":{"input_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":%d,"output_tokens":%d}}}`+"\n", id, input, cacheRead, output)
}

// writeUsageTranscript writes a transcript holding one assistant call with the
// given tokens, last modified at modified.
func writeUsageTranscript(t *testing.T, path string, modified time.Time, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

// usageCall is one call the usage writer was given.
type usageCall struct {
	usageDir string
	rec      dashboard.UsageRecord
	ids      []string
}

// TestSeatEndWritesUsageBeforeRemoval is the seat's usage record: it sums the
// transcripts the seat's own sessions wrote, leaves out a predecessor's, carries
// the seat's facts, and is written before the seat's directories go.
func TestSeatEndWritesUsageBeforeRemoval(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	f.host.d.clock = clockwork.NewFakeClockAt(end)
	f.writeSeatRecordAt(start)

	// A reused worktree keeps its predecessor's transcript, last written before
	// the seat started. The seat's own session and the subagent it started count.
	projectDir := filepath.Join(f.town, "transcripts")
	predecessor := filepath.Join(projectDir, "old-session.jsonl")
	seatSession := filepath.Join(projectDir, "new-session.jsonl")
	subagent := filepath.Join(projectDir, "new-session", "subagents", "agent-a.jsonl")
	writeUsageTranscript(t, predecessor, start.Add(-time.Hour), seatCallLine("old", 1000, 0, 1000))
	writeUsageTranscript(t, seatSession, start.Add(30*time.Minute), seatCallLine("new", 100, 50, 20))
	writeUsageTranscript(t, subagent, start.Add(40*time.Minute), seatCallLine("sub", 10, 0, 5))

	var events []string
	var calls []usageCall
	var listedFor string
	f.host.seatUsage = seatUsageHook{
		list: func(_, workDir string) ([]string, error) {
			listedFor = workDir
			return []string{predecessor, seatSession}, nil
		},
		write: func(usageDir string, rec dashboard.UsageRecord, ids []string) error {
			events = append(events, "usage")
			calls = append(calls, usageCall{usageDir: usageDir, rec: rec, ids: ids})
			return nil
		},
	}
	removeDirs := f.host.removeSandboxSeatDirs
	f.host.removeSandboxSeatDirs = func(rigPath, rig, name string) (bool, error) {
		events = append(events, "remove")
		return removeDirs(rigPath, rig, name)
	}

	finished := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, finished) {
		t.Fatal("the finished seat was not removed")
	}
	if listedFor != dirs.ClonePath {
		t.Errorf("transcripts listed for %q, want the seat's worktree %q", listedFor, dirs.ClonePath)
	}
	if got := strings.Join(events, ","); got != "usage,remove" {
		t.Fatalf("events = %q, want the usage record written before the removal", got)
	}
	if len(calls) != 1 {
		t.Fatalf("usage writes = %d, want 1", len(calls))
	}
	c := calls[0]
	if want := filepath.Join(f.town, ".runtime", "usage"); c.usageDir != want {
		t.Errorf("usage dir = %q, want %q", c.usageDir, want)
	}
	if len(c.ids) != 1 || c.ids[0] != "new-session" {
		t.Errorf("session ids = %q, want only the seat's own session", c.ids)
	}
	rec := c.rec
	if rec.Bead != endBead || rec.Rig != endRig || rec.Kind != dashboard.UsageKindWork || rec.Class != dashboard.UsageClassPolecat {
		t.Errorf("record names bead %q rig %q kind %q class %q, want %q %q %q %q",
			rec.Bead, rec.Rig, rec.Kind, rec.Class, endBead, endRig, dashboard.UsageKindWork, dashboard.UsageClassPolecat)
	}
	if rec.Seat == nil || *rec.Seat != endPolecat {
		t.Errorf("record seat = %v, want %q", rec.Seat, endPolecat)
	}
	if want, err := usageattrib.WorkspaceOf(f.town, endBead); err != nil || rec.Workspace != want {
		t.Errorf("record workspace = %q (%v), want %q", rec.Workspace, err, want)
	}
	if !rec.Start.Equal(start) || !rec.End.Equal(end) {
		t.Errorf("record window = %v to %v, want %v to %v", rec.Start, rec.End, start, end)
	}
	if !rec.Complete {
		t.Error("record is incomplete, want complete: the seat ended after its landing")
	}
	// One main call and one subagent call; the predecessor's 1000 input tokens
	// are not among them.
	if rec.Totals.Calls != 2 || rec.Totals.Input != 110 || rec.Totals.CacheRead != 50 || rec.Totals.Output != 25 {
		t.Errorf("record totals = %+v, want 2 calls, 110 input, 50 cache read, 25 output", rec.Totals)
	}
	if rec.Subagents.Calls != 1 {
		t.Errorf("record subagent calls = %d, want 1", rec.Subagents.Calls)
	}
	if dirExists(t, dirs.PolecatDir) {
		t.Error("the seat's directories are still there after the removal")
	}
}

// TestSeatEndUsageFailureNeverBlocks: a usage record that cannot be read or
// written is logged once, and the seat is removed all the same.
func TestSeatEndUsageFailureNeverBlocks(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		// noRecord leaves the seat's sandbox record gone, so its start is unknown.
		noRecord bool
		// files are the transcripts the lister names, under the town.
		files func(t *testing.T, town string) []string
		// listErr fails the listing.
		listErr error
		// writeErr fails the usage writer.
		writeErr error
		// badWorkspace replaces the town's workspace map with one that does not parse.
		badWorkspace bool
		// corruptRecord leaves the seat's sandbox record unreadable.
		corruptRecord bool
		wantWrites    int
		wantLogs      int
	}{
		{
			name:  "a seat with no transcript writes no record and logs nothing",
			files: func(*testing.T, string) []string { return nil },
		},
		{
			name: "a transcript with no session id is logged once and writes nothing",
			files: func(_ *testing.T, town string) []string {
				return []string{filepath.Join(town, "transcripts", ".jsonl")}
			},
			wantWrites: 0, wantLogs: 1,
		},
		{
			name:          "an unreadable sandbox record is logged once and writes nothing",
			corruptRecord: true,
			files: func(t *testing.T, town string) []string {
				path := filepath.Join(town, "transcripts", "seat.jsonl")
				writeUsageTranscript(t, path, start.Add(time.Hour), seatCallLine("ok", 1, 0, 1))
				return []string{path}
			},
			wantWrites: 0, wantLogs: 1,
		},
		{
			name:          "an unreadable sandbox record with no transcript is logged once and writes nothing",
			corruptRecord: true,
			files:         func(*testing.T, string) []string { return nil },
			wantWrites:    0, wantLogs: 1,
		},
		{
			name: "a missing transcript is logged once and its record is incomplete",
			files: func(_ *testing.T, town string) []string {
				return []string{filepath.Join(town, "transcripts", "gone.jsonl")}
			},
			wantWrites: 1, wantLogs: 1,
		},
		{
			name: "an unreadable transcript is logged once and its record is incomplete",
			files: func(t *testing.T, town string) []string {
				bad := filepath.Join(town, "transcripts", "bad.jsonl")
				if err := os.MkdirAll(bad, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(bad, start.Add(time.Hour), start.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				return []string{bad}
			},
			wantWrites: 1, wantLogs: 1,
		},
		{
			name: "a transcript line that does not parse is logged once",
			files: func(t *testing.T, town string) []string {
				path := filepath.Join(town, "transcripts", "torn.jsonl")
				writeUsageTranscript(t, path, start.Add(time.Hour), "not json\n"+seatCallLine("ok", 1, 0, 1))
				return []string{path}
			},
			wantWrites: 1, wantLogs: 1,
		},
		{
			name:       "a listing that fails is logged once and writes nothing",
			files:      func(*testing.T, string) []string { return nil },
			listErr:    errors.New("project directory unreadable"),
			wantWrites: 0, wantLogs: 1,
		},
		{
			name: "a failing write is logged once",
			files: func(t *testing.T, town string) []string {
				path := filepath.Join(town, "transcripts", "seat.jsonl")
				writeUsageTranscript(t, path, start.Add(time.Hour), seatCallLine("ok", 1, 0, 1))
				return []string{path}
			},
			writeErr:   errors.New("usage tree read-only"),
			wantWrites: 1, wantLogs: 1,
		},
		{
			name:     "a seat whose sandbox record is gone is logged once and writes nothing",
			noRecord: true,
			files: func(t *testing.T, town string) []string {
				path := filepath.Join(town, "transcripts", "seat.jsonl")
				writeUsageTranscript(t, path, start.Add(time.Hour), seatCallLine("ok", 1, 0, 1))
				return []string{path}
			},
			wantWrites: 0, wantLogs: 1,
		},
		{
			name: "a town whose workspace map does not parse is logged once and writes nothing",
			files: func(t *testing.T, town string) []string {
				path := filepath.Join(town, "transcripts", "seat.jsonl")
				writeUsageTranscript(t, path, start.Add(time.Hour), seatCallLine("ok", 1, 0, 1))
				return []string{path}
			},
			badWorkspace: true,
			wantWrites:   0, wantLogs: 1,
		},
		{
			name: "a readable seat writes its record and logs nothing",
			files: func(t *testing.T, town string) []string {
				path := filepath.Join(town, "transcripts", "seat.jsonl")
				writeUsageTranscript(t, path, start.Add(time.Hour), seatCallLine("ok", 1, 0, 1))
				return []string{path}
			},
			wantWrites: 1, wantLogs: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSeatEndFake(t)
			f.seedAgent()
			f.retire(intent.DesiredStop)
			dirs := f.seatDirs()
			f.host.d.clock = clockwork.NewFakeClockAt(end)
			if !tt.noRecord {
				f.writeSeatRecordAt(start)
			}
			if tt.corruptRecord {
				record := filepath.Join(session.SandboxRegistryDir(f.town), f.host.sessionName(endRig, endPolecat)+".json")
				writeUsageTranscript(t, record, start, "{not json")
			}
			if tt.badWorkspace {
				writeTownFile(t, f.town, workspaceid.File, "{not json")
			}
			var logs bytes.Buffer
			f.host.d.logger = log.New(&logs, "", 0)
			writes := 0
			f.host.seatUsage = seatUsageHook{
				list: func(_, _ string) ([]string, error) {
					if tt.listErr != nil {
						return nil, tt.listErr
					}
					return tt.files(t, f.town), nil
				},
				write: func(string, dashboard.UsageRecord, []string) error {
					writes++
					return tt.writeErr
				},
			}

			finished := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
			if !f.host.endFinishedSandboxSeat(endRig, endPolecat, finished) {
				t.Fatal("a usage failure kept the seat")
			}
			if dirExists(t, dirs.PolecatDir) {
				t.Error("the seat's directories are still there after the removal")
			}
			if writes != tt.wantWrites {
				t.Errorf("usage writes = %d, want %d", writes, tt.wantWrites)
			}
			if got := strings.Count(logs.String(), "usage record is not written"); got != tt.wantLogs {
				t.Errorf("usage failures logged %d times, want %d; log:\n%s", got, tt.wantLogs, logs.String())
			}
		})
	}
}

// TestSeatEndPendingRulesWritesAtRealRemoval: a seat kept for the rules ask
// writes nothing at the deferral, one record at its real removal, and a second
// removal attempt for the same session leaves that record as it was.
func TestSeatEndPendingRulesWritesAtRealRemoval(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	start := now.Add(-2 * time.Hour)
	f, _, landing := seatEndWaitingForRules(t, now)
	usageDir := filepath.Join(f.town, ".runtime", "usage")
	record := filepath.Join(usageDir, "beads", endBead+".work.json")
	if _, err := os.Stat(usageDir); err == nil {
		t.Fatal("the deferral wrote a usage tree")
	}

	// The seat's record and transcript exist from here on, and the real usage
	// writer runs against the town's usage tree.
	f.writeSeatRecordAt(start)
	seat := filepath.Join(f.town, "transcripts", "seat.jsonl")
	writeUsageTranscript(t, seat, now.Add(-time.Hour), seatCallLine("ok", 100, 0, 20))
	f.host.seatUsage.list = func(_, _ string) ([]string, error) { return []string{seat}, nil }

	// The rules ask has happened: the cursor is past the submission, so the seat
	// goes at this pass.
	f.cursorAt = landing.Submitted.Add(time.Minute)
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landing) {
		t.Fatal("the seat was not removed once its rules ask had happened")
	}
	first, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("no usage record at the removal: %v", err)
	}

	f.host.endFinishedSandboxSeat(endRig, endPolecat, landing)
	second, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("usage record gone after a second removal attempt: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("a second removal attempt changed the usage record:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestSandboxSeatEndUsageReadsTheStartBeforeAnErroredSandboxGoes: an errored
// sandbox's removal deletes the record that names the seat's start, so the
// start is read before it, and the usage record still counts from it.
func TestSandboxSeatEndUsageReadsTheStartBeforeAnErroredSandboxGoes(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	f.host.d.clock = clockwork.NewFakeClockAt(end)
	f.writeSeatRecordAt(start)
	// The sandbox still exists at the gateway, in a terminal failure phase.
	f.host.sandboxExists = func(string, string) (bool, error) { return true, nil }
	f.host.sandboxStatus = func(_, name string) (sandbox.Status, error) {
		return sandbox.Status{Name: name, Phase: "Error"}, nil
	}
	seat := filepath.Join(f.town, "transcripts", "seat.jsonl")
	writeUsageTranscript(t, seat, start.Add(time.Hour), seatCallLine("ok", 100, 0, 20))
	f.host.seatUsage.list = func(_, _ string) ([]string, error) { return []string{seat}, nil }
	var calls []usageCall
	f.host.seatUsage.write = func(usageDir string, rec dashboard.UsageRecord, ids []string) error {
		calls = append(calls, usageCall{usageDir: usageDir, rec: rec, ids: ids})
		return nil
	}

	finished := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, finished) {
		t.Fatal("the finished seat with an errored sandbox was not removed")
	}
	if len(f.deleted) != 1 {
		t.Fatalf("errored sandbox deletions = %d, want 1", len(f.deleted))
	}
	if len(calls) != 1 {
		t.Fatalf("usage writes = %d, want 1: the start was read after the record went", len(calls))
	}
	if !calls[0].rec.Start.Equal(start) {
		t.Errorf("record start = %v, want the seat's start %v", calls[0].rec.Start, start)
	}
	if dirExists(t, dirs.PolecatDir) {
		t.Error("the seat's directories are still there after the removal")
	}
}

// TestSeatEndUsageUnlistedSeamRefuses: a seat end with no transcript lister wired
// reads no transcripts from the host's Claude config and home, writes no usage
// record, logs the refusal once, and still removes the seat.
func TestSeatEndUsageUnlistedSeamRefuses(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	f.host.d.clock = clockwork.NewFakeClockAt(end)
	f.writeSeatRecordAt(start)
	var logs bytes.Buffer
	f.host.d.logger = log.New(&logs, "", 0)
	writes := 0
	f.host.seatUsage.write = func(string, dashboard.UsageRecord, []string) error {
		writes++
		return nil
	}

	finished := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, finished) {
		t.Fatal("a seat with no transcript lister wired was not removed")
	}
	if writes != 0 {
		t.Errorf("usage writes = %d, want 0", writes)
	}
	if got := strings.Count(logs.String(), "usage record is not written"); got != 1 {
		t.Errorf("usage failures logged %d times, want 1; log:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), errSeatUsageUnlisted.Error()) {
		t.Errorf("log does not name the refusal %q:\n%s", errSeatUsageUnlisted, logs.String())
	}
	if dirExists(t, dirs.PolecatDir) {
		t.Error("the seat's directories are still there after the removal")
	}
}
