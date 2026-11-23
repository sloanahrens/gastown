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

// diffCmd is ReadDiff's command with origin/main as the base, written out so a test passes only while it is pinned.
const (
	diffCmd  = "-c core.quotePath=false diff --no-color --no-ext-diff --unified=0 --src-prefix=a/ --dst-prefix=b/ --no-renames --end-of-options origin/main...HEAD -- *.go"
	testSrc  = "package x\n\nfunc F(t *testing.T) {\n\t<-ch\n}\n"
	cleanSrc = "package x\n\nfunc F(t *testing.T) {\n\tx := 1\n}\n"
)

// fakeGit answers the branch diff and each HEAD file read from canned content, so the unit tier runs no process.
type fakeGit struct {
	diff  string
	files map[string]string
	err   error
}

func (f fakeGit) run(args []string) ([]byte, error) {
	line := strings.Join(args, " ")
	if line == diffCmd {
		if f.err != nil {
			return nil, f.err
		}
		return []byte(f.diff), nil
	}
	path, ok := strings.CutPrefix(line, "show HEAD:")
	if !ok {
		return nil, fmt.Errorf("unexpected git command %q", line)
	}
	src, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("no canned content for %s", path)
	}
	return []byte(src), nil
}

// diffAdding writes the -U0 diff that adds text as line n of path, as git writes a pure insertion.
func diffAdding(path, text string, n int) string {
	return fmt.Sprintf("diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -%d,0 +%d,1 @@\n+%s\n", path, path, path, path, n-1, n, text)
}

// TestRun judges the command's exit code and report for a finding, a clean branch, and each way it cannot judge the branch (gt-4ndz7z).
func TestRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		git       fakeGit
		wantCode  int
		wantOut   []string
		wantNoOut string
		wantGit   bool
	}{
		{"an added wait with no bound fails and names the wait", nil, fakeGit{diff: diffAdding("x_test.go", "\t<-ch", 4), files: map[string]string{"x_test.go": testSrc}}, 1,
			[]string{"judged 1 added test line(s), 1 with a wait with no bound", "x_test.go:4: added wait with no bound: <-ch", "bound each wait"}, "", true},
		{"an added test line with no wait passes and says what it judged", nil, fakeGit{diff: diffAdding("x_test.go", "\tx := 1", 4), files: map[string]string{"x_test.go": cleanSrc}}, 0,
			[]string{"judged 1 added test line(s), 0 with a wait with no bound"}, "", true},
		{"a branch that adds no test line passes and says nothing was judged", nil, fakeGit{diff: diffAdding("x.go", "\t<-ch", 4)}, 0,
			[]string{"judged 0 added test line(s)"}, "", true},
		{"a branch git cannot read fails as unread, not as a wait", nil, fakeGit{err: errors.New("exit status 128")}, 1,
			[]string{"cannot read the branch", "git diff origin/main...HEAD"}, "added wait with no bound:", true},
		{"a test file the check cannot read fails it", nil, fakeGit{diff: diffAdding("x_test.go", "\t<-ch", 4)}, 1, []string{"reading x_test.go"}, "", true},
		{"an empty base is refused before git runs", []string{"-base", ""}, fakeGit{}, 2, []string{"-base names the ref"}, "", false},
		{"a flag it cannot read is refused before git runs", []string{"-nope"}, fakeGit{}, 2, nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			var out bytes.Buffer
			code := run(tt.args, &out, func(args []string) ([]byte, error) {
				called = true
				return tt.git.run(args)
			})
			if code != tt.wantCode {
				t.Errorf("run = %d, want %d: %s", code, tt.wantCode, out.String())
			}
			if called != tt.wantGit {
				t.Errorf("git ran = %v, want %v", called, tt.wantGit)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("report = %q, want it to contain %q", out.String(), want)
				}
			}
			if tt.wantNoOut != "" && strings.Contains(out.String(), tt.wantNoOut) {
				t.Errorf("report = %q, want it not to contain %q", out.String(), tt.wantNoOut)
			}
		})
	}
}
