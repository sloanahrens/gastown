package polecat

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// unnamedSandboxLines reports one line per gateway for what it holds under name
// when both the seat's record and agent bead are gone: a gateway that holds it
// is not removed because its work is not proven on origin, and one that cannot
// answer says so without stopping the rest (gt-xo4p78). status must be non-nil;
// the caller resolves a nil seam to session.GatewaySandboxPhase.
func unnamedSandboxLines(status func(gateway, name string) (sandbox.Status, error), name string, gateways []string) []string {
	var lines []string
	asked := make(map[string]bool, len(gateways))
	for _, gateway := range gateways {
		if asked[gateway] {
			continue
		}
		asked[gateway] = true
		st, err := status(gateway, name)
		if err != nil {
			if session.SandboxNotFound(err) {
				continue
			}
			lines = append(lines, fmt.Sprintf("gateway %q could not answer for sandbox %s: %v", gateway, name, err))
			continue
		}
		lines = append(lines, fmt.Sprintf("gateway %q holds sandbox %s in phase %s: it is not removed because its agent bead is gone, so its work is not proven on origin", gateway, name, st.Phase))
	}
	return lines
}
