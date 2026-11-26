package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
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
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-chdir — the cwd is the input under test: runDoctor finds the town from the working directory
	t.Chdir(townRoot)

	sum := sha256.Sum256([]byte("{}"))
	present := fmt.Sprintf("%s\t%d\tmayor/town.json\n", hex.EncodeToString(sum[:]), time.Now().Unix()) +
		fmt.Sprintf("%s\t%d\tsettings/config.json\n", hex.EncodeToString(sum[:]), time.Now().Unix())
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

func podTownListing(names ...string) string {
	sum := sha256.Sum256([]byte("{}"))
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s\t%d\t%s\n", hex.EncodeToString(sum[:]), time.Now().Unix(), name)
	}
	return b.String()
}

func podTownStdinTown(t *testing.T) {
	townRoot := t.TempDir()
	for _, name := range []string{"mayor/town.json", "settings/config.json"} {
		p := filepath.Join(townRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	//testpolicy:allow no-chdir — the cwd is the input under test: runDoctor finds the town from the working directory
	t.Chdir(townRoot)
}

// runPodTown runs mac-town-state through runDoctor with podTown and stdin set.
func runPodTown(t *testing.T, podTown string, stdin io.Reader) error {
	savedCheck, savedPodTown := doctorCheck, doctorPodTown
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the flag variables and standard input swapped below
		doctorCheck, doctorPodTown = savedCheck, savedPodTown
		doctorCmd.SetIn(nil)
	})
	//testpolicy:allow no-global-swap — runDoctor reads the --check and --pod-town flag variables and standard input; they are not arguments
	doctorCheck, doctorPodTown = []string{doctor.MacTownStateCheckName}, podTown
	doctorCmd.SetIn(stdin)
	return runDoctor(doctorCmd, nil)
}

// TestDoctorPodTownStdin: --pod-town - compares the piped listing; a bad one is an error.
//
//testpolicy:allow parallel — runDoctor reads process-wide flag variables, the working directory and standard input
func TestDoctorPodTownStdin(t *testing.T) {
	podTownStdinTown(t)
	cases := []struct {
		name    string
		stdin   io.Reader
		wantErr bool
	}{
		{name: "claim holds every file", stdin: strings.NewReader(podTownListing("mayor/town.json", "settings/config.json"))},
		{name: "empty input", stdin: strings.NewReader(""), wantErr: true},
		{name: "claim lacks a file", stdin: strings.NewReader(podTownListing("mayor/town.json")), wantErr: true},
		{name: "malformed line", stdin: strings.NewReader("not a listing\n"), wantErr: true},
		{name: "unreadable input", stdin: iotest.ErrReader(fmt.Errorf("read failed")), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := runPodTown(t, "-", tc.stdin); (err != nil) != tc.wantErr {
				t.Fatalf("runDoctor = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

//testpolicy:allow parallel — runDoctor reads process-wide flag variables and the working directory
func TestDoctorPodTownFile(t *testing.T) {
	podTownStdinTown(t)
	cases := []struct {
		name    string
		listing string
		wantErr bool
	}{
		{name: "claim holds every file", listing: podTownListing("mayor/town.json", "settings/config.json")},
		{name: "claim lacks a file", listing: podTownListing("mayor/town.json"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pod-town.tsv")
			if err := os.WriteFile(path, []byte(tc.listing), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := runPodTown(t, path, nil); (err != nil) != tc.wantErr {
				t.Fatalf("runDoctor = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
