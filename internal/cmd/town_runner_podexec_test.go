package cmd

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
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
		wantOut    string
		wantErr    string
		wantStatus int
	}{
		{name: "success", ctx: t.Context(), wantOut: "out"},
		{name: "context done", ctx: done, err: errors.New("killed"), wantErr: "running kubectl: context canceled"},
		{name: "signalled child", ctx: t.Context(), err: &exec.ExitError{}, wantErr: "running kubectl"},
		{name: "cannot start", ctx: t.Context(), err: errors.New("not found"), wantErr: "starting kubectl: not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, status, err := podExecOutcome(tt.ctx, "kubectl", []byte("out"), tt.err)
			if tt.wantErr == "" {
				if err != nil || string(out) != tt.wantOut || status != tt.wantStatus {
					t.Fatalf("podExecOutcome = %q, %d, %v; want %q, %d, no error", out, status, err, tt.wantOut, tt.wantStatus)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || out != nil || status != 0 {
				t.Fatalf("podExecOutcome = %q, %d, %v; want no output, status 0 and an error containing %q", out, status, err, tt.wantErr)
			}
		})
	}
}
