package main

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

func TestTargetAnswers(t *testing.T) {
	t.Parallel()
	doctorErr := errors.New("kubectl cannot list the daemon pod")
	tests := []struct {
		name    string
		prepare func(h *harness)
		wantErr string
		// wantReached is whether configuredTarget opened and doctorAnswers asked.
		wantReached bool
	}{
		{"holding cluster target answers", func(h *harness) {
			h.writeConfig(t, kubernetesConfig)
			h.fake.diag = gtwrap.Diagnosis{Rung: gtwrap.RungExec}
		}, "", true},
		{"down target names the doctor's reason", func(h *harness) {
			h.writeConfig(t, kubernetesConfig)
			h.fake.diag = gtwrap.Diagnosis{Rung: gtwrap.RungDown, Reason: "the daemon pod is not Running"}
		}, "the daemon pod is not Running", true},
		{"doctor failure names its reason", func(h *harness) {
			h.writeConfig(t, kubernetesConfig)
			h.fake.doctorErr = doctorErr
		}, doctorErr.Error(), true},
		{"open failure names its reason without asking the doctor", func(h *harness) {
			h.surface.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
		}, "read the target config file", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			tt.prepare(h)
			err := targetAnswers(h.surface, "om")
			if tt.wantErr == "" && err != nil {
				t.Fatalf("targetAnswers = %v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("targetAnswers = %v, want an error containing %q", err, tt.wantErr)
			}
			if h.kubeBuilt != tt.wantReached || h.fake.doctored != tt.wantReached {
				t.Fatalf("opened = %v, asked the doctor = %v, want %v", h.kubeBuilt, h.fake.doctored, tt.wantReached)
			}
			if tt.wantReached && h.kubeTool != "om" {
				t.Fatalf("opened the target for tool %q, want om", h.kubeTool)
			}
		})
	}
}
