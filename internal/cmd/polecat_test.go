package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
)

// notFoundBeads is a recoveryBeads whose every Show answers ErrNotFound and
// which has no MR for any branch: a rig where the source issue was purged.
type notFoundBeads struct{ beads.Client }

func (notFoundBeads) Show(string) (*beads.Issue, error) { return nil, beads.ErrNotFound }

func (notFoundBeads) FindMRForBranchAny(string) (*beads.Issue, error) {
	return nil, beads.ErrNotFound
}

// A purged source bead is absent, not unreadable: the active-MR and branch-MR
// lookups already tolerate ErrNotFound, and the source-issue lookup must too,
// or every sweep forces NEEDS_RECOVERY on a polecat whose bead was deleted.
func TestRecoveryTargetRefsSourceIssueNotFound(t *testing.T) {
	t.Parallel()
	_, lookupFailed, _, _ := recoveryTargetRefs(notFoundBeads{}, "gt-purged", "", "polecat/nitro")
	if lookupFailed {
		t.Fatal("MQLookupFailed = true for a source issue that is not found, want false")
	}
}

// A source-issue read that fails for any other reason still fails closed.
func TestRecoveryTargetRefsSourceIssueReadErrorFailsClosed(t *testing.T) {
	t.Parallel()
	bd := failingShowBeads{notFoundBeads{}}
	_, lookupFailed, _, _ := recoveryTargetRefs(bd, "gt-abc", "", "")
	if !lookupFailed {
		t.Fatal("MQLookupFailed = false for an unreadable source issue, want true")
	}
}

type failingShowBeads struct{ notFoundBeads }

func (failingShowBeads) Show(string) (*beads.Issue, error) { return nil, errors.New("dolt down") }

// The verdict flips to NEEDS_RECOVERY on a failed reconcile, so the flags
// DecideWorkstate derived from SAFE_TO_NUKE must flip with it: --json must not
// report safe_to_nuke true beside needs_recovery true.
func TestReconcileCleanupStatusFailureClearsSafeToNuke(t *testing.T) {
	t.Parallel()
	for name, updater := range map[string]beads.Client{
		"update fails": func() beads.Client {
			u := newCleanupUpdater(t, polecat.CleanupUnpushed)
			u.err = errors.New("bd update failed")
			return u
		}(),
		"updater unavailable": nil,
	} {
		t.Run(name, func(t *testing.T) {
			status := &RecoveryStatus{
				CleanupStatus:        polecat.CleanupUnpushed,
				Verdict:              "SAFE_TO_NUKE",
				Branch:               "polecat/nitro",
				MQStatus:             "submitted",
				SafeToNuke:           true,
				Reusable:             true,
				CountsTowardCapacity: true,
			}
			reconcileCleanupStatusIfSafe(status, updater, "gt-gastown-polecat-nitro", &polecat.Polecat{State: polecat.StateIdle}, &beads.AgentFields{
				AgentState:    string(beads.AgentStateIdle),
				CleanupStatus: string(polecat.CleanupUnpushed),
			})

			if status.Verdict != "NEEDS_RECOVERY" || !status.NeedsRecovery {
				t.Fatalf("verdict = %q needs_recovery = %v, want NEEDS_RECOVERY true", status.Verdict, status.NeedsRecovery)
			}
			if status.SafeToNuke || status.Reusable || status.CountsTowardCapacity {
				t.Errorf("safe_to_nuke=%v reusable=%v counts_toward_capacity=%v, want all false beside NEEDS_RECOVERY",
					status.SafeToNuke, status.Reusable, status.CountsTowardCapacity)
			}
		})
	}
}

// --force bypasses the safety refusal, so its dry-run must not count one, and
// the preview must name the session and worktree the nuke will act on: the
// rig's registered session prefix and the Manager's clone path.
func TestPolecatNukeForceDryRunSummary(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "gastown", Path: t.TempDir()}
	reg := session.NewPrefixRegistry()
	reg.Register("zz", "gastown")
	target := polecatTarget{rigName: "gastown", polecatName: "basalt", mgr: polecat.NewManager(r, nil, nil, reg), r: r}
	blocked := func(polecatTarget) *SafetyCheckResult { return &SafetyCheckResult{Blocked: true} }
	listBlocked := func(polecatTarget) bool { return true }

	var out bytes.Buffer
	if previewNukeDryRun(&out, target, true, reg, blocked, listBlocked) {
		t.Error("forced preview counted a refusal that --force bypasses")
	}
	if got := dryRunNukeSummary(1, 0); got != "Would nuke 1 polecat(s)." {
		t.Errorf("summary = %q, want the unrefused summary", got)
	}
	text := out.String()
	if strings.Contains(text, "refuse") {
		t.Errorf("forced preview mentions a refusal:\n%s", text)
	}
	for _, want := range []string{
		"Would nuke gastown/basalt:",
		"Kill session: zz-basalt\n",
		"Delete worktree: " + target.mgr.ClonePath("basalt") + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("preview lacks %q:\n%s", want, text)
		}
	}

	out.Reset()
	if !previewNukeDryRun(&out, target, false, reg, blocked, listBlocked) {
		t.Error("unforced preview of a blocked polecat did not count a refusal")
	}
	if !strings.Contains(out.String(), "Would refuse to nuke gastown/basalt without --force:") {
		t.Errorf("unforced preview does not say it would refuse:\n%s", out.String())
	}
}
