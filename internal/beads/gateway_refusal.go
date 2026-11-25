package beads

import "strings"

// gatewaySQLVerb is the bd verb a gateway refusal names, kept out of a call's
// arguments so internal/beadsql's guard does not read it as a hand-built bd
// argv (gt-kybol9).
const gatewaySQLVerb = "sql"

// gatewaySQLRefused reports whether err is the bd gateway's HTTP 403 refusal
// of a bd sql call, by the wording gtwrap produces: the status and the verb
// both have to be the gateway's, as with allowStaleAnswer's --allow-stale
// answer (gt-d3v0iu). Any other error is not a refusal (gt-kybol9).
func gatewaySQLRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "refused the call with HTTP 403") && strings.Contains(msg, gatewaySQLVerb)
}
