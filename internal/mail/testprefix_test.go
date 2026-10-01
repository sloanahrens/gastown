package mail

import "github.com/steveyegge/gastown/internal/session"

// testPrefixRegistry maps the session prefixes the tests use to their rigs.
// Tests hand it to the routers and helpers that resolve session names.
func testPrefixRegistry() *session.PrefixRegistry {
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	reg.Register("bd", "beads")
	return reg
}
