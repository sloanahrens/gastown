package cmd

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doctor"
)

// TestDataUnitDoctorRegistration: the data unit check is registered on the
// doctor, and it reads the unit only when --check names it. Named, the status
// reader is the injected docker call, so the unit rung runs one ps through the
// fake and fails on its empty output. Unnamed, the check is skipped and calls
// nothing. No real docker or environment is read.
func TestDataUnitDoctorRegistration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		names      []string
		wantStatus doctor.CheckStatus
		wantCalls  int
	}{
		{name: "named by --check runs the unit through the injected docker", names: []string{doctor.DataUnitCheckName}, wantStatus: doctor.StatusError, wantCalls: 1},
		{name: "not named is skipped and calls nothing", names: nil, wantStatus: doctor.StatusSkipped, wantCalls: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := composeTown(t, composeRef("gastown-bd"))
			var calls []composeCall
			d := doctor.NewDoctor()
			registerDataUnitCheck(d, tt.names, fakeComposeDeps(townRoot, &calls, nil, nil))
			checks := d.Checks()
			if len(checks) != 1 || checks[0].Name() != doctor.DataUnitCheckName {
				t.Fatalf("registered checks = %d, want the data unit check alone", len(checks))
			}
			res := checks[0].Run(&doctor.CheckContext{TownRoot: townRoot})
			if res.Status != tt.wantStatus {
				t.Errorf("status = %v, want %v; message %q", res.Status, tt.wantStatus, res.Message)
			}
			if len(calls) != tt.wantCalls {
				t.Errorf("docker calls = %d, want %d", len(calls), tt.wantCalls)
			}
		})
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
