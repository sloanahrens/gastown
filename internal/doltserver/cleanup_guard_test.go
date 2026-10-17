package doltserver

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestHoldsFromIssues(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{
		nil, // must not panic
		{ID: "hq-1", Title: "no holds here", Labels: []string{"gt:task", "urgent"}},
		{ID: "hq-2", Title: "hold forkrig", Labels: []string{"dolt-hold:forkrig"}},
		{ID: "hq-3", Title: "blanket hold", Labels: []string{"dolt-hold"}},
		{ID: "hq-4", Title: "empty db name", Labels: []string{"dolt-hold:"}},
		{ID: "hq-5", Title: "two dbs", Labels: []string{"dolt-hold:alpha", "dolt-hold:beta"}},
	}

	holds := holdsFromIssues(issues)
	if len(holds) != 4 {
		t.Fatalf("expected 4 holds, got %d: %+v", len(holds), holds)
	}

	if holds[0].BeadID != "hq-2" || holds[0].DB != "forkrig" || holds[0].All {
		t.Errorf("unexpected first hold: %+v", holds[0])
	}
	if holds[1].BeadID != "hq-3" || !holds[1].All {
		t.Errorf("expected blanket hold from hq-3, got: %+v", holds[1])
	}
	if holds[2].DB != "alpha" || holds[3].DB != "beta" {
		t.Errorf("expected per-db holds alpha/beta, got: %+v %+v", holds[2], holds[3])
	}
}

func TestHoldFor(t *testing.T) {
	t.Parallel()
	specific := []DatabaseHold{
		{BeadID: "hq-2", DB: "forkrig"},
		{BeadID: "hq-5", DB: "alpha"},
	}

	if h := HoldFor(specific, "forkrig"); h == nil || h.BeadID != "hq-2" {
		t.Errorf("expected forkrig held by hq-2, got %+v", h)
	}
	if h := HoldFor(specific, "alpha"); h == nil || h.BeadID != "hq-5" {
		t.Errorf("expected alpha held by hq-5, got %+v", h)
	}
	if h := HoldFor(specific, "unrelated"); h != nil {
		t.Errorf("expected no hold for unrelated db, got %+v", h)
	}

	blanket := []DatabaseHold{{BeadID: "hq-3", All: true}}
	if h := HoldFor(blanket, "anything"); h == nil || h.BeadID != "hq-3" {
		t.Errorf("expected blanket hold to protect any db, got %+v", h)
	}

	if h := HoldFor(nil, "anything"); h != nil {
		t.Errorf("expected no hold with empty hold list, got %+v", h)
	}
}

func TestValidateForceAuthorization(t *testing.T) {
	t.Parallel()
	valid := func() *beads.Issue {
		return &beads.Issue{
			ID:        "hq-auth1",
			Title:     "Authorize forced cleanup of testdb_*",
			Status:    "open",
			CreatedBy: "overseer",
			Labels:    []string{ForceAuthLabel},
		}
	}

	t.Run("nil issue refused", func(t *testing.T) {
		if err := ValidateForceAuthorization(nil, "gastown/polecats/onyx"); err == nil {
			t.Fatal("expected error for nil issue")
		}
	})

	t.Run("valid authorization bead passes", func(t *testing.T) {
		if err := ValidateForceAuthorization(valid(), "gastown/polecats/onyx"); err != nil {
			t.Fatalf("expected valid authorization to pass, got: %v", err)
		}
	})

	t.Run("missing label refused", func(t *testing.T) {
		issue := valid()
		issue.Labels = []string{"gt:task"}
		err := ValidateForceAuthorization(issue, "gastown/polecats/onyx")
		if err == nil {
			t.Fatal("expected refusal for bead without the authorization label")
		}
		if !strings.Contains(err.Error(), ForceAuthLabel) {
			t.Errorf("error should name the required label, got: %v", err)
		}
		// bd update has --add-label, not --labels (gt-ch732).
		if want := "bd update hq-auth1 --add-label " + ForceAuthLabel; !strings.Contains(err.Error(), want) {
			t.Errorf("error should show %q, got: %v", want, err)
		}
		if strings.Contains(err.Error(), "--labels") {
			t.Errorf("error must not suggest the nonexistent bd update --labels flag, got: %v", err)
		}
	})

	t.Run("closed bead refused", func(t *testing.T) {
		issue := valid()
		issue.Status = "closed"
		if err := ValidateForceAuthorization(issue, "gastown/polecats/onyx"); err == nil {
			t.Fatal("expected refusal for closed authorization bead")
		}
	})

	t.Run("self-created bead refused", func(t *testing.T) {
		issue := valid()
		issue.CreatedBy = "gastown/polecats/onyx"
		err := ValidateForceAuthorization(issue, "gastown/polecats/onyx")
		if err == nil {
			t.Fatal("expected refusal when the requesting agent created the bead itself")
		}
		if !strings.Contains(err.Error(), "self") {
			t.Errorf("error should call out self-authorization, got: %v", err)
		}
	})
}

// TestValidateForceAuthorizationUnidentifiedActor covers gt-kz9k0: the actor
// resolveDestructiveActor reports for unset GT_ROLE/BD_ACTOR off-terminal never
// equals a bead's CreatedBy, so the self-authorization check used to pass any
// bead, including one the same agent had just created.
func TestValidateForceAuthorizationUnidentifiedActor(t *testing.T) {
	t.Parallel()
	for _, actor := range []string{string(UnidentifiedActor), ""} {
		issue := &beads.Issue{
			ID:        "hq-auth1",
			Title:     "Authorize forced cleanup of testdb_*",
			Status:    "open",
			CreatedBy: "gastown/polecats/onyx",
			Labels:    []string{ForceAuthLabel},
		}
		err := ValidateForceAuthorization(issue, actor)
		if err == nil {
			t.Fatalf("actor %q: expected refusal, got nil", actor)
		}
		if !errors.Is(err, ErrUnidentifiedActor) {
			t.Errorf("actor %q: error = %v, want ErrUnidentifiedActor", actor, err)
		}
	}
}

// TestIsUnidentifiedActor covers gt-kz9k0: only a missing identity or the
// sentinel itself counts as unidentified. A real identity never does, and an
// environment identity that happens to equal the sentinel's literal value is
// still refused (the safe direction).
func TestIsUnidentifiedActor(t *testing.T) {
	t.Parallel()
	for _, actor := range []string{"", string(UnidentifiedActor)} {
		if !IsUnidentifiedActor(actor) {
			t.Errorf("IsUnidentifiedActor(%q) = false, want true", actor)
		}
	}
	for _, actor := range []string{"gastown/polecats/onyx", "overseer", "unidentified-agent"} {
		if IsUnidentifiedActor(actor) {
			t.Errorf("IsUnidentifiedActor(%q) = true, want false", actor)
		}
	}
}

// TestUnidentifiedActorIsNotAnIdentity pins the sentinel to a placeholder
// rather than an agent name: a GT_ROLE names a rig and role ("<rig>/<role>"),
// so the sentinel cannot pass for one (gt-kz9k0, gt-od3fn).
func TestUnidentifiedActorIsNotAnIdentity(t *testing.T) {
	t.Parallel()
	got := string(UnidentifiedActor)
	if got == "" {
		t.Fatal("UnidentifiedActor must be a distinct value, not the empty string")
	}
	if strings.Contains(got, "/") {
		t.Errorf("UnidentifiedActor = %q has GT_ROLE shape; it must not read as a real identity", got)
	}
	if !IsUnidentifiedActor(got) {
		t.Errorf("IsUnidentifiedActor(%q) = false; the sentinel must always be refused", got)
	}
}

// TestErrUnidentifiedActorText guards the refusal's wording (gt-od3fn): it gives
// the reason no bead can authorize an unidentified process and a concrete
// retry, without repeating the --authorized-by usage block that
// checkAgentForceAuthorization already prints.
func TestErrUnidentifiedActorText(t *testing.T) {
	t.Parallel()
	msg := ErrUnidentifiedActor.Error()
	for _, want := range []string{
		"cannot be identified",
		"created by this same process",
		"GT_ROLE=",
		"interactive terminal",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrUnidentifiedActor should contain %q, got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "--authorized-by") {
		t.Errorf("ErrUnidentifiedActor repeats the --authorized-by usage hint:\n%s", msg)
	}
}
