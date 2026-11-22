package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sandbox"
)

func TestSaveSeatTranscriptsArchivesThePreviousCopy(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
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
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	remote := seatRemote("ses-1.jsonl")
	b.cli.Run = copyingRunner(fake, listingLine(remote, seatBase, 4), map[string]string{remote: `{"type":"user"}`})

	// No seat end came before this one, so there is no previous copy to archive.
	if err := b.saveSeatTranscripts(SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat}); err != nil {
		t.Fatalf("saveSeatTranscripts: %v", err)
	}
	if _, err := os.Stat(SandboxTranscriptArchiveDir(b.townRoot, openshellTestRig, openshellTestPolecat)); !os.IsNotExist(err) {
		t.Errorf("archive made for a seat with no previous copy: stat err = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none for a seat with no previous copy", warnings)
	}
}
