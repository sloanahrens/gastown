package cmdtree

import (
	"fmt"
	"strings"
)

// BdSyncDenied is the reason every `bd sync` invocation fails the lint. In
// the beads fork, bd sync pulls, repairs is_blocked and pushes the rig's
// Dolt remote (exit codes 0-4); an agent step that runs it publishes the
// database and races its siblings (deep review B1-08, B5-11).
const BdSyncDenied = "bd sync is the Dolt federation loop in this fork; never call it from an agent"

// denied maps a command path, as "<bin> <words>", to the reason it may not
// appear in any lint input even though the command exists.
var denied = map[string]string{
	"bd sync": BdSyncDenied,
}

// Violation is an invocation the command trees cannot resolve, or one that
// is deny-listed.
type Violation struct {
	Ref
	Reason string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", v.File, v.Line, v.Token(), v.Reason)
}

// Check resolves every ref against trees (keyed by binary name) and returns
// the violations in ref order.
func Check(refs []Ref, trees map[string]*Tree) []Violation {
	var out []Violation
	for _, r := range refs {
		if reason, ok := deniedReason(r); ok {
			out = append(out, Violation{Ref: r, Reason: reason})
			continue
		}
		tree := trees[r.Bin]
		if tree == nil {
			out = append(out, Violation{Ref: r, Reason: "no command tree for " + r.Bin})
			continue
		}
		res := tree.Resolve(r.Words)
		if res.OK {
			continue
		}
		if r.Comment && len(res.Matched) == 0 {
			continue // comment prose that only mentions gt/bd
		}
		if res.HelpOnly {
			out = append(out, Violation{Ref: r, Reason: fmt.Sprintf("%q stops on a parent command: name one of its subcommands", r.Bin+" "+res.MatchedPath())})
			continue
		}
		reason := fmt.Sprintf("unknown command %q for %s", res.Unknown, r.Bin)
		if len(res.Matched) > 0 {
			reason = fmt.Sprintf("unknown subcommand %q for %q", res.Unknown, r.Bin+" "+res.MatchedPath())
		}
		out = append(out, Violation{Ref: r, Reason: reason})
	}
	return out
}

func deniedReason(r Ref) (string, bool) {
	for i := len(r.Words); i > 0; i-- {
		if reason, ok := denied[r.Bin+" "+strings.Join(r.Words[:i], " ")]; ok {
			return reason, true
		}
	}
	return "", false
}
