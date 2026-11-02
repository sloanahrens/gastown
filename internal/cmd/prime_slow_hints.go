package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// hookedSlowHints shows a polecat the town's top prior records on slow or
// timed-out tests when its hooked bead's text mentions one, so it reads the
// town's standard before writing a fix (gt-jamix). The hint is advice: every
// failure prints nothing and the start goes on.
func (p primeTools) hookedSlowHints(ctx RoleContext, hookedBead *beads.Issue) {
	if p.skipHints || ctx.Role != RolePolecat || hookedBead == nil {
		return
	}
	if !specdispatch.SlowTextHint(hookedBead.Title + "\n" + hookedBead.Description) {
		return
	}
	hint := p.hint
	if hint == nil {
		hint = hmHintSource(nil)
	}
	hits, err := hint(context.Background(), strings.TrimSpace(hookedBead.Title+" "+specHintWords), specHintLimit)
	if err != nil || len(hits) == 0 {
		return
	}
	fmt.Fprintln(p.w(), hmSlowHintHeading)
	for _, h := range hits {
		fmt.Fprintf(p.w(), "hint: %s\n", hmHitLine(h))
	}
	fmt.Fprintln(p.w())
}
