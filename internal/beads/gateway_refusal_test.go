package beads

import (
	"errors"
	"testing"
)

// gatewaySQLRefusal is the refusal gtwrap prints when the gateway denies a
// read token's bd sql: an HTTP 403 over the policy's message naming the verb
// and its class (gt-bbclhr). The address is the one allowStaleCache's test
// uses for the gateway's refusal of --allow-stale.
const gatewaySQLRefusal = `gtwrap: the bd gateway at http://127.0.0.1:7777 refused the call with HTTP 403: forbidden: bdgateway: token class may not run this verb: read token may not run sql (write)`

func TestGatewaySQLRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"403 refusal of sql", errors.New(gatewaySQLRefusal), true},
		{"403 refusal of --allow-stale", errors.New(gatewayRefusedAllowStale), false},
		{"sql refused with another status", errors.New(`gtwrap: the bd gateway at http://127.0.0.1:7777 refused the call with HTTP 503: unavailable: read token may not run sql (write)`), false},
		{"exit error", exitError{1}, false},
		{"unavailable error", errors.New("bd unavailable"), false},
		{"nil-text unavailable error", &unavailableError{}, false},
		{"nil", nil, false},
	} {
		if got := gatewaySQLRefused(tc.err); got != tc.want {
			t.Errorf("%s: gatewaySQLRefused = %v, want %v", tc.name, got, tc.want)
		}
	}
}
