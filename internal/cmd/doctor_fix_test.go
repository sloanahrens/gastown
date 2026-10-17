package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doctor"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// TestDoctorHasNoBlanketFixFlag locks in the split (gt-638go.3): the town-wide
// --fix is gone, so no invocation can repair the whole town at once.
func TestDoctorHasNoBlanketFixFlag(t *testing.T) {
	t.Parallel()
	if f := doctorCmd.Flags().Lookup("fix"); f != nil {
		t.Fatalf("gt doctor must not carry a --fix flag (got %q); repairs are 'gt doctor fix <check>'", f.Usage)
	}
	if f := doctorCmd.Flags().Lookup("no-start"); f != nil {
		t.Errorf("--no-start is a repair flag and belongs on 'gt doctor fix', got %q", f.Usage)
	}
	if f := doctorCmd.Flags().Lookup("restart-sessions"); f != nil {
		t.Errorf("--restart-sessions is a repair flag and belongs on 'gt doctor fix', got %q", f.Usage)
	}
}

// TestDoctorFixIsRegistered: the repair command hangs off gt doctor.
func TestDoctorFixIsRegistered(t *testing.T) {
	t.Parallel()
	found := false
	for _, c := range doctorCmd.Commands() {
		if c.Name() == "fix" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("gt doctor fix is not registered")
	}
}

// TestDoctorFixRequiresExactlyOneCheck: a repair names its check, so neither a
// bare `gt doctor fix` nor two names is accepted.
func TestDoctorFixRequiresExactlyOneCheck(t *testing.T) {
	t.Parallel()
	if err := doctorFixCmd.Args(doctorFixCmd, nil); err == nil {
		t.Error("gt doctor fix with no check should be rejected")
	}
	if err := doctorFixCmd.Args(doctorFixCmd, []string{"a", "b"}); err == nil {
		t.Error("gt doctor fix with two checks should be rejected")
	}
	if err := doctorFixCmd.Args(doctorFixCmd, []string{"zombie-sessions"}); err != nil {
		t.Errorf("gt doctor fix <check> should be accepted, got %v", err)
	}
}

// TestAuthorizeDoctorFixRefusesAgentWithoutAuthorization: an agent may not run
// a destructive repair without recorded authorization (gt-638go.3), and the
// refusal names the check and the remedy.
func TestAuthorizeDoctorFixRefusesAgentWithoutAuthorization(t *testing.T) {
	t.Parallel()
	err := authorizeDestructiveFix(t.TempDir(), doctor.NewZombieSessionCheck(), "gastown/polecats/amber", false, "")
	if err == nil {
		t.Fatal("an agent should be refused without --authorized-by")
	}
	for _, want := range []string{"zombie-sessions", "--authorized-by", "gt-638go.3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q, got: %v", want, err)
		}
	}
}

// TestAuthorizeDoctorFixRefusesUnidentifiedActor covers gt-2kwxr: with no env
// identity and no terminal the actor is the unidentified sentinel, and the
// refusal must hold even when the named bead is one it could have created.
func TestAuthorizeDoctorFixRefusesUnidentifiedActor(t *testing.T) {
	t.Parallel()
	actor, isAgent := resolveDestructiveActor("", false)
	if !isAgent || !doltserver.IsUnidentifiedActor(actor) {
		t.Fatalf("resolveDestructiveActor(\"\", false) = %q, %v; want the unidentified agent", actor, isAgent)
	}
	issue := &beads.Issue{ID: "gt-1", Status: "open", CreatedBy: "self-made", Labels: []string{doctor.FixAuthLabel}}
	if err := doctor.ValidateFixAuthorization(issue, actor); !errors.Is(err, doctor.ErrUnidentifiedFixActor) {
		t.Errorf("error = %v, want doctor.ErrUnidentifiedFixActor", err)
	}
}

// TestDestructiveChecksAreFixable: a DestructiveFix marker on a report-only
// check is inert — the authorize hook is never consulted — so it would
// advertise a gate that does not exist.
func TestDestructiveChecksAreFixable(t *testing.T) {
	t.Parallel()
	d := newDoctorForCommand("")
	for _, check := range d.Checks() {
		if doctor.IsDestructiveFix(check) && !check.CanFix() {
			t.Errorf("%s is marked destructive but has no repair", check.Name())
		}
	}
}

// The --authorized-by bead lookup itself runs bd, so its refusal paths
// (unknown bead, unlabeled bead, self-authorization) are unit-tested on
// doctor.ValidateFixAuthorization and exercised end to end by the
// integration tier.
