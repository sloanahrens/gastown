package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

const logCmd = "log -z --format=%H%x00%P%x00%B --end-of-options origin/main..HEAD"

// fakeGit answers the two commands the check runs from canned output, or fails
// the log when errLog is set. It keeps the unit tier's runs process-free.
type fakeGit struct {
	log  string
	diff map[string]string
	err  error
}

func (f fakeGit) run(args []string) ([]byte, error) {
	if strings.Join(args, " ") == logCmd {
		if f.err != nil {
			return nil, f.err
		}
		return []byte(f.log), nil
	}
	for sha, out := range f.diff {
		if strings.HasSuffix(strings.Join(args, " "), " "+sha) {
			return []byte(out), nil
		}
	}
	return nil, fmt.Errorf("unexpected git command %q", args)
}

// TestRunFailsACommitThatChangesAModuleFileWithoutAReason: the unexplained
// change exits non-zero and the report names the commit, its module file and
// the amend that fixes it (gt-b9j3o.6).
func TestRunFailsACommitThatChangesAModuleFileWithoutAReason(t *testing.T) {
	t.Parallel()
	git := fakeGit{
		log:  "0123456789abcdef0123\x00p\x00bump x (gt-1)\x00",
		diff: map[string]string{"0123456789abcdef0123": "go.mod\x00"},
	}
	var out bytes.Buffer
	if code := run(nil, &out, git.run); code != 1 {
		t.Errorf("run = %d, want 1 for an unexplained module change", code)
	}
	for _, want := range []string{
		"judged 1 commit(s)",
		"0123456789ab bump x (gt-1): changes go.mod with no rationale",
		"Rationale: <why the module graph moved>",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report = %q, want it to contain %q", out.String(), want)
		}
	}
}

// TestRunPassesACommitThatExplainsItsChange: a rationale line passes, and the
// report still says what was judged, so a clean run is not a silent one.
func TestRunPassesACommitThatExplainsItsChange(t *testing.T) {
	t.Parallel()
	git := fakeGit{
		log:  "abc\x00p\x00bump x (gt-1)\n\nRationale: x v2 fixes the panic.\x00",
		diff: map[string]string{"abc": "go.mod\x00go.sum\x00"},
	}
	var out bytes.Buffer
	if code := run(nil, &out, git.run); code != 0 {
		t.Errorf("run = %d, want 0: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "judged 1 commit(s)") || !strings.Contains(out.String(), "0 without a rationale line") {
		t.Errorf("report = %q, want the judged counts", out.String())
	}
}

// TestRunPassesABranchThatTouchesNoModuleFile: an ordinary branch passes and
// its report says nothing was judged, rather than reading as a clean check.
func TestRunPassesABranchThatTouchesNoModuleFile(t *testing.T) {
	t.Parallel()
	git := fakeGit{log: "abc\x00p\x00fix x (gt-1)\x00", diff: map[string]string{"abc": "internal/x/x.go\x00"}}
	var out bytes.Buffer
	if code := run(nil, &out, git.run); code != 0 {
		t.Errorf("run = %d, want 0: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "judged 0 commit(s)") {
		t.Errorf("report = %q, want it to say nothing was judged", out.String())
	}
}

// TestRunFailsWhenGitCannotBeRead: git's own failure fails the check and its
// message says the branch could not be read, not that a rationale is missing.
func TestRunFailsWhenGitCannotBeRead(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 128")
	var out bytes.Buffer
	if code := run(nil, &out, fakeGit{err: boom}.run); code != 1 {
		t.Errorf("run = %d, want 1 when git fails", code)
	}
	if !strings.Contains(out.String(), "cannot read the branch") {
		t.Errorf("report = %q, want it to name the unreadable branch", out.String())
	}
	if strings.Contains(out.String(), "Rationale:") {
		t.Errorf("report = %q, want no rationale advice for a git failure", out.String())
	}
}

// TestRunRefusesAnEmptyBase: an empty base would read as an empty range and
// pass a branch nothing judged, so the command refuses it instead.
func TestRunRefusesAnEmptyBase(t *testing.T) {
	t.Parallel()
	called := false
	var out bytes.Buffer
	code := run([]string{"-base", ""}, &out, func([]string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if code != 2 {
		t.Errorf("run = %d, want 2 for an empty base", code)
	}
	if called {
		t.Error("git ran despite the empty base")
	}
}

// TestRunRefusesABadFlag: a flag the command cannot read exits 2 without
// reading git at all.
func TestRunRefusesABadFlag(t *testing.T) {
	t.Parallel()
	called := false
	var out bytes.Buffer
	code := run([]string{"-nope"}, &out, func([]string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if code != 2 {
		t.Errorf("run = %d, want 2 for an unreadable flag", code)
	}
	if called {
		t.Error("git ran despite the bad flag")
	}
}
