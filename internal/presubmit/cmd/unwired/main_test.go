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

// The two commands run are written out, so a test passes only while they are pinned.
const (
	unwiredDiffCmd = "-c core.quotePath=false diff --no-color --no-ext-diff --unified=0 --src-prefix=a/ --dst-prefix=b/ --no-renames --end-of-options origin/main...HEAD -- *.go"
	unwiredListCmd = "ls-files -z --full-name -- *.go"
	declSrc        = "package x\n\nfunc Foo() {}\n"
)

// fakeGit answers the branch diff and the module file list from canned content, so the unit tier
// runs no process.
type fakeGit struct {
	diff, list       string
	diffErr, listErr error
}

func (f fakeGit) run(args []string) ([]byte, error) {
	switch line := strings.Join(args, " "); line {
	case unwiredDiffCmd:
		return []byte(f.diff), f.diffErr
	case unwiredListCmd:
		return []byte(f.list), f.listErr
	default:
		return nil, fmt.Errorf("unexpected git command %q", line)
	}
}

// diffAdding writes the -U0 diff that adds text as line n of path, as git writes a pure insertion.
func diffAdding(path, text string, n int) string {
	return fmt.Sprintf("diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -%d,0 +%d,1 @@\n+%s\n", path, path, path, path, n-1, n, text)
}

// cannedFiles answers FileReader from a map, so the check reads no file on the host.
func cannedFiles(files map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		src, ok := files[p]
		if !ok {
			return nil, fmt.Errorf("no canned content for %s", p)
		}
		return []byte(src), nil
	}
}

// TestUnwiredRun judges the command's exit code and report for a finding, a wired export, a branch
// that adds nothing to judge, and each way it cannot judge the branch (gt-46ezdn).
func TestUnwiredRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		git       fakeGit
		files     map[string]string
		wantCode  int
		wantOut   []string
		wantNoOut string
		wantGit   bool
	}{
		{"a finding still exits 0 and reports both counts", nil,
			fakeGit{diff: diffAdding("x.go", "func Foo() {}", 3), list: "x.go\x00"}, map[string]string{"x.go": declSrc}, 0,
			[]string{"judged 1 added exported function(s), 1 with no caller", "x.go:3 Foo"}, "", true},
		{"an export another file names exits 0 with no finding", nil,
			fakeGit{diff: diffAdding("x.go", "func Foo() {}", 3), list: "x.go\x00y.go\x00"},
			map[string]string{"x.go": declSrc, "y.go": "package x\n\nfunc use() { Foo() }\n"}, 0,
			[]string{"judged 1 added exported function(s), 0 with no caller"}, "x.go:3", true},
		{"a branch that adds nothing to judge says so", nil,
			fakeGit{diff: diffAdding("x_test.go", "func Foo() {}", 3), list: "x_test.go\x00"}, map[string]string{"x_test.go": declSrc}, 0,
			[]string{"judged 0 added exported function(s), 0 with no caller"}, "", true},
		{"a branch git cannot read fails as unread, not as clean", nil,
			fakeGit{diffErr: errors.New("exit status 128")}, nil, 1,
			[]string{"cannot read the branch", "git diff origin/main...HEAD"}, "judged", true},
		{"a module list git cannot read fails as unread", nil,
			fakeGit{diff: diffAdding("x.go", "func Foo() {}", 3), listErr: errors.New("exit status 128")}, map[string]string{"x.go": declSrc}, 1,
			[]string{"listing the module's Go files"}, "", true},
		{"an empty base is refused before git runs", []string{"-base", ""}, fakeGit{}, nil, 2, []string{"-base names the ref"}, "", false},
		{"a flag it cannot read is refused before git runs", []string{"-nope"}, fakeGit{}, nil, 2, nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			var out bytes.Buffer
			code := run(tt.args, &out, func(args []string) ([]byte, error) {
				called = true
				return tt.git.run(args)
			}, cannedFiles(tt.files), nil)
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
