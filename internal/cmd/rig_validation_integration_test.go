//go:build integration

package cmd

import (
	"os/exec"
	"strings"
	"testing"
)

func TestIntegrationRigAddURLValidation(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed, skipping integration test")
	}

	// The flags reach runRigAdd through cobra, so this drives the CLI: the
	// push/upstream URL globals it would otherwise have to be handed by
	// assignment are package-level state, which t.Parallel forbids touching.
	// The rig name and git URL are valid, so the command stops at the URL
	// check before AddRig and needs no Dolt server.
	townRoot := setupTestTown(t)
	gitURL := "https://github.com/org/repo.git"

	tests := []struct {
		name        string
		pushURL     string
		upstreamURL string
		wantErr     string
	}{
		{
			name:        "invalid push url",
			pushURL:     "/local/path",
			upstreamURL: "",
			wantErr:     "invalid push URL",
		},
		{
			name:        "invalid upstream url",
			pushURL:     "",
			upstreamURL: "not-a-url",
			wantErr:     "invalid upstream URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"rig", "add", "myrig", gitURL}
			if tt.pushURL != "" {
				args = append(args, "--push-url", tt.pushURL)
			}
			if tt.upstreamURL != "" {
				args = append(args, "--upstream-url", tt.upstreamURL)
			}

			// cmd.Dir is the town root: workspace discovery walks up from it
			// instead of this process chdir-ing into it.
			out, err := runGTCmdMayFail(t, buildGT(t), townRoot, nil, args...)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(out, tt.wantErr) {
				t.Errorf("output %q does not contain %q", out, tt.wantErr)
			}
		})
	}
}
