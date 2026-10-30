package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// The recorder runs bd only through an injected Runner, so the hermetic harness keeps the test
// binary off the live town's state, as every package that can reach it must.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// safeCwd is the working directory runIn is given by a test that is not about the guard: a temp
// directory whose parents hold no beads workspace.
func safeCwd(t *testing.T) func() (string, error) {
	t.Helper()
	dir := t.TempDir()
	return func() (string, error) { return dir, nil }
}

// TestGuardRunRefusesWhatItCannotCheck: a working directory the process cannot name, or a marker it
// cannot read, refuses the run rather than being taken for a safe tree.
func TestGuardRunRefusesWhatItCannotCheck(t *testing.T) {
	t.Parallel()
	t.Run("a working directory it cannot name", func(t *testing.T) {
		t.Parallel()
		unnamed := func() (string, error) { return "", errors.New("getwd: i/o error") }
		err := guardRun(unnamed, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "i/o error") {
			t.Fatalf("guardRun with no working directory = %v, want the getwd failure reported", err)
		}
	})
	t.Run("a marker it cannot read", func(t *testing.T) {
		t.Parallel()
		// A marker symlinked to itself fails the stat with ELOOP, which is neither a workspace nor a
		// missing marker.
		tree := t.TempDir()
		if err := os.Symlink(".beads", filepath.Join(tree, ".beads")); err != nil {
			t.Fatalf("make the marker loop: %v", err)
		}
		err := guardRun(safeCwd(t), tree)
		if err == nil || !strings.Contains(err.Error(), "check for a beads workspace") {
			t.Fatalf("guardRun over an unreadable marker = %v, want the read failure reported", err)
		}
	})
}

// TestGuardRunAcceptsAClearScratchDirectory: a -scratch that is not there yet passes, since run
// makes it after the check; nothing above it holds a workspace.
func TestGuardRunAcceptsAClearScratchDirectory(t *testing.T) {
	t.Parallel()
	scratch := filepath.Join(t.TempDir(), "scratch")
	if err := guardRun(safeCwd(t), scratch); err != nil {
		t.Fatalf("guardRun with a clear scratch directory = %v, want it accepted", err)
	}
}

// TestRunRefusesAScratchDirectoryItCannotMake: fixture copies need a scratch directory, and when it
// cannot be made the command stops with the failure reported, before bd runs.
func TestRunRefusesAScratchDirectoryItCannotMake(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	args := []string{"-fixture", writeFixture(t, fixtureIDs...), "-scratch", ""}
	if code := runIn(safeCwd(t), args, &stdout, &stderr); code != 1 {
		t.Errorf("run with an empty -scratch = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "make scratch directory") {
		t.Errorf("stderr = %q, want the scratch directory failure reported", stderr.String())
	}
}

// TestRunRefusesAMissingFixture: without -fixture there is nothing for bd to run in, so the command
// stops before it runs anything.
func TestRunRefusesAMissingFixture(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := runIn(safeCwd(t), nil, &stdout, &stderr); code != 2 {
		t.Errorf("run with no -fixture = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "-fixture is required") {
		t.Errorf("stderr = %q, want the missing -fixture named", stderr.String())
	}
}

// TestRunRefusesAFixtureFileThatIsNotThere: a -fixture that names no file stops the run before any
// database is made.
func TestRunRefusesAFixtureFileThatIsNotThere(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.jsonl")
	if code := runIn(safeCwd(t), []string{"-fixture", missing, "-scratch", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Errorf("run with a missing fixture file = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "open the fixture") {
		t.Errorf("stderr = %q, want the fixture failure reported", stderr.String())
	}
}

// TestRunRefusesAFixtureMissingABead: a fixture file that lacks a bead the table names stops the
// run before any database is made, naming the bead.
func TestRunRefusesAFixtureMissingABead(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	fixture := writeFixture(t, fixtureID, fixtureParent)
	if code := runIn(safeCwd(t), []string{"-fixture", fixture, "-scratch", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Errorf("run over a fixture missing %s = %d, want 1", fixtureDepID, code)
	}
	if !strings.Contains(stderr.String(), fixtureDepID) {
		t.Errorf("stderr = %q, want the missing bead named", stderr.String())
	}
}

// TestRunRefusesAScratchUnderAWorkspace: bd init walks up from the copy it runs in, and a copy under
// a tree that holds a beads workspace would make it rewrite that workspace's store (gt-p85pw.103).
// The run is refused before bd runs and before anything is written, so the workspace is untouched.
func TestRunRefusesAScratchUnderAWorkspace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		marker func(t *testing.T, tree string) string // returns the file the tree must keep byte for byte
	}{
		{"a .beads directory", func(t *testing.T, tree string) string {
			metadata := filepath.Join(tree, ".beads", "metadata.json")
			mustWrite(t, metadata, `{"dolt_database":"hq"}`)
			return metadata
		}},
		{"a metadata.json", func(t *testing.T, tree string) string {
			metadata := filepath.Join(tree, "metadata.json")
			mustWrite(t, metadata, `{"dolt_database":"hq"}`)
			return metadata
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := t.TempDir()
			metadata := tc.marker(t, tree)
			before := readFile(t, metadata)
			scratch := filepath.Join(tree, "scratch")
			out := filepath.Join(t.TempDir(), "golden")
			var stdout, stderr bytes.Buffer
			args := []string{
				"-fixture", writeFixture(t, fixtureIDs...),
				"-scratch", scratch,
				"-out", out,
				"-bd", filepath.Join(t.TempDir(), "no-such-bd"),
			}
			if code := runIn(safeCwd(t), args, &stdout, &stderr); code != 1 {
				t.Fatalf("run with -scratch under a workspace = %d, want 1\nstderr: %s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "beads workspace") {
				t.Errorf("stderr = %q, want the workspace named", stderr.String())
			}
			if strings.Contains(stderr.String(), "no such file") {
				t.Errorf("stderr = %q, want no bd run attempted", stderr.String())
			}
			if after := readFile(t, metadata); after != before {
				t.Errorf("%s = %q after the refused run, want %q", metadata, after, before)
			}
			if _, err := os.Stat(scratch); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("scratch %s stat = %v, want the refused run to make nothing", scratch, err)
			}
			if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
				t.Errorf("goldens written under a workspace: %v", entries)
			}
		})
	}
}

// TestRunRefusesASymlinkedScratchUnderAWorkspace: bd follows the symlinks on its way to a directory,
// so a -scratch whose lexical path never passes a workspace but whose symlink leads into one is
// refused too (gt-p85pw.103). Nothing is made and the workspace is untouched.
func TestRunRefusesASymlinkedScratchUnderAWorkspace(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	metadata := filepath.Join(tree, ".beads", "metadata.json")
	mustWrite(t, metadata, `{"dolt_database":"hq"}`)
	before := readFile(t, metadata)
	inside := filepath.Join(tree, "inside")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatalf("make the directory the link leads to: %v", err)
	}
	link := filepath.Join(t.TempDir(), "clean-looking")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatalf("make the symlink: %v", err)
	}
	scratch := filepath.Join(link, "scratch")
	out := filepath.Join(t.TempDir(), "golden")
	var stdout, stderr bytes.Buffer
	args := []string{
		"-fixture", writeFixture(t, fixtureIDs...),
		"-scratch", scratch,
		"-out", out,
		"-bd", filepath.Join(t.TempDir(), "no-such-bd"),
	}
	if code := runIn(safeCwd(t), args, &stdout, &stderr); code != 1 {
		t.Fatalf("run with -scratch behind a symlink into a workspace = %d, want 1\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "beads workspace") {
		t.Errorf("stderr = %q, want the workspace named", stderr.String())
	}
	if strings.Contains(stderr.String(), "no such file") {
		t.Errorf("stderr = %q, want no bd run attempted", stderr.String())
	}
	if after := readFile(t, metadata); after != before {
		t.Errorf("%s = %q after the refused run, want %q", metadata, after, before)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Errorf("the linked directory stat = %v, want the refused run to make nothing", err)
	}
	if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
		t.Errorf("goldens written under a workspace: %v", entries)
	}
}

// TestGuardRunRefusesAScratchItCannotResolve: a -scratch whose symlinks loop cannot be followed, so
// the recorder cannot tell which workspace bd init would reach and refuses rather than guessing.
func TestGuardRunRefusesAScratchItCannotResolve(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	loop := filepath.Join(tree, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatalf("make the symlink loop: %v", err)
	}
	err := guardRun(safeCwd(t), filepath.Join(loop, "scratch"))
	if err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("guardRun over a symlink loop = %v, want the resolve failure reported", err)
	}
}

// TestGuardRunAcceptsASymlinkedScratchOutsideAWorkspace: resolving the link must not turn a clear
// scratch directory into a refusal, or the guard would block the recording it exists to protect.
func TestGuardRunAcceptsASymlinkedScratchOutsideAWorkspace(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("make the symlink: %v", err)
	}
	if err := guardRun(safeCwd(t), filepath.Join(link, "scratch")); err != nil {
		t.Errorf("guardRun over a symlink to %s = %v, want it accepted", target, err)
	}
}

// TestRunRefusesAWorkingDirectoryUnderAWorkspace: the recorder's own working directory is checked
// too, since a recorder started inside a beads worktree is where the incident began.
func TestRunRefusesAWorkingDirectoryUnderAWorkspace(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	metadata := filepath.Join(tree, ".beads", "metadata.json")
	mustWrite(t, metadata, `{"dolt_database":"hq"}`)
	before := readFile(t, metadata)
	work := filepath.Join(tree, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatalf("make the working directory: %v", err)
	}
	out := filepath.Join(t.TempDir(), "golden")
	var stdout, stderr bytes.Buffer
	args := []string{
		"-fixture", writeFixture(t, fixtureIDs...),
		"-scratch", t.TempDir(),
		"-out", out,
		"-bd", filepath.Join(t.TempDir(), "no-such-bd"),
	}
	inWorkspace := func() (string, error) { return work, nil }
	if code := runIn(inWorkspace, args, &stdout, &stderr); code != 1 {
		t.Fatalf("run from a working directory under a workspace = %d, want 1\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "beads workspace") {
		t.Errorf("stderr = %q, want the workspace named", stderr.String())
	}
	if strings.Contains(stderr.String(), "no such file") {
		t.Errorf("stderr = %q, want no bd run attempted", stderr.String())
	}
	if after := readFile(t, metadata); after != before {
		t.Errorf("%s = %q after the refused run, want %q", metadata, after, before)
	}
	if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
		t.Errorf("goldens written under a workspace: %v", entries)
	}
}
