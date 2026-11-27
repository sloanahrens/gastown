package daemon

import (
	"strings"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/sandbox"
)

// stuckSandboxMessageMax is how much of the Ready condition's message one queue row carries.
const stuckSandboxMessageMax = 200

// appendStuckSandboxReasons appends the gateway's own Ready reason and message to each stuck sandbox's item, so a sandbox left outside Ready records why before anyone deletes it (gt-7kd8ko; gt-m7s2wf wires it into the collector). items are keyed seatSandboxStaleKeyPrefix + sandbox name; failed reports a sandbox in a terminal failure phase, whose item is returned unchanged and whose sandbox is not read. The input slice and its items are not modified.
func appendStuckSandboxReasons(items []attention.Item, failed func(name string) bool, read func(name string) (sandbox.Status, error)) []attention.Item {
	out := make([]attention.Item, 0, len(items))
	for _, it := range items {
		name := strings.TrimPrefix(it.Key, seatSandboxStaleKeyPrefix)
		if failed(name) {
			out = append(out, it)
			continue
		}
		st, err := read(name)
		switch {
		case err != nil:
			it.Summary += " Gateway Ready reason: not read." // the error text can carry a secret
		case !st.ReadyKnown:
			it.Summary += " Gateway Ready reason: none given."
		default:
			reason, message := stuckSandboxOneLine(st.ReadyReason), stuckSandboxOneLine(st.ReadyMessage)
			if len(message) > stuckSandboxMessageMax {
				message = strings.ToValidUTF8(message[:stuckSandboxMessageMax], "")
			}
			it.Summary += " Gateway Ready reason: " + reason + " (" + message + ")."
		}
		out = append(out, it)
	}
	return out
}

// stuckSandboxOneLine collapses whitespace so a multi-line reason or message stays one queue row.
func stuckSandboxOneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
