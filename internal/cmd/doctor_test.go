package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doctor"
)

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
