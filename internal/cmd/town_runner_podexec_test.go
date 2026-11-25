package cmd

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestProductionPodExecRefusesAnEmptyArgv: no argv is an error before anything runs.
func TestProductionPodExecRefusesAnEmptyArgv(t *testing.T) {
	t.Parallel()
	if _, _, err := productionPodExec(t.Context(), nil, nil); err == nil {
		t.Error("an empty argv gave no error")
	}
}

// TestPodExecOutcome drives each way a child ends without starting one: the error
// is the value the run would have returned. A nil ProcessState reads as exit -1,
// which is what a child killed by a signal reports.
func TestPodExecOutcome(t *testing.T) {
	t.Parallel()
	done, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name       string
		ctx        context.Context
		err        error
		stderr     string
		status     int
		wantOut    string
		wantErr    string
		wantStatus int
	}{
		{name: "success", ctx: t.Context(), stderr: "noise", status: -1, wantOut: "out"},
		{name: "exit status", ctx: t.Context(), err: errors.New("exit status 3"), status: 3, stderr: "refused", wantOut: "out", wantErr: "kubectl exited 3: refused", wantStatus: 3},
		{name: "context done", ctx: done, err: errors.New("killed"), status: -1, wantErr: "running kubectl: context canceled"},
		{name: "signalled child", ctx: t.Context(), err: &exec.ExitError{}, status: -1, wantErr: "running kubectl"},
		{name: "cannot start", ctx: t.Context(), err: errors.New("not found"), status: -1, wantErr: "starting kubectl: not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, status, err := podExecOutcome(tt.ctx, "kubectl", []byte("out"), []byte(tt.stderr), tt.status, tt.err)
			if tt.wantErr == "" {
				if err != nil || string(out) != tt.wantOut || status != tt.wantStatus {
					t.Fatalf("podExecOutcome = %q, %d, %v; want %q, %d, no error", out, status, err, tt.wantOut, tt.wantStatus)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || string(out) != tt.wantOut || status != tt.wantStatus {
				t.Fatalf("podExecOutcome = %q, %d, %v; want %q, status %d and an error containing %q", out, status, err, tt.wantOut, tt.wantStatus, tt.wantErr)
			}
		})
	}
}

// TestStderrTail covers the bounded, scrubbed tail an error carries: the last 2048
// bytes, cut on a character boundary, with a credential-shaped run replaced and
// nothing for an empty stderr.
func TestStderrTail(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 8000) + "END"
	if got := stderrTail([]byte(long)); len(got) != podExecStderrTail || !strings.HasSuffix(got, "END") {
		t.Errorf("a 8003-byte stderr gave %d bytes ending %q, want its last %d", len(got), got[len(got)-3:], podExecStderrTail)
	}
	multi := strings.Repeat("é", 3000)
	if got := stderrTail([]byte(multi)); !utf8.ValidString(got) || len(got) > podExecStderrTail || len(got) < podExecStderrTail-utf8.UTFMax {
		t.Errorf("a multi-byte stderr gave %d bytes, valid %v; want at most %d and a whole character at the cut", len(got), utf8.ValidString(got), podExecStderrTail)
	}
	if got := stderrTail([]byte("failed\ntoken=abc123 now")); got != "failed [redacted] now" {
		t.Errorf("a token-shaped run gave %q, want it scrubbed and the line break a space", got)
	}
	if got := stderrTail(nil); got != "" {
		t.Errorf("an empty stderr gave %q, want none", got)
	}
}
