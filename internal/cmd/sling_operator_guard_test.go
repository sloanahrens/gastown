package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// TestSlingRejectsOperatorReservedBead pins gt-21pl0 at the CLI: a bead the
// human operator owns — labelled `operator`, or assigned to a person rather
// than to an agent address — is not an agent's to take, so gt sling refuses it
// instead of spending a polecat seat on it.
//
// The refusal must also carry dispatch.SlingRefusalMarker. Automatic
// dispatchers run sling as a subprocess and read that string to tell "this
// bead is not yours" from "the sling broke"; without it the daemon's convoy
// feeder would count the refusal as a failed dispatch.
func TestSlingRejectsOperatorReservedBead(t *testing.T) {
	tests := []struct {
		name     string
		bdOutput string
		force    bool
		wantErr  bool
	}{
		{
			name:     "operator label is refused",
			bdOutput: `[{"title":"Hand-run audit","status":"open","assignee":"","description":"some task","labels":["operator"]}]`,
			wantErr:  true,
		},
		{
			name:     "operator label on an agent-held bead is refused",
			bdOutput: `[{"title":"Hand-run audit","status":"open","assignee":"gastown/polecats/onyx","description":"some task","labels":["operator"]}]`,
			wantErr:  true,
		},
		{
			name:     "human assignee is refused",
			bdOutput: `[{"title":"Operator task","status":"open","assignee":"sloan","description":"some task"}]`,
			wantErr:  true,
		},
		{
			name:     "named human assignee is refused",
			bdOutput: `[{"title":"Operator task","status":"open","assignee":"Sloan Ahrens","description":"some task"}]`,
			wantErr:  true,
		},
		{
			name:     "agent assignee is allowed",
			bdOutput: `[{"title":"Agent task","status":"open","assignee":"gastown/polecats/onyx","description":"some task"}]`,
		},
		{
			name:     "crew assignee is allowed",
			bdOutput: `[{"title":"Crew task","status":"open","assignee":"gastown/crew/sloan","description":"some task"}]`,
		},
		{
			name:     "unassigned, unlabelled bead is allowed",
			bdOutput: `[{"title":"Normal work","status":"open","assignee":"","description":"just a regular task"}]`,
		},
		{
			name:     "--force takes the bead anyway",
			bdOutput: `[{"title":"Hand-run audit","status":"open","assignee":"sloan","description":"some task","labels":["operator"]}]`,
			force:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot := t.TempDir()
			if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			binDir := filepath.Join(townRoot, "bin")
			if err := os.MkdirAll(binDir, 0755); err != nil {
				t.Fatalf("mkdir bin: %v", err)
			}
			bdScript := "#!/bin/sh\necho '" + tt.bdOutput + "'\n"
			bdScriptWindows := "@echo off\r\necho " + tt.bdOutput + "\r\n"
			writeBDStub(t, binDir, bdScript, bdScriptWindows)

			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv(EnvGTRole, "crew")
			t.Setenv("GT_CREW", "jv")
			t.Setenv("GT_POLECAT", "")
			t.Setenv("TMUX_PANE", "")
			t.Setenv("GT_TEST_NO_NUDGE", "1")

			cwd, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			t.Cleanup(func() { _ = os.Chdir(cwd) })
			if err := os.Chdir(townRoot); err != nil {
				t.Fatalf("chdir: %v", err)
			}

			prevDryRun, prevNoConvoy, prevForce := slingDryRun, slingNoConvoy, slingForce
			t.Cleanup(func() {
				slingDryRun, slingNoConvoy, slingForce = prevDryRun, prevNoConvoy, prevForce
			})
			slingDryRun = true
			slingNoConvoy = true
			slingForce = tt.force

			err = runSling(nil, []string{"gt-test123"})

			refusal := err != nil && strings.Contains(err.Error(), dispatch.SlingRefusalMarker)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected the operator reservation to refuse the sling, got nil")
				}
				if !refusal {
					t.Fatalf("refusal must carry %q so an automatic dispatcher defers it, got: %v",
						dispatch.SlingRefusalMarker, err)
				}
				if !strings.Contains(err.Error(), "is the operator's work") {
					t.Fatalf("refusal should name the reservation, got: %v", err)
				}
				return
			}
			if refusal {
				t.Fatalf("guard refused a bead it should not have: %v", err)
			}
		})
	}
}

// TestExecuteSlingRejectsOperatorReservedBead pins the same guard on the
// programmatic entry point (batch sling, queue dispatch, the daemon's convoy
// feeders). It exists separately from the runSling test because the two paths
// carry their own copy of every bead guard, and one without the other leaves
// the schedulers able to dispatch work the CLI refuses.
func TestExecuteSlingRejectsOperatorReservedBead(t *testing.T) {
	tests := []struct {
		name       string
		bdOutput   string
		force      bool
		wantErrMsg string
	}{
		{
			name:       "operator label is refused",
			bdOutput:   `[{"title":"Hand-run audit","status":"open","assignee":"","description":"","labels":["operator"]}]`,
			wantErrMsg: "operator-reserved",
		},
		{
			name:       "human assignee is refused",
			bdOutput:   `[{"title":"Operator task","status":"open","assignee":"sloan","description":""}]`,
			wantErrMsg: "operator-reserved",
		},
		{
			name:     "agent assignee is allowed",
			bdOutput: `[{"title":"Agent task","status":"open","assignee":"gastown/polecats/onyx","description":""}]`,
		},
		{
			name:     "--force takes the bead anyway",
			bdOutput: `[{"title":"Hand-run audit","status":"open","assignee":"sloan","description":"","labels":["operator"]}]`,
			force:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot := t.TempDir()
			if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
				t.Fatalf("failed to create .beads: %v", err)
			}
			if err := os.MkdirAll(filepath.Join(townRoot, "mayor", "rig"), 0o755); err != nil {
				t.Fatalf("mkdir mayor/rig: %v", err)
			}

			binDir := filepath.Join(townRoot, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("mkdir binDir: %v", err)
			}
			bdScript := "#!/bin/sh\necho '" + tt.bdOutput + "'\n"
			bdScriptWindows := "@echo off\r\necho " + tt.bdOutput + "\r\n"
			writeBDStub(t, binDir, bdScript, bdScriptWindows)

			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			result, err := executeSling(SlingParams{
				BeadID:   "test-operator1",
				RigName:  "testrig",
				TownRoot: townRoot,
				Force:    tt.force,
			})

			if tt.wantErrMsg == "" {
				if err != nil && strings.Contains(err.Error(), dispatch.SlingRefusalMarker) {
					t.Fatalf("guard refused a bead it should not have: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected the operator reservation to refuse the dispatch, got nil")
			}
			if !strings.Contains(err.Error(), dispatch.SlingRefusalMarker) {
				t.Fatalf("refusal must carry %q, got: %v", dispatch.SlingRefusalMarker, err)
			}
			if result.ErrMsg != tt.wantErrMsg {
				t.Errorf("ErrMsg = %q, want %q", result.ErrMsg, tt.wantErrMsg)
			}
		})
	}
}
