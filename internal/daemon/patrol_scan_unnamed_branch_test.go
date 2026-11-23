package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// TestPatrolScanUnnamedBranchRef: the escalation filed for a seat that stopped
// on ESCALATED names the branch and short sha this run pushed, says no pushed
// branch when origin holds nothing newer than the sling, and names a failed
// read in place of the clause while the filing still goes out (gt-t4wvy4).
func TestPatrolScanUnnamedBranchRef(t *testing.T) {
	t.Parallel()
	slung := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	head := "0123456789abcdef0123456789abcdef01234567"
	// pushed is the stub's answer for a branch origin holds with a head
	// committed after the sling.
	pushed := func(p *pushedTown) {
		p.git.commitTime = slung.Add(5 * time.Minute)
		p.git.sha = head
		p.originHolds(head)
	}
	for _, tc := range []struct {
		name  string
		setup func(p *pushedTown)
		want  string
	}{
		{"pushed head", pushed, "branch " + pushedBranch + " @ " + head[:8] + " pushed"},
		{"no pushed branch", func(*pushedTown) {}, "no pushed branch"},
		{"failed head read", func(p *pushedTown) {
			p.git.originErr = errors.New("ls-remote: transport failed")
		}, "pushed branch unreadable: read origin's"},
		{"failed branch read", func(p *pushedTown) {
			p.d.openGitFn = func(string) daemonGit { return branchReadFails{p.git} }
		}, "pushed branch unreadable: gastown/capable: read the seat's branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPushedTown(t)
			p.slingAt(t, slung)
			rec := notifyfake.New()
			p.d.notifier = rec
			tc.setup(p)

			if err := p.h.FileUnnamedEscalation(pushedRig, pushedPolecat, pushedBead); err != nil {
				t.Fatalf("FileUnnamedEscalation: %v", err)
			}
			calls := rec.Escalations()
			if len(calls) != 1 {
				t.Fatalf("escalations = %d, want 1", len(calls))
			}
			if reason := calls[0].Escalation.Reason; !strings.Contains(reason, tc.want) {
				t.Errorf("reason %q lacks %q", reason, tc.want)
			}
		})
	}
}
