package cmd

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
)

// fakePodExec stands in for the production podExec: it records the argv and stdin
// of each call and answers with the stdout, exit status and error it was given.
type fakePodExec struct {
	stdout []byte
	exit   int
	err    error
	argv   []string
	stdin  []byte
}

func (f *fakePodExec) exec(_ context.Context, argv []string, stdin []byte) ([]byte, int, error) {
	f.argv, f.stdin = argv, stdin
	return f.stdout, f.exit, f.err
}

func TestPodRunnerArgvAndStdin(t *testing.T) {
	t.Parallel()
	fake := &fakePodExec{stdout: []byte("created\n")}
	out, err := newPodRunner(fake.exec)(context.Background(), []string{"sh", "-c", "cat", "sh", "/town/landing.hold"}, []byte("owner=roll\n"))
	if err != nil {
		t.Fatalf("running in the pod: %v", err)
	}
	want := []string{"kubectl", "exec", "-i", "deploy/daemon", "-c", "daemon", "-n", "gt", "--", "sh", "-c", "cat", "sh", "/town/landing.hold"}
	if !slices.Equal(fake.argv, want) {
		t.Errorf("pod argv = %q, want %q", fake.argv, want)
	}
	if !bytes.Equal(fake.stdin, []byte("owner=roll\n")) {
		t.Errorf("stdin = %q, want the bytes the runner was given", fake.stdin)
	}
	if string(out) != "created\n" {
		t.Errorf("stdout = %q, want the script's reply", out)
	}
}

// TestPodRunnerFailuresCarryNoOutput covers each refusal and failure. A refusal
// that ran the pod exec would answer the reply with exit 0 and no error, so the
// "want an error" check also proves the refusal comes before the run.
func TestPodRunnerFailuresCarryNoOutput(t *testing.T) {
	t.Parallel()
	const reply = "SECRET-REPLY"
	tests := []struct {
		name string
		exec podExec
		argv []string
	}{
		{"non-zero exit", (&fakePodExec{stdout: []byte(reply), exit: 1}).exec, []string{"sh", "-c", "true"}},
		{"start failure", (&fakePodExec{stdout: []byte(reply), err: errors.New(`exec: "kubectl": executable file not found`)}).exec, []string{"sh", "-c", "true"}},
		{"no pod exec is wired", nil, []string{"true"}},
		{"no argv to run", (&fakePodExec{stdout: []byte(reply)}).exec, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := newPodRunner(tc.exec)(context.Background(), tc.argv, nil)
			if err == nil {
				t.Fatal("want an error, got none")
			}
			if out != nil {
				t.Errorf("stdout = %q, want none on a failed run", out)
			}
			if bytes.Contains([]byte(err.Error()), []byte(reply)) {
				t.Errorf("error %q echoes the script's output", err)
			}
		})
	}
}
