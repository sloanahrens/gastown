package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doctor"
)

// TestPodTownRefusal: mac-town-state needs the pod claim's listing, so a run that
// names it without one is refused; every other run is not affected.
func TestPodTownRefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		names   []string
		podTown string
		wantErr bool
	}{
		{name: "named without listing", names: []string{doctor.MacTownStateCheckName}, wantErr: true},
		{name: "named beside others without listing", names: []string{"daemon", doctor.MacTownStateCheckName}, wantErr: true},
		{name: "named with listing", names: []string{doctor.MacTownStateCheckName}, podTown: "pod.tsv"},
		{name: "other check without listing", names: []string{"daemon"}},
		{name: "no check named", names: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := podTownRefusal(tc.names, tc.podTown)
			if (err != nil) != tc.wantErr {
				t.Fatalf("podTownRefusal(%v, %q) = %v, wantErr %v", tc.names, tc.podTown, err, tc.wantErr)
			}
		})
	}
}

// TestRunDoctorRefusesMacTownStateWithoutListing: runDoctor refuses the named check
// before it builds the doctor, so a run without a listing cannot report clean.
//
//testpolicy:allow parallel — runDoctor reads the --check and --pod-town flag variables and the working directory, all process-wide
func TestRunDoctorRefusesMacTownStateWithoutListing(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-chdir — the cwd is the input under test: runDoctor finds the town from the working directory
	t.Chdir(townRoot)

	savedCheck, savedPodTown := doctorCheck, doctorPodTown
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the flag variables swapped below
		doctorCheck, doctorPodTown = savedCheck, savedPodTown
	})
	//testpolicy:allow no-global-swap — runDoctor reads the --check and --pod-town flag variables; they are not arguments
	doctorCheck, doctorPodTown = []string{doctor.MacTownStateCheckName}, ""

	err := runDoctor(doctorCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "needs --pod-town") {
		t.Fatalf("runDoctor = %v, want the --pod-town refusal", err)
	}
}

// TestRunDoctorMacTownStateComparesTheListing runs mac-town-state through runDoctor
// over a Mac town and a listing file, with no cluster: a claim that holds the file
// passes, and a claim that lacks it fails the run.
//
//testpolicy:allow parallel — runDoctor reads the --check and --pod-town flag variables and the working directory, all process-wide
func TestRunDoctorMacTownStateComparesTheListing(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	townJSON := filepath.Join(townRoot, "mayor", "town.json")
	if err := os.WriteFile(townJSON, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-chdir — the cwd is the input under test: runDoctor finds the town from the working directory
	t.Chdir(townRoot)

	sum := sha256.Sum256([]byte("{}"))
	present := fmt.Sprintf("%s\t%d\tmayor/town.json\n", hex.EncodeToString(sum[:]), time.Now().Unix())
	cases := []struct {
		name    string
		listing string
		wantErr bool
	}{
		{name: "claim holds the file", listing: present},
		{name: "claim lacks the file", listing: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listing := filepath.Join(t.TempDir(), "pod-town.tsv")
			if err := os.WriteFile(listing, []byte(tc.listing), 0o644); err != nil {
				t.Fatal(err)
			}
			savedCheck, savedPodTown := doctorCheck, doctorPodTown
			t.Cleanup(func() {
				//testpolicy:allow no-global-swap — restores the flag variables swapped below
				doctorCheck, doctorPodTown = savedCheck, savedPodTown
			})
			//testpolicy:allow no-global-swap — runDoctor reads the --check and --pod-town flag variables; they are not arguments
			doctorCheck, doctorPodTown = []string{doctor.MacTownStateCheckName}, listing

			err := runDoctor(doctorCmd, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("runDoctor = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
