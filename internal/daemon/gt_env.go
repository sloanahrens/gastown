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
// BD_ACTOR=daemon appended. PublishIdentity clears and sets the same variables
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

// PublishIdentity replaces the agent identity the daemon process inherited
// with its own: every identity variable is unset and BD_ACTOR=daemon set, so
// nothing the daemon spawns is attributed to the agent session that started
// it (GH#3006). runDaemonRun calls it before New.
func PublishIdentity() {
	for _, k := range agentconfig.IdentityEnvVars {
		processEnv{}.Unsetenv(k)
	}
	processEnv{}.Setenv("BD_ACTOR", daemonActor)
}

// polecatImageTrimContextEnv names the engine the polecat-image plugin trims
// beside the default one (gt-bxp17).
const polecatImageTrimContextEnv = "POLECAT_IMAGE_TRIM_CONTEXT"

// applyDaemonEnv publishes the patrol config's env to this process and, with
// it, the engine the polecat-image plugin trims beside the default one: the
// plugin runs as a child of the daemon and reads the context from there
// (gt-bxp17). A secret's value is redacted in the log line, never printed.
func applyDaemonEnv(cfg *DaemonPatrolConfig, source string, setenv func(string, string), logf func(string, ...any)) {
	if cfg == nil {
		return
	}
	for k, v := range cfg.Env {
		setenv(k, v)
		if agentconfig.LooksLikeSecret(k, v) {
			logf("Set env %s=<redacted> from %s", k, source)
		} else {
			logf("Set env %s=%s from %s", k, v, source)
		}
	}
	if trimContext := polecatImageTrimContext(cfg); trimContext != "" {
		setenv(polecatImageTrimContextEnv, trimContext)
		logf("Set env %s=%s from %s (the tier sweep runner's context)", polecatImageTrimContextEnv, trimContext, source)
	}
}
