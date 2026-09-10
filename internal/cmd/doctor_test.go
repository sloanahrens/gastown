package cmd

import "testing"

func TestDoctorDoesNotRegisterDoltConfigCheck(t *testing.T) {
	d := newDoctorForCommand("")
	for _, check := range d.Checks() {
		if check.Name() == "dolt-config" {
			t.Fatalf("dolt-config check must not be registered; it writes runtime Dolt keys into tracked .beads/config.yaml")
		}
	}
}

func TestDoctorRegistersEditorialChecksWithRig(t *testing.T) {
	d := newDoctorForCommand("testrig")
	want := map[string]bool{
		"editorial-coverage": false,
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

func TestDoctorCheckFlagFiltersToNamedCheck(t *testing.T) {
	d := newDoctorForCommand("testrig")
	d.Only([]string{"editorial-coverage"})
	checks := d.Checks()
	if len(checks) != 1 || checks[0].Name() != "editorial-coverage" {
		names := make([]string, len(checks))
		for i, c := range checks {
			names[i] = c.Name()
		}
		t.Fatalf("expected --check to filter to exactly [editorial-coverage], got %v", names)
	}
}
