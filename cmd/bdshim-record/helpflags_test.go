package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helpBD answers bd's help for the arguments a test names.
type helpBD map[string]string

func (f helpBD) run(dir string, args []string) ([]byte, int, error) {
	if out, ok := f[strings.Join(args, " ")]; ok {
		return []byte(out), 0, nil
	}
	return []byte("bd: no such command"), 1, nil
}

// oneCommand is the smallest bd whose help records: a version and a root
// command with one flag.
var oneCommand = helpBD{
	"version": "bd version 1.2.2 (dev) schema<=66\n",
	"--help": `Usage:
  bd [flags]

Flags:
      --json   Output in JSON format
`,
}

// TestRecordFlagsCheck: -check reports whether the recorded file still matches
// bd's help and writes nothing, so a bd upgrade has a command that names it.
func TestRecordFlagsCheck(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bd-flags.txt")
	scratch := tempDir(t)
	var stderr bytes.Buffer
	if code := recordFlags(options{out: path, scratch: scratch}, oneCommand.run, &stderr); code != 0 {
		t.Fatalf("recordFlags = %d, want 0: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "wrote "+path) {
		t.Errorf("stderr = %q, want the file it wrote named", stderr.String())
	}

	stderr.Reset()
	if code := recordFlags(options{out: path, scratch: scratch, check: true}, oneCommand.run, &stderr); code != 0 {
		t.Fatalf("recordFlags -check on its own output = %d, want 0: %s", code, stderr.String())
	}

	stale := []byte("value\tcreate\t--title\n")
	if err := os.WriteFile(path, stale, 0o644); err != nil {
		t.Fatalf("writing a stale recording: %v", err)
	}
	stderr.Reset()
	if code := recordFlags(options{out: path, scratch: scratch, check: true}, oneCommand.run, &stderr); code != 1 {
		t.Fatalf("recordFlags -check on a stale file = %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "differs from bd's help") {
		t.Errorf("stderr = %q, want the drift named", stderr.String())
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, stale) {
		t.Error("-check rewrote the file, want it left untouched")
	}
}

// rootWithMail is the root help of a bd whose one command, mail, the walk must reach to record it.
const rootWithMail = `Usage:
  bd [command]

Available Commands:
  mail        Send and receive messages

Flags:
      --json   Output in JSON format
`

// mailHelp is the help of the mail command, whose flags the walk records under it.
const mailHelp = `Usage:
  bd mail [flags]

Flags:
      --to string   Recipient of the message
`

// TestRecordFlagsRunsEveryCallInTheScratch: bd mail --help needs a store to print, so every help
// call the walk makes runs in the scratch workspace the recording holds. A call with no directory
// fails the test, and the walk reaching mail's help is what the recording proves (gt-ufil6n).
func TestRecordFlagsRunsEveryCallInTheScratch(t *testing.T) {
	t.Parallel()
	scratch := tempDir(t)
	help := helpBD{
		"version":     "bd version 1.2.2 (dev) schema<=66\n",
		"--help":      rootWithMail,
		"mail --help": mailHelp,
	}
	var calls []string
	run := func(dir string, args []string) ([]byte, int, error) {
		calls = append(calls, strings.Join(args, " "))
		if dir != scratch {
			t.Errorf("bd %s ran in %q, want the scratch %s", strings.Join(args, " "), dir, scratch)
			return []byte("bd: no beads database found\n"), 1, nil
		}
		return help.run(dir, args)
	}
	path := filepath.Join(t.TempDir(), "bd-flags.txt")
	var stderr bytes.Buffer
	if code := recordFlags(options{out: path, scratch: scratch}, run, &stderr); code != 0 {
		t.Fatalf("recordFlags = %d, want 0: %s", code, stderr.String())
	}
	reached := false
	for _, call := range calls {
		if call == "mail --help" {
			reached = true
		}
	}
	if !reached {
		t.Fatalf("bd ran %v, want the walk to reach mail's help", calls)
	}
}

// TestRecordFlagsRefusesAWalkWithNoScratch: a walk with no scratch directory would run bd's help
// with no directory, where bd mail --help exits 1, so the walk refuses to start and runs no bd.
func TestRecordFlagsRefusesAWalkWithNoScratch(t *testing.T) {
	t.Parallel()
	calls := 0
	run := func(string, []string) ([]byte, int, error) {
		calls++
		return nil, 0, nil
	}
	var stderr bytes.Buffer
	if code := recordFlags(options{out: filepath.Join(t.TempDir(), "bd-flags.txt")}, run, &stderr); code != 1 {
		t.Fatalf("recordFlags with no scratch = %d, want 1: %s", code, stderr.String())
	}
	if calls != 0 {
		t.Errorf("bd ran %d times with no scratch, want none", calls)
	}
	if !strings.Contains(stderr.String(), "scratch directory") {
		t.Errorf("stderr = %q, want the missing scratch named", stderr.String())
	}
}

// TestRunFlagsRemovesTheScratchWorkspaceOnEveryExit: the workspace the flags walk holds is dropped
// and removed once the walk ends, whether the walk succeeded, failed, or its drop failed, and every
// bd call of the walk runs in the scratch (gt-ufil6n).
func TestRunFlagsRemovesTheScratchWorkspaceOnEveryExit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		rootExit int // the exit of the root help, which fails the walk when non-zero
		dropExit int // the exit of the drop that releases the workspace
		wantCode int
		wantErr  string
	}{
		{name: "a walk that succeeds", wantCode: 0, wantErr: "wrote"},
		{name: "a walk that fails", rootExit: 1, wantCode: 1, wantErr: "exited 1"},
		{name: "a drop that fails", dropExit: 1, wantCode: 1, wantErr: "clean up the scratch workspace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scratch := tempDir(t)
			out := filepath.Join(tempDir(t), "bd-flags.txt")
			bd := &fakeBD{out: func(args []string) ([]byte, int, error) {
				switch {
				case args[0] == "init":
					return nil, 0, nil
				case args[0] == "sql":
					return nil, tc.dropExit, nil
				case len(args) == 1 && args[0] == "--help" && tc.rootExit != 0:
					return nil, tc.rootExit, nil
				}
				return oneCommand.run("", args)
			}}
			var stdout, stderr bytes.Buffer
			args := []string{"-flags", "-scratch", scratch, "-out", out}
			if code := runIn(safeCwd(t), childFor(passingChild(bd)), args, &stdout, &stderr); code != tc.wantCode {
				t.Fatalf("run = %d, want %d\nstderr: %s", code, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.wantErr)
			}
			if _, err := os.Stat(filepath.Join(scratch, ".beads")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the scratch workspace stat = %v, want it removed once the walk ends", err)
			}
			if drops := callIndices(bd, "sql"); len(drops) != 1 {
				t.Errorf("bd ran %v, want the workspace's database dropped once", bd.args)
			}
			for i, dir := range bd.dirs {
				if dir != scratch {
					t.Errorf("bd %v ran in %q, want the scratch %s", bd.args[i], dir, scratch)
				}
			}
		})
	}
}

// TestRunFlagsRefusesAWorkingDirectoryThatIsAWorkspaceRoot: the flags walk goes through the same
// guard as the goldens, so a recorder started in a workspace root stops before the scratch is made
// or any bd runs (gt-p85pw.116, gt-ufil6n).
func TestRunFlagsRefusesAWorkingDirectoryThatIsAWorkspaceRoot(t *testing.T) {
	t.Parallel()
	tree := tempDir(t)
	mustWrite(t, filepath.Join(tree, ".beads", "metadata.json"), `{"dolt_database":"hq"}`)
	bd := &fakeBD{out: okBD}
	scratch := filepath.Join(tempDir(t), "scratch")
	var stdout, stderr bytes.Buffer
	args := []string{"-flags", "-scratch", scratch, "-out", filepath.Join(tempDir(t), "bd-flags.txt")}
	if code := runIn(cwdAt(tree), childFor(passingChild(bd)), args, &stdout, &stderr); code != 1 {
		t.Fatalf("flags run from a workspace root = %d, want 1\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "workspace root") {
		t.Errorf("stderr = %q, want the workspace root named", stderr.String())
	}
	if len(bd.args) != 0 {
		t.Errorf("bd ran %v, want no bd run attempted", bd.args)
	}
	if _, err := os.Stat(scratch); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the scratch stat = %v, want no scratch made for a refused run", err)
	}
}

// TestRunFlagsRefusesAnEmptyScratch: -scratch names the directory the walk runs bd in for a flags
// recording too, so an empty one is refused before any bd runs.
func TestRunFlagsRefusesAnEmptyScratch(t *testing.T) {
	t.Parallel()
	bd := &fakeBD{out: okBD}
	var stdout, stderr bytes.Buffer
	args := []string{"-flags", "-scratch", "", "-out", filepath.Join(tempDir(t), "bd-flags.txt")}
	if code := runIn(safeCwd(t), childFor(passingChild(bd)), args, &stdout, &stderr); code != 1 {
		t.Errorf("run with an empty -scratch = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "make scratch directory") {
		t.Errorf("stderr = %q, want the scratch directory failure reported", stderr.String())
	}
	if len(bd.args) != 0 {
		t.Errorf("bd ran %v for a scratch directory that was never made", bd.args)
	}
}
