package mail

import "github.com/steveyegge/gastown/internal/session"

// testPrefixRegistry maps the session prefixes the tests use to their rigs.
// Both tiers' TestMain set it once, so the tests can run in parallel.
func testPrefixRegistry() *session.PrefixRegistry {
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	reg.Register("bd", "beads")
	return reg
}
