package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/rig"
)

// TestCrewRemoveTmuxErrorKeepsWorkspace: a has-session that fails for a reason
// tmux does not recognise as "no session" says nothing about whether the
// member's session is live, so the removal stops there, with or without --force.
func TestCrewRemoveTmuxErrorKeepsWorkspace(t *testing.T) {
	t.Parallel()

	tmuxErr := errors.New("tmux: unexpected failure talking to the server")

	for _, force := range []bool{false, true} {
		name := "without --force"
		if force {
			name = "with --force"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rigPath := t.TempDir()
			crewPath := filepath.Join(rigPath, "crew", "max")
			if err := os.MkdirAll(crewPath, 0755); err != nil {
				t.Fatalf("mkdir crew workspace: %v", err)
			}
			env := crewRemoveEnv{
				member: func(rigOverride, name string) (*crew.Manager, *rig.Rig, error) {
					return nil, &rig.Rig{Name: "gastown", Path: rigPath}, nil
				},
				hasSession: func(string) (bool, error) { return false, tmuxErr },
			}

			err := runCrewRemoveWith([]string{"max"}, "gastown", force, false, env)

			if !errors.Is(err, tmuxErr) {
				t.Errorf("error = %v, want it to wrap %q", err, tmuxErr)
			}
			if _, statErr := os.Stat(crewPath); statErr != nil {
				t.Errorf("crew workspace gone after a failed session check: %v", statErr)
			}
		})
	}
}

// A running session still blocks a removal without --force, and leaves the
// workspace where it is.
func TestCrewRemoveRunningSessionKeepsWorkspace(t *testing.T) {
	t.Parallel()

	rigPath := t.TempDir()
	crewPath := filepath.Join(rigPath, "crew", "max")
	if err := os.MkdirAll(crewPath, 0755); err != nil {
		t.Fatalf("mkdir crew workspace: %v", err)
	}
	env := crewRemoveEnv{
		member: func(rigOverride, name string) (*crew.Manager, *rig.Rig, error) {
			return nil, &rig.Rig{Name: "gastown", Path: rigPath}, nil
		},
		hasSession: func(string) (bool, error) { return true, nil },
	}

	err := runCrewRemoveWith([]string{"max"}, "gastown", false, false, env)

	if err == nil || !strings.Contains(err.Error(), "session running") {
		t.Errorf("error = %v, want session running", err)
	}
	if _, statErr := os.Stat(crewPath); statErr != nil {
		t.Errorf("crew workspace gone while its session runs: %v", statErr)
	}
}

// `--resume <id>` with a space does not carry the ID: NoOptDefVal leaves it as a
// positional argument, which then reads as a crew name. This is why the help
// documents only the = form. The flag is rebuilt on a private command, with the
// NoOptDefVal checked against crewStartCmd, so the package globals stay untouched.
func TestCrewStartResumeSpaceFormIsPositional(t *testing.T) {
	t.Parallel()

	if got := crewStartCmd.Flags().Lookup("resume").NoOptDefVal; got != "last" {
		t.Fatalf("crewStartCmd --resume NoOptDefVal = %q, want %q", got, "last")
	}

	cmd := &cobra.Command{}
	var resume string
	cmd.Flags().StringVar(&resume, "resume", "", "")
	cmd.Flags().Lookup("resume").NoOptDefVal = "last"

	if err := cmd.ParseFlags([]string{"--resume", "abc123"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if resume != "last" {
		t.Errorf("resume = %q, want %q (bare --resume)", resume, "last")
	}
	if got := cmd.Flags().Args(); len(got) != 1 || got[0] != "abc123" {
		t.Errorf("positional args = %v, want [abc123]", got)
	}
}

// The help must show the = form for a session ID and never the space form.
func TestCrewStartResumeHelpUsesEqualsForm(t *testing.T) {
	t.Parallel()

	if strings.Contains(crewStartCmd.Long, "--resume abc123") {
		t.Errorf("help shows the space form, which is read as a crew name")
	}
	if !strings.Contains(crewStartCmd.Long, "--resume=abc123") {
		t.Errorf("help does not show the --resume=<session-id> form")
	}
}
