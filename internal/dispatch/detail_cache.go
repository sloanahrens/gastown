package dispatch

import "path/filepath"

// SpecDetailOwnerEnv is the variable the daemon sets on the `gt spec dispatch`
// process it starts for a tick, naming itself. Each tick is a process of its
// own, so what the tick learns about the pool's design and notes text survives
// to the next one only in SpecDetailCachePath; the owner is what makes that
// file the daemon's: a file written under another owner is another daemon's,
// and is dropped, so the cache lasts as long as the daemon does (gt-l0vlf).
// A tick run by hand sets none and keeps nothing between runs.
const SpecDetailOwnerEnv = "GT_SPEC_DETAIL_OWNER"

// SpecDetailCachePath is where a daemon's ticks keep the design and notes text
// they read for the duplicate check's pool.
func SpecDetailCachePath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "spec_detail_cache.json")
}

// SpecAnnotatedPath is where a daemon's ticks keep which held beads they have
// already annotated, beside SpecDetailCachePath and owned the same way.
func SpecAnnotatedPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "spec_annotated.json")
}
