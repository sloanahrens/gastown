package doltserver

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestHoldsFromIssues(t *testing.T) {
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
	valid := func() *beads.Issue {
		return &beads.Issue{
			ID:        "hq-auth1",
			Title:     "Authorize forced cleanup of testdb_*",
			Status:    "open",
			CreatedBy: "mayor",
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
