package main

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// fakeRunner answers git and go list from canned output and records each argv,
// so the command is tested without starting a process.
type fakeRunner struct {
	diff    []byte
	list    []byte
	listErr error
	calls   [][]string
}

func (f *fakeRunner) run(argv []string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	switch argv[0] {
	case "git":
		return f.diff, nil
	case "go":
		return f.list, f.listErr
	}
	return nil, errors.New("fake runner: unexpected program")
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
		diff      []byte
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
			wantCalls: 2,
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
			wantCalls: 2,
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
			r := &fakeRunner{diff: tc.diff, list: tc.list, listErr: tc.listErr}
			var out, errw bytes.Buffer
			code := run(tc.args, &out, &errw, r.run)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			if out.String() != tc.wantOut {
				t.Errorf("stdout = %q, want %q", out.String(), tc.wantOut)
			}
			if !strings.Contains(errw.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want it to name %q", errw.String(), tc.wantErr)
			}
			if len(r.calls) != tc.wantCalls {
				t.Errorf("ran %d commands, want %d", len(r.calls), tc.wantCalls)
			}
		})
	}
}
