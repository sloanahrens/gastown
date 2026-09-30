package doctor

import "github.com/steveyegge/gastown/internal/session"

// testPrefixRegistry maps the session prefixes the unit tests use to their
// rigs. "ga" and "gt" both name gastown; gt, registered last, is the prefix
// gastown's sessions are given.
func testPrefixRegistry() *session.PrefixRegistry {
	reg := session.NewPrefixRegistry()
	for _, p := range [][2]string{
		{"ga", "gastown"}, {"gt", "gastown"}, {"bd", "beads"}, {"mr", "myrig"},
		{"r1", "rig1"}, {"fb", "foo-bar"}, {"nif", "niflheim"}, {"grc", "grctool"},
		{"7s", "7thsense"}, {"pf", "pulseflow"},
	} {
		reg.Register(p[0], p[1])
	}
	return reg
}
