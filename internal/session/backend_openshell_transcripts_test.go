package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// copyingRunner is the CLI runner a test installs: a download writes the files
// named in copied into the destination the download was given, and every call
// goes on to the fake, so its calls and failures are the fake's.
func copyingRunner(fake *sandbox.Fake, copied map[string]string) sandbox.Runner {
	run := fake.Runner()
	return func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		if slices.Contains(argv, "download") {
			dest := argv[len(argv)-1]
			for name, body := range copied {
				p := filepath.Join(dest, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return nil, nil, err
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					return nil, nil, err
				}
			}
		}
		return run(ctx, argv, stdin, env)
	}
}

// seatFolder is the project folder Claude Code names for the test seat's sandbox
// clone: the folder a session run in /sandbox/gt/<rig>/polecats/<polecat>/<rig>
// writes to.
func seatFolder() string {
	return fmt.Sprintf("-sandbox-gt-%s-polecats-%s-%s", openshellTestRig, openshellTestPolecat, openshellTestRig)
}

// callIndex is the index of the first call whose argv holds verb, or -1.
func callIndex(calls [][]string, verb string) int {
	for i, c := range calls {
		if slices.Contains(c, verb) {
			return i
		}
	}
	return -1
}

func TestOpenShellOpsDeleteCopiesTheTranscriptsBeforeTheDelete(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = copyingRunner(fake, map[string]string{
		seatFolder() + "/ses-1.jsonl": `{"type":"assistant"}`,
	})
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	calls := fake.Calls()
	down, del := callIndex(calls, "download"), callIndex(calls, "delete")
	if down < 0 || del < 0 || down > del {
		t.Fatalf("openshell calls = %v, want a download before the delete", calls)
	}
	if got := calls[down][len(calls[down])-2:]; !reflect.DeepEqual(got, []string{"/sandbox/.claude/projects", SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)}) {
		t.Errorf("download = %v, want the sandbox projects dir into the seat's copy dir", calls[down])
	}
	copied, err := CopiedSandboxTranscripts(b.townRoot, openshellTestRig, openshellTestPolecat)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 1 || filepath.Base(copied[0]) != "ses-1.jsonl" {
		t.Fatalf("copied transcripts = %v, want ses-1.jsonl", copied)
	}
}

func TestOpenShellOpsDeleteFailedCopyLogsOneLineAndStillDeletes(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"download": errors.New("exit status 1")},
		Stderr: map[string]string{"download": "connection reset"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = copyingRunner(fake, nil)
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want the delete to go on past a failed copy", err)
	}
	if callIndex(fake.Calls(), "delete") < 0 {
		t.Fatalf("openshell calls = %v, want the sandbox deleted", fake.Calls())
	}
	if _, ok, _ := ReadSandboxRecord(b.townRoot, openshellTestSession); ok {
		t.Error("record survived Delete after a failed copy")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], openshellTestSession) || !strings.Contains(warnings[0], "copying its transcripts out failed") {
		t.Fatalf("warnings = %q, want one line naming the sandbox", warnings)
	}
	if _, err := os.Stat(SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)); !os.IsNotExist(err) {
		t.Errorf("partial copy left behind: stat err = %v", err)
	}
}

func TestOpenShellOpsDeleteCopyStopsAtTheCapAndSaysSo(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.transcriptCap = 10
	folder := seatFolder()
	b.cli.Run = copyingRunner(fake, map[string]string{
		folder + "/old.jsonl":    "aaaa",
		folder + "/middle.jsonl": "bbbb",
		folder + "/new.jsonl":    "cccc",
	})
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)
	// The copy writes the files on download; the wrapper then sets their
	// modification times, which decide what the cap keeps.
	inner := b.cli.Run
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		out, errOut, err := inner(ctx, argv, stdin, env)
		if slices.Contains(argv, "download") {
			dest := argv[len(argv)-1]
			stamp := func(name string, at time.Time) {
				if err := os.Chtimes(filepath.Join(dest, folder, name), at, at); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			stamp("old.jsonl", now.Add(-3*time.Hour))
			stamp("middle.jsonl", now.Add(-2*time.Hour))
			stamp("new.jsonl", now.Add(-time.Hour))
		}
		return out, errOut, err
	}

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	copied, err := CopiedSandboxTranscripts(b.townRoot, openshellTestRig, openshellTestPolecat)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range copied {
		names = append(names, filepath.Base(p))
	}
	slices.Sort(names)
	if want := []string{"middle.jsonl", "new.jsonl"}; !reflect.DeepEqual(names, want) {
		t.Errorf("kept transcripts = %v, want the newest that fit the cap %v", names, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "stopped at the 10-byte cap") || !strings.Contains(warnings[0], "1 older files") {
		t.Fatalf("warnings = %q, want one line saying the copy stopped at the cap", warnings)
	}
}

func TestOpenShellOpsDeleteRemovesCopiesOlderThanAWeek(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = copyingRunner(fake, nil)
	writeOpsRecord(t, b)

	rigDir := filepath.Dir(SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, "x"))
	stale := filepath.Join(rigDir, "sandbox-retired")
	fresh := filepath.Join(rigDir, "sandbox-recent")
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	eightDays := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stale, eightDays, eightDays); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("copy older than a week kept: stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("copy within a week removed: %v", err)
	}
}

func TestCopiedSandboxTranscriptsReadsNothingWithoutACopyAndRefusesANonPlainSeat(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if paths, err := CopiedSandboxTranscripts(town, openshellTestRig, openshellTestPolecat); err != nil || len(paths) != 0 {
		t.Fatalf("no copy: paths %v, err %v, want none and no error", paths, err)
	}
	if _, err := CopiedSandboxTranscripts(town, openshellTestRig, "../escape"); err == nil {
		t.Fatal("a polecat name that steps out of the transcripts dir was read")
	}
}

func TestCopiedSandboxTranscriptsRefusesACopyItCannotRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// The seat's folder is a file, so the copy cannot be listed as a directory.
	copyDir := SandboxTranscriptCopyDir(town, openshellTestRig, openshellTestPolecat)
	if err := os.MkdirAll(copyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, SandboxTranscriptFolder(openshellTestRig, openshellTestPolecat)), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CopiedSandboxTranscripts(town, openshellTestRig, openshellTestPolecat); err == nil {
		t.Fatal("an unreadable copy listed as holding none, want the read failure")
	}
}

func TestOpenShellOpsDeleteWarnsWhenTheCopyVanishesBeforeItIsCapped(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	inner := copyingRunner(fake, nil)
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		out, errOut, err := inner(ctx, argv, stdin, env)
		if slices.Contains(argv, "download") {
			// The copy is gone when the cap walks it: the walk fails.
			if rmErr := os.RemoveAll(argv[len(argv)-1]); rmErr != nil {
				t.Fatal(rmErr)
			}
		}
		return out, errOut, err
	}
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want the delete to go on past an unreadable copy", err)
	}
	if callIndex(fake.Calls(), "delete") < 0 {
		t.Fatalf("openshell calls = %v, want the sandbox deleted", fake.Calls())
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "reading its transcript copy failed") {
		t.Fatalf("warnings = %q, want one line saying the copy could not be read", warnings)
	}
}

func TestOpenShellOpsDeleteWarnsWhenTheTranscriptsDirCannotBeRead(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	b.cli.Run = copyingRunner(fake, nil)
	// The transcripts directory is a file, so neither the prune nor the copy can use it.
	copyDir := SandboxTranscriptCopyDir(b.townRoot, openshellTestRig, openshellTestPolecat)
	transcripts := filepath.Dir(filepath.Dir(copyDir))
	if err := os.MkdirAll(filepath.Dir(transcripts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcripts, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	b.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete = %v, want the delete to go on", err)
	}
	if callIndex(fake.Calls(), "delete") < 0 {
		t.Fatalf("openshell calls = %v, want the sandbox deleted", fake.Calls())
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "pruning old transcript copies failed") {
		t.Fatalf("warnings = %q, want the prune failure named", warnings)
	}
}
