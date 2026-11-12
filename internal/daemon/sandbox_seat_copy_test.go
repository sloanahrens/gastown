package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

// TestSeatEndReadsTheSandboxCopyAndWritesTheWorkRecord: a sandbox seat's
// transcripts are the copy taken from its sandbox before the delete, under the
// sandbox's project folder name. The seat end reads that copy, not the host's
// Claude directories, and writes the seat's work record from it.
func TestSeatEndReadsTheSandboxCopyAndWritesTheWorkRecord(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.host.d.clock = clockwork.NewFakeClockAt(end)
	f.writeSeatRecordAt(start)

	copied := filepath.Join(session.SandboxTranscriptCopyDir(f.town, endRig, endPolecat),
		session.SandboxTranscriptFolder(endRig, endPolecat), "sandbox-session.jsonl")
	writeUsageTranscript(t, copied, start.Add(time.Hour), seatCallLine("sandbox", 100, 50, 20))

	hostListed := false
	f.host.seatUsage.list = func(_, _ string) ([]string, error) {
		hostListed = true
		return nil, nil
	}

	finished := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, finished) {
		t.Fatal("the finished seat was not removed")
	}
	if hostListed {
		t.Error("the host's Claude directories were read although the sandbox copy holds the seat's transcript")
	}
	record := usageattrib.WorkRecordPath(filepath.Join(f.town, ".runtime", "usage"), endBead)
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("no work record written from the sandbox copy: %v", err)
	}
}

// TestSeatEndRefusesACopyItCannotListAndStillRemovesTheSeat: a copy that cannot
// be read is named as the refusal, and the host's directories are not read in its
// place. The seat is still removed.
func TestSeatEndRefusesACopyItCannotListAndStillRemovesTheSeat(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	f.host.d.clock = clockwork.NewFakeClockAt(end)
	f.writeSeatRecordAt(start)

	// The seat's folder in the copy is a file, so the copy cannot be listed.
	copyDir := session.SandboxTranscriptCopyDir(f.town, endRig, endPolecat)
	if err := os.MkdirAll(copyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, session.SandboxTranscriptFolder(endRig, endPolecat)), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostListed := false
	f.host.seatUsage.list = func(_, _ string) ([]string, error) {
		hostListed = true
		return nil, nil
	}
	var logs bytes.Buffer
	f.host.d.logger = log.New(&logs, "", 0)

	finished := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, finished) {
		t.Fatal("the seat with an unreadable copy was not removed")
	}
	if hostListed {
		t.Error("the host's directories were read in place of an unreadable copy")
	}
	if dirExists(t, dirs.PolecatDir) {
		t.Error("the seat's directories are still there after the removal")
	}
	lines := usageLogLines(logs.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "listing its copied transcripts") {
		t.Fatalf("usage lines = %q, want one refusal naming the copy", lines)
	}
}

// TestSeatEndNamesTheCapWhenTheCopyWasCutShort: a copy the cap cut short writes
// its record as incomplete, and the reason names the transcripts the cap left
// out, so the record does not read as the seat's whole run.
func TestSeatEndNamesTheCapWhenTheCopyWasCutShort(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.host.d.clock = clockwork.NewFakeClockAt(end)
	f.writeSeatRecordAt(start)

	copyDir := session.SandboxTranscriptCopyDir(f.town, endRig, endPolecat)
	copied := filepath.Join(copyDir, session.SandboxTranscriptFolder(endRig, endPolecat), "sandbox-session.jsonl")
	writeUsageTranscript(t, copied, start.Add(time.Hour), seatCallLine("sandbox", 100, 50, 20))
	// The cap record the copy writes at its root: two transcripts were left out.
	if err := os.WriteFile(filepath.Join(copyDir, "skipped-by-cap"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.host.seatUsage.list = func(_, _ string) ([]string, error) {
		t.Error("the host's Claude directories were read although the sandbox copy holds the seat's transcript")
		return nil, nil
	}
	var logs bytes.Buffer
	f.host.d.logger = log.New(&logs, "", 0)

	finished := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, finished) {
		t.Fatal("the finished seat was not removed")
	}
	lines := usageLogLines(logs.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "transcripts were not read in full") || !strings.Contains(lines[0], "transcript copy cut by the cap: 2 files skipped") {
		t.Fatalf("usage lines = %q, want the record written as incomplete, naming the cap", lines)
	}
}
