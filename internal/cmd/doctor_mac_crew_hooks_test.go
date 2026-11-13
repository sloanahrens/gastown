package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doctor"
)

// TestDoctorCheckMacCrewHooks: --check mac-crew-hooks runs without --pod-town, and
// the help names it. A town with a dangling crew clone fails the run; a town with
// no crew clones only warns, so the run passes.
//
//testpolicy:allow parallel — runDoctor reads the --check and --pod-town flag variables and the working directory, all process-wide
func TestDoctorCheckMacCrewHooks(t *testing.T) {
	if !strings.Contains(doctorCmd.Long, doctor.MacCrewHooksCheckName) {
		t.Errorf("doctor help does not name %s", doctor.MacCrewHooksCheckName)
	}

	cases := []struct {
		name    string
		setup   func(t *testing.T, town string)
		wantErr bool
	}{
		{
			name: "no crew clones warns and passes",
			setup: func(t *testing.T, town string) {
				if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "dangling crew clone fails the run",
			setup: func(t *testing.T, town string) {
				clone := filepath.Join(town, "gastown", "crew", "sloan")
				if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
				config := "[core]\n\thooksPath = /nonexistent/gt-9glye5/hooks\n"
				if err := os.WriteFile(filepath.Join(clone, ".git", "config"), []byte(config), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, townRoot)
			//testpolicy:allow no-chdir — the cwd is the input under test: runDoctor finds the town from the working directory
			t.Chdir(townRoot)

			savedCheck, savedPodTown := doctorCheck, doctorPodTown
			t.Cleanup(func() {
				//testpolicy:allow no-global-swap — restores the flag variables swapped below
				doctorCheck, doctorPodTown = savedCheck, savedPodTown
			})
			//testpolicy:allow no-global-swap — runDoctor reads the --check and --pod-town flag variables; they are not arguments
			doctorCheck, doctorPodTown = []string{doctor.MacCrewHooksCheckName}, ""

			err := runDoctor(doctorCmd, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("runDoctor = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
