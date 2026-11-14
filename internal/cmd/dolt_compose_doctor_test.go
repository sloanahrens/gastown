package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doctor"
)

// TestDataUnitDoctorRegisteredByName: --check data-unit reaches the data unit
// check, so the run names it rather than refusing an unknown check. The town has
// no compose values file, so the status reader refuses before docker runs and
// the run fails on the unit rung.
//
//testpolicy:allow parallel — runDoctor reads the --check flag variable and the working directory, both process-wide
func TestDataUnitDoctorRegisteredByName(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-chdir — the cwd is the input under test: runDoctor finds the town from the working directory
	t.Chdir(townRoot)

	savedCheck := doctorCheck
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the flag variable swapped below
		doctorCheck = savedCheck
	})
	//testpolicy:allow no-global-swap — runDoctor reads the --check flag variable; it is not an argument
	doctorCheck = []string{doctor.DataUnitCheckName}

	err := runDoctor(doctorCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "doctor found 1 error(s)") {
		t.Fatalf("runDoctor(--check data-unit) = %v, want the run to fail on the data unit check", err)
	}
}

// TestDataUnitDoctorPsRunsTheComposeUnit: the data unit check reads the unit
// with docker compose ps for the gt-data project and its compose file, in the
// environment gt dolt start gives it, and returns what ps printed.
func TestDataUnitDoctorPsRunsTheComposeUnit(t *testing.T) {
	t.Parallel()
	townRoot := composeTown(t, composeRef("gastown-bd"))
	var calls []composeCall
	ps := []byte("dolt running healthy\nbd running healthy\n")
	out, err := doltComposePsFor(fakeComposeDeps(townRoot, &calls, ps, nil))(townRoot)
	if err != nil {
		t.Fatalf("status reader: %v", err)
	}
	if string(out) != string(ps) {
		t.Errorf("status reader returned %q, want what ps printed %q", out, ps)
	}
	if len(calls) != 1 {
		t.Fatalf("status reader made %d docker calls, want 1", len(calls))
	}
	argv := calls[0].argv
	file := filepath.Join(townRoot, "gastown", doltComposeRelPath)
	for _, want := range []string{"-p", doltComposeProject, "-f", file, "ps", "--all", "--format", doctor.DataUnitPsFormat} {
		if !composeArgvHas(argv, want) {
			t.Errorf("argv %q does not carry %q", argv, want)
		}
	}
	if got, _ := lastEnv(calls[0].env, "BD_IMAGE"); got != composeRef("gastown-bd") {
		t.Errorf("BD_IMAGE = %q, want the values file's bd image", got)
	}
	if got, _ := lastEnv(calls[0].env, "GT_BD_TOKEN_DIR"); got != "/tokens" {
		t.Errorf("GT_BD_TOKEN_DIR = %q, want the inherited environment's value", got)
	}
}

// TestDataUnitDoctorPsRefusesWithoutBdKey: a unit whose values file names no bd
// image is refused before docker runs, as gt dolt start refuses it.
func TestDataUnitDoctorPsRefusesWithoutBdKey(t *testing.T) {
	t.Parallel()
	townRoot := composeTown(t, "")
	var calls []composeCall
	_, err := doltComposePsFor(fakeComposeDeps(townRoot, &calls, nil, nil))(townRoot)
	if err == nil || !strings.Contains(err.Error(), "no bd key") {
		t.Fatalf("status reader = %v, want the no-bd-key refusal", err)
	}
	if len(calls) != 0 {
		t.Errorf("docker calls = %d, want 0 when the unit is refused", len(calls))
	}
}

// TestDataUnitDoctorTokenDirRefuses: until the town config names the token
// directory, the check names none, so the token rung cannot pass on a guess.
func TestDataUnitDoctorTokenDirRefuses(t *testing.T) {
	t.Parallel()
	dir, err := dataUnitTokenDir(t.TempDir())
	if err == nil || dir != "" {
		t.Fatalf("dataUnitTokenDir = %q, %v; want a refusal and no directory", dir, err)
	}
}

// TestDataUnitDoctorPsSurfacesDockerFailure: a failed ps is an error that names
// the project and the exit status only, never what docker wrote.
func TestDataUnitDoctorPsSurfacesDockerFailure(t *testing.T) {
	t.Parallel()
	townRoot := composeTown(t, composeRef("gastown-bd"))
	var calls []composeCall
	deps := fakeComposeDeps(townRoot, &calls, []byte("host detail"), errors.New("docker exited with status 1"))
	_, err := doltComposePsFor(deps)(townRoot)
	if err == nil || !strings.Contains(err.Error(), "docker compose ps for "+doltComposeProject) {
		t.Fatalf("status reader = %v, want the failed ps named", err)
	}
	if strings.Contains(err.Error(), "host detail") {
		t.Errorf("error %q carries what docker wrote", err)
	}
}
