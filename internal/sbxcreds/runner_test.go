package sbxcreds

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProgram writes a shell script under dir that runs body and returns its path. ExecRunner's
// contract is a real program's stdout and exit status, so the fake must be an executable the kernel runs.
func fakeProgram(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	//testpolicy:allow no-exec-files — ExecRunner's contract is a real program's stdout and exit status, so the fake must be a script the kernel runs
	script := []byte("#!/bin/sh\n" + body + "\n")
	//testpolicy:allow no-exec-files — the fake is made executable on purpose, for the reason above
	if err := os.WriteFile(path, script, 0o700); err != nil {
		t.Fatalf("writing fake program %s: %v", path, err)
	}
	return path
}

// TestExecRunner fixes ExecRunner's outcomes: stdout on success; an error naming the program when it
// cannot start; an error naming the program and its status, and no output, on a non-zero exit; and
// an error with no output for an empty argv.
func TestExecRunner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const marker = "KUBECONFIG-MARKER"
	ok := fakeProgram(t, dir, "ok", "printf 'kubeconfig-body'")
	failing := fakeProgram(t, dir, "failing", "printf '"+marker+"'\nprintf '"+marker+"' >&2\nexit 7")
	absent := filepath.Join(dir, "absent")
	tests := []struct {
		name    string
		argv    []string
		wantOut string
		wantErr []string // substrings the error must name; empty when the run must succeed
	}{
		{name: "success returns stdout", argv: []string{ok}, wantOut: "kubeconfig-body"},
		{name: "program that cannot start is named", argv: []string{absent}, wantErr: []string{absent}},
		{name: "empty argv is refused", argv: nil, wantErr: []string{"no program"}},
		{name: "non-zero exit names program and status", argv: []string{failing}, wantErr: []string{failing, "exit status 7"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ExecRunner(context.Background(), tc.argv)
			if len(tc.wantErr) == 0 {
				if err != nil || string(out) != tc.wantOut {
					t.Errorf("ExecRunner(%q) = %q, %v, want %q, nil", tc.argv, out, err, tc.wantOut)
				}
				return
			}
			if err == nil {
				t.Fatalf("ExecRunner(%q) = %q, nil, want an error naming %q", tc.argv, out, tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("error %q carries the program's output, want none", err)
			}
			if out != nil {
				t.Errorf("ExecRunner(%q) returned output %q with an error, want none", tc.argv, out)
			}
		})
	}
}
