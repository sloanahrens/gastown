package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// pruneWireBackend is a backend whose clock is fixed, with an archive holding one transcript
// older than the keep and one younger, for the seat of the test seat name.
func pruneWireBackend(t *testing.T) (b *openshellBackend, warnings *[]string, oldPath, youngPath string) {
	t.Helper()
	fake := &sandbox.Fake{}
	b, _ = opsBackend(t, fake, newSandboxProbe(t, true))
	now := seatBase.Add(10 * sandboxTranscriptKeep)
	b.now = func() time.Time { return now }
	warnings = &[]string{}
	b.warnf = func(format string, args ...any) { *warnings = append(*warnings, fmt.Sprintf(format, args...)) }
	archive := SandboxTranscriptArchiveDir(b.townRoot, openshellTestRig, openshellTestPolecat)
	oldPath, youngPath = filepath.Join(archive, "old.jsonl"), filepath.Join(archive, "young.jsonl")
	for p, age := range map[string]time.Duration{oldPath: 2 * sandboxTranscriptKeep, youngPath: time.Hour} {
		if err := os.MkdirAll(archive, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	remote := seatRemote("ses-1.jsonl")
	b.cli.Run = copyingRunner(fake, listingLine(remote, now, 4), map[string]string{remote: `{"type":"user"}`})
	return b, warnings, oldPath, youngPath
}

// TestSaveSeatTranscriptsPrunesTheArchive: every seat end prunes the transcript archive after it
// archives: an archived transcript older than the keep goes and a younger one stays (gt-i55f4x).
func TestSaveSeatTranscriptsPrunesTheArchive(t *testing.T) {
	t.Parallel()
	b, warnings, oldPath, youngPath := pruneWireBackend(t)
	if err := b.saveSeatTranscripts(SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat}); err != nil {
		t.Fatalf("saveSeatTranscripts: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("the transcript past the keep is still archived: %v", err)
	}
	if _, err := os.Stat(youngPath); err != nil {
		t.Errorf("the young transcript left the archive: %v", err)
	}
	if len(*warnings) != 0 {
		t.Errorf("warnings = %q, want none", *warnings)
	}
}

// TestSaveSeatTranscriptsPruneFailureWarnsAndCopies: a prune that fails is warned once naming the
// file and the seat's copy is still written (gt-i55f4x).
func TestSaveSeatTranscriptsPruneFailureWarnsAndCopies(t *testing.T) {
	t.Parallel()
	b, warnings, oldPath, _ := pruneWireBackend(t)
	f := &failingArchiveFS{removeFail: map[string]bool{oldPath: true}}
	b.archiveFS = f.fs()
	if err := b.saveSeatTranscripts(SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat}); err != nil {
		t.Fatalf("a failed prune failed the seat end: %v", err)
	}
	if len(*warnings) != 1 || !strings.Contains((*warnings)[0], oldPath) || !strings.Contains((*warnings)[0], "pruning the transcript archive failed") {
		t.Fatalf("warnings = %q, want one naming %s and the failed prune", *warnings, oldPath)
	}
	copied, _, err := CopiedSandboxTranscripts(b.townRoot, openshellTestRig, openshellTestPolecat)
	if err != nil || len(copied) != 1 || filepath.Base(copied[0]) != "ses-1.jsonl" {
		t.Fatalf("copied transcripts = %v, %v; want the seat's copy written", copied, err)
	}
}

// TestArchiveFSOrRealFillsEachField: a backend archiveFS that sets only some of its calls keeps
// those and takes the real filesystem's for the rest, so a partly filled one is not silently
// replaced whole (gt-i55f4x).
func TestArchiveFSOrRealFillsEachField(t *testing.T) {
	t.Parallel()
	var b openshellBackend
	got := b.archiveFSOrReal()
	if got.readDir == nil || got.remove == nil || got.info == nil {
		t.Fatalf("a zero archiveFS = %+v, want every call filled from the real filesystem", got)
	}
	marker := errors.New("remove refused")
	b.archiveFS = archiveFS{remove: func(string) error { return marker }}
	got = b.archiveFSOrReal()
	if got.readDir == nil || got.info == nil {
		t.Errorf("a partly filled archiveFS lost its real readDir or info: %+v", got)
	}
	if err := got.remove("x"); !errors.Is(err, marker) {
		t.Errorf("the set remove was replaced: err = %v, want the test's", err)
	}
}
