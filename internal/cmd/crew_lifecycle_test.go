package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// --resume=<value> names a session ID. A crew member's name there is rejected;
// a session ID, or a name that matches no member, passes.
func TestCheckCrewResumeTarget(t *testing.T) {
	t.Parallel()

	workers := []*crew.CrewWorker{{Name: "max"}, {Name: "emma"}}
	tests := []struct {
		name    string
		resume  string
		wantErr bool
	}{
		{"crew member name is rejected", "max", true},
		{"session ID passes", "abc123", false},
		{"name matching no member passes", "joe", false},
		{"name matching a later member is rejected", "emma", true},
		{"name differing only in case passes", "Max", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCrewResumeTarget(tt.resume, workers)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkCrewResumeTarget(%q) error = %v, wantErr %v", tt.resume, err, tt.wantErr)
			}
		})
	}
}

// A bare --resume must mean "last" on the real command: runCrewStart's guard
// relies on it, since the space form is read as a positional crew name.
func TestCrewStartResumeNoOptDefValIsLast(t *testing.T) {
	t.Parallel()

	if got := crewStartCmd.Flags().Lookup("resume").NoOptDefVal; got != "last" {
		t.Fatalf("crewStartCmd --resume NoOptDefVal = %q, want %q", got, "last")
	}
}

// checkCrewResumeFlag rejects a crew member's name given as a session ID, and
// a failed listing lets the start through.
func TestCheckCrewResumeFlag(t *testing.T) {
	t.Parallel()

	workers := []*crew.CrewWorker{{Name: "max"}, {Name: "last"}}
	listErr := errors.New("listing crew: disk gone")
	ok := func() ([]*crew.CrewWorker, error) { return workers, nil }
	failed := func() ([]*crew.CrewWorker, error) { return nil, listErr }

	tests := []struct {
		name    string
		resume  string
		list    func() ([]*crew.CrewWorker, error)
		wantErr bool
	}{
		{"empty value is not checked", "", ok, false},
		{"bare resume is not checked, even for a member named last", "last", ok, false},
		{"crew member name is rejected", "max", ok, true},
		{"session ID passes", "abc123", ok, false},
		{"listing failure fails open for a member name", "max", failed, false},
		{"listing failure fails open for a session ID", "abc123", failed, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCrewResumeFlag(tt.resume, tt.list)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkCrewResumeFlag(%q) error = %v, wantErr %v", tt.resume, err, tt.wantErr)
			}
		})
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
