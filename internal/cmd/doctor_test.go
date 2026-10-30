package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doctor"
)

// TestDoctorRegistersCensusBudgetCheck drives the registered check itself, not
// a fresh one: a check that is registered but never reached is a check no
// operator sees, and an Error status is the only one that fails a gate
// (gt-gojo0).
func TestDoctorRegistersCensusBudgetCheck(t *testing.T) {
	t.Parallel()
	var check *doctor.CensusBudgetCheck
	for _, c := range newDoctorForCommand("").Checks() {
		if census, ok := c.(*doctor.CensusBudgetCheck); ok {
			check = census
		}
	}
	if check == nil {
		t.Fatal("the census-budget check is not in the default doctor set")
	}

	// A town whose daemon logged an hour of census records, one caller far
	// above any row the table can carry.
	town := t.TempDir()
	log := filepath.Join(town, "daemon", "daemon.log")
	if err := os.MkdirAll(filepath.Dir(log), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local)
	var body strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&body, "%s bd census: total=99 window=60s top: list@metrics.queueLandingColumn=99\n",
			start.Add(time.Duration(i)*time.Minute).Format("2006/01/02 15:04:05"))
	}
	if err := os.WriteFile(log, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	check.Now = func() time.Time { return start.Add(60 * time.Minute) }

	r := check.Run(&doctor.CheckContext{TownRoot: town})

	if r.Status != doctor.StatusError {
		t.Fatalf("status = %v (%s), want Error from the registered check", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "metrics.queueLandingColumn") {
		t.Errorf("details %q must name the caller over budget", r.Details)
	}
}

func TestDoctorDoesNotRegisterDoltConfigCheck(t *testing.T) {
	t.Parallel()
	d := newDoctorForCommand("")
	for _, check := range d.Checks() {
		if check.Name() == "dolt-config" {
			t.Fatalf("dolt-config check must not be registered; it writes runtime Dolt keys into tracked .beads/config.yaml")
		}
	}
}

func TestDoctorRegistersEditorialChecksWithRig(t *testing.T) {
	t.Parallel()
	d := newDoctorForCommand("testrig")
	want := map[string]bool{
		"harness-drift":      false,
		"editorial-required": false,
	}
	for _, check := range d.Checks() {
		if _, ok := want[check.Name()]; ok {
			want[check.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected %q to be registered when --rig is set", name)
		}
	}
}

func TestDoctorRegistersRigBDBinaryCheck(t *testing.T) {
	t.Parallel()
	d := newDoctorForCommand("")
	for _, check := range d.Checks() {
		if check.Name() == "rig-bd-binary" {
			return
		}
	}
	t.Fatal("expected the rig-bd-binary check in the default doctor set")
}

func TestDoctorRegistersBdShimFlagsCheck(t *testing.T) {
	t.Parallel()
	d := newDoctorForCommand("")
	for _, check := range d.Checks() {
		if check.Name() == "bd-shim-flags" {
			return
		}
	}
	t.Fatal("expected the bd-shim-flags check in the default doctor set")
}

func TestDoctorRegistersTrackerCheck(t *testing.T) {
	t.Parallel()
	d := newDoctorForCommand("")
	for _, check := range d.Checks() {
		if check.Name() == "tracker" {
			return
		}
	}
	t.Fatal("expected the tracker check in the default doctor set")
}

func TestDoctorRegistersOpenShellGatewayCheck(t *testing.T) {
	t.Parallel()
	d := newDoctorForCommand("")
	for _, check := range d.Checks() {
		if check.Name() == "openshell-gateway" {
			return
		}
	}
	t.Fatal("expected the openshell-gateway check in the default doctor set")
}

// TestDoctorHelpListsOnlyRegisteredChecks ties the check names `gt doctor
// --help` prints to the checks the command registers (om finding ff39e1346648).
// The list is hand-written, so a renamed or retired check leaves its line
// behind: editorial-coverage (gt-bl2ix) and pre-checkout-hook (gt-wrpt9).
//
// A registered check the help omits is a documentation gap rather than drift,
// so the help -> registry direction is the one asserted.
func TestDoctorHelpListsOnlyRegisteredChecks(t *testing.T) {
	t.Parallel()

	// --rig adds checks on top of the default set, and the help puts them under
	// their own heading, so a name in that section must be one --rig adds.
	registered := registeredCheckNames(newDoctorForCommand(""))
	rigOnly := registeredCheckNames(newDoctorForCommand("testrig"))
	for name := range registered {
		delete(rigOnly, name)
	}

	const rigHeading = "Rig checks (with --rig flag):"
	heading := ""
	listed, sawRigHeading := 0, false
	for line := range strings.SplitSeq(doctorCmd.Long, "\n") {
		if !strings.HasPrefix(line, "  - ") {
			// Blank lines separate paragraphs within one list; any other
			// non-bullet line is the next heading.
			if line != "" {
				heading = line
				if line == rigHeading {
					sawRigHeading = true
				}
			}
			continue
		}
		listed++
		name, _, _ := strings.Cut(strings.TrimPrefix(line, "  - "), " ")
		want := registered
		if heading == rigHeading {
			want = rigOnly
		}
		if !want[name] {
			t.Errorf("`gt doctor --help` lists check %q under %q, but no check of that name is registered there", name, heading)
		}
	}
	// Both guards are about this test's own reach: with no names read, or with
	// the rig section gone, it would pass while checking nothing.
	if listed == 0 {
		t.Fatal("read no check names from the help text: the bullet format changed, so this test no longer ties the help to the registry")
	}
	if !sawRigHeading {
		t.Fatalf("no %q heading in the help text, so the rig-section check above never ran", rigHeading)
	}
}

// TestDoctorHelpDescribesMayorCloneExistsAsEitherRepo pins the help line to what
// the check does: a rig passes on its shared .repo.git alone (gt-9gnar), so the
// line must not claim the mayor/rig clone is what is verified (gt-7dfez).
func TestDoctorHelpDescribesMayorCloneExistsAsEitherRepo(t *testing.T) {
	t.Parallel()

	const prefix = "  - mayor-clone-exists "
	found := false
	for line := range strings.SplitSeq(doctorCmd.Long, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		found = true
		for _, want := range []string{".repo.git", "mayor/rig"} {
			if !strings.Contains(line, want) {
				t.Errorf("`gt doctor --help` line for mayor-clone-exists %q does not name %q", line, want)
			}
		}
	}
	if !found {
		t.Fatal("no mayor-clone-exists line in the doctor help text")
	}
}

// registeredCheckNames is the set of names a doctor instance holds.
func registeredCheckNames(d *doctor.Doctor) map[string]bool {
	names := make(map[string]bool, len(d.Checks()))
	for _, check := range d.Checks() {
		names[check.Name()] = true
	}
	return names
}

func TestDoctorCheckFlagFiltersToNamedCheck(t *testing.T) {
	t.Parallel()
	d := newDoctorForCommand("testrig")
	d.Only([]string{"editorial-required"})
	checks := d.Checks()
	if len(checks) != 1 || checks[0].Name() != "editorial-required" {
		names := make([]string, len(checks))
		for i, c := range checks {
			names[i] = c.Name()
		}
		t.Fatalf("expected --check to filter to exactly [editorial-required], got %v", names)
	}
}

// Doctor.Find returns the first check of a name and --check selects every
// check of it, so two checks sharing a name made 'gt doctor fix session-hooks'
// always target the config check (gt-uh7hy).
func TestDoctorChecksHaveUniqueNames(t *testing.T) {
	t.Parallel()
	for _, rig := range []string{"", "testrig"} {
		seen := make(map[string]bool)
		for _, check := range newDoctorForCommand(rig).Checks() {
			if seen[check.Name()] {
				t.Errorf("rig %q: check name %q is registered twice", rig, check.Name())
			}
			seen[check.Name()] = true
		}
	}
}

// Doctor.Only drops a name nothing registers, so 'gt doctor --check daemon
// --check dameon' ran daemon alone and exited 0 (gt-uh7hy).
func TestRunDoctorUnknownCheckAmongKnown(t *testing.T) {
	t.Parallel()
	d := newDoctorForCommand("")
	err := selectDoctorChecks(d, []string{"daemon", "dameon"})
	if err == nil {
		t.Fatal("selectDoctorChecks accepted a --check list holding an unknown name")
	}
	if !strings.Contains(err.Error(), "dameon") {
		t.Errorf("error = %q, want the unknown name dameon", err)
	}
	if strings.Contains(err.Error(), "daemon") {
		t.Errorf("error = %q, want only the unknown name, not the known one", err)
	}

	d = newDoctorForCommand("")
	if err := selectDoctorChecks(d, []string{"daemon"}); err != nil {
		t.Fatalf("selectDoctorChecks(daemon) = %v, want nil", err)
	}
	if got := d.Checks(); len(got) != 1 || got[0].Name() != "daemon" {
		t.Errorf("checks after --check daemon = %d, want exactly daemon", len(got))
	}
}

// 'gt doctor clone-divergence' ignored its argument and ran every check; a
// positional name now selects like --check does, and an unknown one is refused
// the same way (gt-m2lqo).
func TestDoctorPositionalCheckNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		flag     []string
		args     []string
		wantErr  string
		wantRuns []string
	}{
		{name: "positional alone", args: []string{"daemon"}, wantRuns: []string{"daemon"}},
		{name: "flag alone", flag: []string{"daemon"}, wantRuns: []string{"daemon"}},
		{name: "flag and positional", flag: []string{"daemon"}, args: []string{"disk-space"}, wantRuns: []string{"daemon", "disk-space"}},
		{name: "unknown positional", args: []string{"dameon"}, wantErr: "dameon"},
		{name: "unknown positional beside known", args: []string{"daemon", "dameon"}, wantErr: "dameon"},
		{name: "unknown positional beside known flag", flag: []string{"daemon"}, args: []string{"dameon"}, wantErr: "dameon"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDoctorForCommand("")
			err := selectDoctorChecks(d, doctorCheckNames(tc.flag, tc.args))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("selectDoctorChecks accepted %v %v, want an error naming %s", tc.flag, tc.args, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to name %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectDoctorChecks(%v, %v) = %v, want nil", tc.flag, tc.args, err)
			}
			got := make(map[string]bool)
			for _, c := range d.Checks() {
				got[c.Name()] = true
			}
			if len(got) != len(tc.wantRuns) {
				t.Errorf("checks after selection = %v, want exactly %v", got, tc.wantRuns)
			}
			for _, name := range tc.wantRuns {
				if !got[name] {
					t.Errorf("check %q missing after selection, have %v", name, got)
				}
			}
		})
	}
}

// The cases above drive selectDoctorChecks directly; this one runs runDoctor,
// so a runDoctor that stopped passing its positional arguments or the --check
// flag to the selection would fail here. An unknown name is refused before any
// check runs, which keeps the test off the host (gt-m2lqo).
//
//testpolicy:allow parallel — runDoctor reads the --check flag variable and the working directory, both process-wide
func TestRunDoctorRefusesUnknownCheckName(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-chdir — the cwd is the input under test: runDoctor finds the town from the working directory
	t.Chdir(townRoot)

	cases := []struct {
		name string
		flag []string
		args []string
	}{
		{name: "positional", args: []string{"dameon"}},
		{name: "positional beside known", args: []string{"daemon", "dameon"}},
		{name: "flag", flag: []string{"dameon"}},
		{name: "flag beside known positional", flag: []string{"dameon"}, args: []string{"daemon"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saved := doctorCheck
			t.Cleanup(func() {
				//testpolicy:allow no-global-swap — restores the flag variable swapped below
				doctorCheck = saved
			})
			//testpolicy:allow no-global-swap — runDoctor reads the --check flag variable; it is not an argument
			doctorCheck = tc.flag

			err := runDoctor(doctorCmd, tc.args)
			if err == nil {
				t.Fatalf("runDoctor(--check %v, %v) = nil, want an error naming dameon", tc.flag, tc.args)
			}
			if !strings.Contains(err.Error(), "dameon") {
				t.Errorf("error = %q, want it to name dameon", err)
			}
			if !strings.Contains(err.Error(), "no registered check matches") {
				t.Errorf("error = %q, want the unknown-check refusal, not another failure", err)
			}
		})
	}
}
