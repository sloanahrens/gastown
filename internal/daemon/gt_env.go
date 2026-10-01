package daemon

import (
	"slices"
	"strings"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// daemonActor is the identity every gt the daemon runs carries. gt reads it
// as the sender of what it writes and the actor of the command usage log.
const daemonActor = "daemon"

// daemonGTEnv returns base with the agent identity replaced by the daemon's:
// every identity variable (config.IdentityEnvVars) is dropped and
// BD_ACTOR=daemon appended. runDaemonRun clears and sets the same variables
// process-wide, so in a running daemon this restates what the child inherits;
// building it here makes a gt child's identity independent of how the daemon
// was started (gt-kyik6).
func daemonGTEnv(base []string) []string {
	env := slices.DeleteFunc(slices.Clone(base), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(agentconfig.IdentityEnvVars, k)
	})
	return append(env, "BD_ACTOR="+daemonActor)
}
