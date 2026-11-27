package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/notify"
)

// TestDoneSizeEscalationRaisesMediumRequest: a size refusal reaches raise as a
// medium escalation on its bead, with the refusal as the reason and a
// fingerprint naming the seat, whatever flags the run carries (gt-35xgjt).
func TestDoneSizeEscalationRaisesMediumRequest(t *testing.T) {
	t.Parallel()
	fx := newEscalateFixture(t)
	fx.r.rig, fx.r.polecat = "gastown", "capable"
	fx.r.severity, fx.r.dryRun, fx.r.json = "low", true, true
	const refusal = "Change exceeds 70 added lines.\nyour work is pushed at abc123"

	if err := raiseSizeRefusal(fx.r, "gt-35xgjt", refusal); err != nil {
		t.Fatalf("raiseSizeRefusal: %v", err)
	}
	if len(fx.raised) != 1 {
		t.Fatalf("raised %d escalations, want 1", len(fx.raised))
	}
	req := fx.raised[0]
	if req.Severity != "medium" || req.RelatedBead != "gt-35xgjt" || req.Reason != refusal || req.Fingerprint != "size-refusal:gastown/capable/gt-35xgjt" {
		t.Errorf("request = %+v", req)
	}
	if want := "gt-35xgjt: gt done refused this branch on size; the overseer decides whether to raise the limit or split the bead"; req.Description != want {
		t.Errorf("description = %q, want %q", req.Description, want)
	}
}

// TestDoneSizeEscalationReturnsRaiseError: a raise that fails is returned
// unchanged, so gt done can say why no overseer was told (gt-35xgjt).
func TestDoneSizeEscalationReturnsRaiseError(t *testing.T) {
	t.Parallel()
	fx := newEscalateFixture(t)
	raiseErr := errors.New("no overseer")
	fx.r.raise = func(notify.EscalationRequest, *config.EscalationConfig) (*notify.RaiseResult, error) {
		return nil, raiseErr
	}

	if err := raiseSizeRefusal(fx.r, "gt-35xgjt", "Change exceeds 70 added lines."); !errors.Is(err, raiseErr) {
		t.Errorf("error = %v, want %v", err, raiseErr)
	}
}
