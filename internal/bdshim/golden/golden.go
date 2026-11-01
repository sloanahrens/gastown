// Package golden rewrites the values bd decides at run time out of a recorded
// answer, so a committed golden holds what the run produced and still reads the
// way bd printed it. The recorder writes its goldens through Normalize, and the
// differential test normalizes a shim handler's answer with the same list, so
// the two cannot drift apart (gt-p85pw.103, gt-p85pw.118).
package golden

// FixturePrefix is the issue prefix every copy of the fixture database is
// initialized with, the one the fixture file's bead ids carry.
const FixturePrefix = "fx"
