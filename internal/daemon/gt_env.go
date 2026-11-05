package daemon

import (
	"fmt"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
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

// requiredDoltEndpointEnv are the variables a plugin's bd needs to reach the
// town's server from inside the container. Unset, bd asks 127.0.0.1:3307,
// which is the container itself (gt-8txy8).
var requiredDoltEndpointEnv = []string{"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT"}

// doltEndpointKeys are every variable bd reads to find its server. A daemon
// environment that sets any of them names an endpoint as a whole, and that
// endpoint is used whole: its host with the town's port would name a server
// neither meant (gt-8txy8).
var doltEndpointKeys = []string{"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"}

// pluginChildEnv returns the environment a script-type plugin child runs
// with: the daemon's own, its identity replaced, and in container mode the
// town's Dolt endpoint.
//
// The endpoint comes from beads.ResolvedDoltEndpointEnv, the resolution the
// daemon's own bd calls use, so a plugin that runs bd meets the same server
// they do. An endpoint the daemon environment already names wins whole.
// Outside container mode the environment is daemonGTEnv's unchanged; a run
// that would have no endpoint is refused rather than started against bd's
// default (gt-8txy8).
func pluginChildEnv(env scriptEnv, townRoot string) ([]string, error) {
	child := daemonGTEnv(env.environ)
	if !env.inContainer {
		return child, nil
	}
	if !namesDoltEndpoint(env.environ) {
		if endpoint, ok := beads.ResolvedDoltEndpointEnv(townRoot, env.inContainer); ok {
			for _, kv := range endpoint {
				key, _, _ := strings.Cut(kv, "=")
				child = append(filterEnvKey(child, key), kv)
			}
		}
	}
	var missing []string
	for _, key := range requiredDoltEndpointEnv {
		if value, set := lookupEnvIn(child, key); !set || value == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("container mode needs the town's Dolt endpoint for a plugin child: %s unset (set dolt.port and a host the container reaches in mayor/town.json)",
			strings.Join(missing, " and "))
	}
	return child, nil
}

// namesDoltEndpoint reports whether env sets any variable bd reads for its
// server to a non-empty value; an empty value names no server.
func namesDoltEndpoint(env []string) bool {
	for _, key := range doltEndpointKeys {
		if value, set := lookupEnvIn(env, key); set && value != "" {
			return true
		}
	}
	return false
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
