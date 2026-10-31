package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// fakeRunner answers git and go list from canned output and records each argv,
// so the command is tested without starting a process. The two git diffs are
// told apart by the range: the first names base...head, the second the head the
// working tree is compared against.
type fakeRunner struct {
	diff      []byte
	treeDiff  []byte
	untracked []byte
	list      []byte
	calls     [][]string

	listErr error
}

func (f *fakeRunner) run(argv []string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	if argv[0] == "go" {
		return f.list, f.listErr
	}
	if argv[0] != "git" || len(argv) < 2 {
		return nil, fmt.Errorf("fake runner: unexpected command %q", argv)
	}
	switch argv[1] {
	case "diff":
		if strings.Contains(argv[len(argv)-1], "...") {
			return f.diff, nil
		}
		return f.treeDiff, nil
	case "ls-files":
		return f.untracked, nil
	}
	return nil, fmt.Errorf("fake runner: unexpected git command %q", argv[1])
}

// names renders `git diff --name-only -z` output: every path, NUL-terminated.
func names(files ...string) []byte {
	var out []byte
	for _, file := range files {
		out = append(out, file...)
		out = append(out, 0)
	}
	return out
}

// moduleRoot is the directory the fixture stream names as the module root.
const moduleRoot = "/repo"

// onePackage is a go list stream for the single package internal/a.
const onePackage = `{
	"ImportPath": "example.com/m/internal/a",
	"Dir": "/repo/internal/a",
	"Deps": ["fmt"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
`

func TestAffectedPrintsTheSelection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		args      []string
		wd        string // the directory the command runs in; empty means moduleRoot
		wdErr     error
		diff      []byte
		treeDiff  []byte
		untracked []byte
		list      []byte
		listErr   error
		wantOut   string
		wantCode  int
		wantErr   string
		wantCalls int
	}{
		{
			name:      "one package",
			diff:      names("internal/a/a.go"),
			list:      []byte(onePackage),
			wantOut:   "example.com/m/internal/a\n",
			wantCode:  0,
			wantCalls: 4,
		},
		{
			name:      "empty diff prints NONE",
			wantOut:   "NONE\n",
			wantCode:  0,
			wantCalls: 1,
		},
		{
			name:      "build input widens without reading the graph",
			diff:      names("go.sum"),
			wantOut:   "ALL\n",
			wantCode:  0,
			wantCalls: 1,
		},
		{
			name:      "unreadable graph widens",
			diff:      names("internal/a/a.go"),
			listErr:   errors.New("go list failed"),
			wantOut:   "ALL\n",
			wantCode:  0,
			wantErr:   "go list",
			wantCalls: 4,
		},
		{
			name:      "started below the module root widens instead of narrowing",
			wd:        "/repo/internal/a",
			diff:      names("internal/a/a.go"),
			list:      []byte(onePackage),
			wantOut:   "ALL\n",
			wantCode:  0,
			wantErr:   "module root",
			wantCalls: 4,
		},
		{
			name:      "working tree outside head widens instead of narrowing",
			diff:      names("internal/a/a.go"),
			treeDiff:  names("internal/a/a.go"),
			list:      []byte(onePackage),
			wantOut:   "ALL\n",
			wantCode:  0,
			wantErr:   "working tree",
			wantCalls: 2,
		},
		{
			name:      "unreadable working directory widens",
			wdErr:     errors.New("getwd failed"),
			wantOut:   "ALL\n",
			wantCode:  0,
			wantErr:   "working directory",
			wantCalls: 0,
		},
		{
			name:     "unexpected argument",
			args:     []string{"extra"},
			wantCode: 2,
			wantErr:  "unexpected argument",
		},
		{
			name:     "unknown flag",
			args:     []string{"-nosuchflag"},
			wantCode: 2,
			wantErr:  "flag provided but not defined",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{diff: tc.diff, treeDiff: tc.treeDiff, untracked: tc.untracked, list: tc.list, listErr: tc.listErr}
			wd := tc.wd
			if wd == "" {
				wd = moduleRoot
			}
			getwd := func() (string, error) { return wd, nil }
			if tc.wdErr != nil {
				getwd = func() (string, error) { return "", tc.wdErr }
			}
			var out, errw bytes.Buffer
			code := run(tc.args, getwd, &out, &errw, r.run)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			if out.String() != tc.wantOut {
				t.Errorf("stdout = %q, want %q", out.String(), tc.wantOut)
			}
			if tc.wantErr == "" {
				if errw.Len() != 0 {
					t.Errorf("stderr = %q, want nothing", errw.String())
				}
			} else if !strings.Contains(errw.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want it to name %q", errw.String(), tc.wantErr)
			}
			if len(r.calls) != tc.wantCalls {
				t.Errorf("ran %d commands, want %d", len(r.calls), tc.wantCalls)
			}
		})
	}
}
