package gtwrap

import (
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// streamBreaks are the error texts a broken kubectl exec stream leaves when the
// call may have reached bd but its reply did not come back (gt-o9enrj).
var streamBreaks = []string{
	"error stream protocol error",
	"error executing command in container",
	"unexpected EOF",
	"stream reset",
}

// lostReplyNote returns the one-line note saying a bd write call may have run,
// without a prefix, or "" when no reply can have been lost: not a bd call, a
// success, no broken exec stream, or a read verb.
func lostReplyNote(argv []string, exit int, errText string) string {
	if len(argv) == 0 || argv[0] != "bd" || exit == 0 {
		return ""
	}
	broken := slices.ContainsFunc(streamBreaks, func(breakText string) bool {
		return strings.Contains(errText, breakText)
	})
	if !broken || bdgateway.IsReadCall(argv[1:]) {
		return ""
	}
	return "the bd call may have run: the exec stream broke before its reply arrived, so check with bd show before repeating anything that is not idempotent (a comment, a create); a repeated dep remove is safe, since it answers No dependency found"
}
