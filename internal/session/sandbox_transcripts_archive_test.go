package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sandbox"
)

func TestSaveSeatTranscriptsArchivesThePreviousCopy(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.now = func() time.Time { return seatBase.Add(2 * time.Hour) } // the archive is pruned against this clock; the transcripts date from seatBase
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	first, second := seatRemote("ses-1.jsonl"), seatRemote("ses-2.jsonl")

	// The first seat end copies its session's transcript into the seat's copy.
	b.cli.Run = copyingRunner(fake, listingLine(first, seatBase, 4), map[string]string{first: `{"type":"user"}`})
	if err := b.saveSeatTranscripts(SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat}); err != nil {
		t.Fatalf("first seat end: %v", err)
	}
	// The second seat end of the same name clears that copy for the next session's.
	b.cli.Run = copyingRunner(fake, listingLine(second, seatBase.Add(time.Hour), 4), map[string]string{second: `{"type":"assistant"}`})
	if err := b.saveSeatTranscripts(SandboxRecord{Session: openshellTestSession + "-2", Rig: openshellTestRig, Polecat: openshellTestPolecat}); err != nil {
		t.Fatalf("second seat end: %v", err)
	}

	archived := filepath.Join(SandboxTranscriptArchiveDir(b.townRoot, openshellTestRig, openshellTestPolecat), "ses-1.jsonl")
	if body, err := os.ReadFile(archived); err != nil || string(body) != `{"type":"user"}` {
		t.Fatalf("archived first session = %q, %v; want its transcript in the archive", body, err)
	}
	copied, _, err := CopiedSandboxTranscripts(b.townRoot, openshellTestRig, openshellTestPolecat)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 1 || filepath.Base(copied[0]) != "ses-2.jsonl" {
		t.Fatalf("copied transcripts = %v, want only ses-2.jsonl", copied)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none", warnings)
	}
}

func TestSaveSeatTranscriptsArchivesThePreviousCopyOnlyWhenThereIsOne(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.now = func() time.Time { return seatBase.Add(2 * time.Hour) } // the archive is pruned against this clock; the transcripts date from seatBase
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	remote := seatRemote("ses-1.jsonl")
	b.cli.Run = copyingRunner(fake, listingLine(remote, seatBase, 4), map[string]string{remote: `{"type":"user"}`})

	// No seat end came before this one, so there is no previous copy to archive.
	if err := b.saveSeatTranscripts(SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat}); err != nil {
		t.Fatalf("saveSeatTranscripts: %v", err)
	}
	if entries, err := filepath.Glob(filepath.Join(SandboxTranscriptArchiveDir(b.townRoot, openshellTestRig, openshellTestPolecat), "*")); err != nil || len(entries) != 0 {
		t.Errorf("archive holds %v (%v) for a seat with no previous copy, want no entries", entries, err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none for a seat with no previous copy", warnings)
	}
}

// TestSaveSeatTranscriptsArchiveFailureWarnsAndStillCopies: an archive that cannot be written is
// not the seat end's input: the failure is warned once naming the sandbox, nothing reaches the
// archive, and the new session's copy is still written (gt-e6cjv3).
func TestSaveSeatTranscriptsArchiveFailureWarnsAndStillCopies(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.now = func() time.Time { return seatBase.Add(2 * time.Hour) } // the archive is pruned against this clock; the transcripts date from seatBase
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	first, second := seatRemote("ses-1.jsonl"), seatRemote("ses-2.jsonl")
	b.cli.Run = copyingRunner(fake, listingLine(first, seatBase, 4), map[string]string{first: `{"type":"user"}`})
	if err := b.saveSeatTranscripts(SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat}); err != nil {
		t.Fatalf("first seat end: %v", err)
	}
	// A leftover partial that cannot be removed makes the archive of ses-1.jsonl fail.
	archive := SandboxTranscriptArchiveDir(b.townRoot, openshellTestRig, openshellTestPolecat)
	if err := os.MkdirAll(filepath.Join(archive, "ses-1.jsonl.partial", "x"), 0o755); err != nil {
		t.Fatal(err)
	}

	b.cli.Run = copyingRunner(fake, listingLine(second, seatBase.Add(time.Hour), 4), map[string]string{second: `{"type":"assistant"}`})
	if err := b.saveSeatTranscripts(SandboxRecord{Session: openshellTestSession + "-2", Rig: openshellTestRig, Polecat: openshellTestPolecat}); err != nil {
		t.Fatalf("a failed archive failed the seat end: %v", err)
	}

	if len(warnings) != 1 || !strings.Contains(warnings[0], openshellTestSession+"-2") || !strings.Contains(warnings[0], "archiving the previous transcript copy failed") {
		t.Fatalf("warnings = %q, want one naming the sandbox and the failed archive", warnings)
	}
	if _, err := os.Lstat(filepath.Join(archive, "ses-1.jsonl")); !os.IsNotExist(err) {
		t.Errorf("a failed archive left a final-name file: %v", err)
	}
	copied, _, err := CopiedSandboxTranscripts(b.townRoot, openshellTestRig, openshellTestPolecat)
	if err != nil || len(copied) != 1 || filepath.Base(copied[0]) != "ses-2.jsonl" {
		t.Fatalf("copied transcripts = %v, %v; want the new session's copy written", copied, err)
	}
}
