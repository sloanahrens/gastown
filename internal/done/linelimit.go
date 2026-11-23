package done

import "github.com/steveyegge/gastown/internal/specdispatch"

// DeclaredLineLimits returns the added-line limits a bead's text declares, in
// order of appearance and once each, or nil when it declares none. It reads
// them through specdispatch's shared grammar, the same one the lint reads, so
// the gate and the lint cannot drift (gt-yzhigm).
func DeclaredLineLimits(text string) []int {
	return specdispatch.DeclaredLineLimits(text)
}
