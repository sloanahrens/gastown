package daemon

import (
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// bdReadOnlyEnv returns an environment slice for read-only bd/gt subprocess
// calls invoked by the daemon. It forces BD_DOLT_AUTO_COMMIT=off so that
// read-only operations (status checks, list, show) do not request a Dolt
// auto-commit on completion. Without this, every read-only call opens a
// fresh connection to attempt a no-op commit, producing thousands of
// failed-but-counted connections per hour on idle towns and spamming
// dolt.log. See gh#3596.
//
// Existing BD_DOLT_AUTO_COMMIT entries are filtered out before appending
// the authoritative "off" value, because glibc getenv() returns the first
// matching entry — a stale "on" earlier in the slice would otherwise win.
func bdReadOnlyEnv() []string {
	return bdReadOnlyRoutingEnvFrom(os.Environ(), "")
}

func bdReadOnlyRoutingEnv(townRoot string) []string {
	return bdReadOnlyRoutingEnvFrom(os.Environ(), townRoot)
}

// bdReadOnlyRoutingEnvFrom is bdReadOnlyRoutingEnv over base instead of the
// daemon's own environment. It is a town-level call, so it names the town's
// .beads (none when townRoot is empty).
func bdReadOnlyRoutingEnvFrom(base []string, townRoot string) []string {
	if townRoot != "" {
		base = agentconfig.NormalizeConfiguredDoltEnv(base, townRoot)
	}
	return beads.BuildReadOnlyRoutingBDEnv(base, routingFallbackBeadsDir(townRoot, ""))
}

func bdMutationRoutingEnv(townRoot string) []string {
	return bdMutationRoutingEnvOver(os.Environ(), townRoot, "")
}

// bdMutationRoutingEnvOver is bdMutationRoutingEnv over base for a call over
// beadsDir, the .beads directory of the store the call reads. A caller whose
// client is over a rig's database names that rig's .beads, so the routed call
// lands in the rig and not in the town database (gt-dl9ug0). An empty beadsDir
// names the town's .beads.
func bdMutationRoutingEnvOver(base []string, townRoot, beadsDir string) []string {
	if townRoot != "" {
		base = agentconfig.NormalizeConfiguredDoltEnv(base, townRoot)
	}
	return beads.BuildMutationRoutingBDEnv(base, routingFallbackBeadsDir(townRoot, beadsDir))
}

// routingFallbackBeadsDir returns the workspace a routed call names as
// BEADS_DIR: beadsDir when the caller names the store the call reads, the
// town's own .beads otherwise. An empty town names none, as before.
func routingFallbackBeadsDir(townRoot, beadsDir string) string {
	if beadsDir != "" {
		return beadsDir
	}
	if townRoot == "" {
		return ""
	}
	return filepath.Join(townRoot, ".beads")
}

func bdReadOnlyPinnedEnv(beadsDir string) []string {
	return bdReadOnlyPinnedEnvFrom(os.Environ(), beadsDir)
}

// bdReadOnlyPinnedEnvFrom is bdReadOnlyPinnedEnv over base instead of the
// daemon's own environment.
func bdReadOnlyPinnedEnvFrom(base []string, beadsDir string) []string {
	if townRoot := beads.FindTownRoot(filepath.Dir(beads.ResolveBeadsDir(beadsDir))); townRoot != "" {
		base = agentconfig.NormalizeConfiguredDoltEnv(base, townRoot)
	}
	return beads.BuildReadOnlyPinnedBDEnv(base, beadsDir)
}
