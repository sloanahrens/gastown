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

// fakeGit answers the commands IntegrationRefs runs from canned output: the
// branch's diff, git grep for tagged tests, and each file at HEAD. A failing
// command returns its err with whatever output it was given, so a test can show
// that the output is never echoed.
type fakeGit struct {
	diff, grep string
	files      map[string]string
	diffErr    error
	grepErr    error
	ran        int
}

func (f *fakeGit) run(args []string) ([]byte, error) {
	f.ran++
	switch args[0] {
	case "diff":
		return []byte(f.diff), f.diffErr
	case "grep":
		return []byte(f.grep), f.grepErr
	case "show":
		if src, ok := f.files[strings.TrimPrefix(args[1], "HEAD:")]; ok {
			return []byte(src), nil
		}
	}
	return nil, fmt.Errorf("unexpected git command %q", args)
}

// hitDiff adds line 3 of pkg/a.go, the line a tagged test names in the hit case.
const hitDiff = "diff --git a/pkg/a.go b/pkg/a.go\n--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -0,0 +3 @@\n+func Changed() {}\n"

// TestRunReportsTheCountsOnANoHitRun: a branch with no Go change and no tagged
// test exits 0 and still prints the summary, so an empty run reads as judged
// nothing rather than as silence (gt-rkuj52).
func TestRunReportsTheCountsOnANoHitRun(t *testing.T) {
	t.Parallel()
	git := &fakeGit{}
	var out bytes.Buffer
	if code := run(nil, &out, git.run); code != 0 {
		t.Errorf("run = %d, want 0 for a run with no hit", code)
	}
	want := "integrationrefs: judged 0 changed declaration(s) and 0 integration-tagged file(s)\n"
	if out.String() != want {
		t.Errorf("report = %q, want %q", out.String(), want)
	}
}

// TestRunCountsAHitAndStillExitsZero: a tagged test that names a changed
// declaration is counted in the summary, and a hit does not fail the run.
func TestRunCountsAHitAndStillExitsZero(t *testing.T) {
	t.Parallel()
	git := &fakeGit{
		diff: hitDiff,
		grep: "pkg/a_test.go\x00",
		files: map[string]string{
			"pkg/a.go":      "package p\n\nfunc Changed() {}\n\nfunc Unchanged() {}\n",
			"pkg/a_test.go": "//go:build integration\n\npackage p\n\nfunc TestX() { Changed() }\n",
		},
	}
	var out bytes.Buffer
	if code := run(nil, &out, git.run); code != 0 {
		t.Errorf("run = %d, want 0 for a hit: %s", code, out.String())
	}
	want := "integrationrefs: judged 1 changed declaration(s) and 1 integration-tagged file(s)\n"
	if out.String() != want {
		t.Errorf("report = %q, want %q", out.String(), want)
	}
}

// TestRunFailsWhenGitCannotBeRead: a git failure exits 1 and names the cause,
// and the output git returned alongside the failure is never printed.
func TestRunFailsWhenGitCannotBeRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		git  *fakeGit
	}{
		{"the diff fails", &fakeGit{diffErr: errors.New("exit status 128")}},
		{"the grep for tagged tests fails", &fakeGit{grepErr: errors.New("exit status 128")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.git.diff = "SECRET-GIT-OUTPUT"
			var out bytes.Buffer
			if code := run(nil, &out, tt.git.run); code != 1 {
				t.Errorf("run = %d, want 1 when git fails", code)
			}
			if !strings.Contains(out.String(), "cannot judge the branch: ") || !strings.Contains(out.String(), "exit status 128") {
				t.Errorf("report = %q, want it to name the failure and its cause", out.String())
			}
			if strings.Contains(out.String(), "SECRET-GIT-OUTPUT") {
				t.Errorf("report = %q, want git's output kept out of the error", out.String())
			}
			if strings.Contains(out.String(), "judged") {
				t.Errorf("report = %q, want no summary when the branch was not judged", out.String())
			}
		})
	}
}

// TestRunRefusesAnEmptyBase: an empty base would read as an empty range and pass
// a branch nothing judged, so the command refuses it before git runs.
func TestRunRefusesAnEmptyBase(t *testing.T) {
	t.Parallel()
	git := &fakeGit{}
	var out bytes.Buffer
	if code := run([]string{"-base", ""}, &out, git.run); code != 2 {
		t.Errorf("run = %d, want 2 for an empty base", code)
	}
	if git.ran != 0 {
		t.Error("git ran despite the empty base")
	}
}

// TestRunRefusesABadFlag: a flag the command cannot read exits 2 without reading
// git at all.
func TestRunRefusesABadFlag(t *testing.T) {
	t.Parallel()
	git := &fakeGit{}
	var out bytes.Buffer
	if code := run([]string{"-nope"}, &out, git.run); code != 2 {
		t.Errorf("run = %d, want 2 for an unreadable flag", code)
	}
	if git.ran != 0 {
		t.Error("git ran despite the bad flag")
	}
}
