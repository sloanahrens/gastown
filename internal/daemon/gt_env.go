package daemon

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
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
		endpoint, ok, err := beads.ResolvedDoltEndpointEnv(townRoot, env.inContainer)
		if err != nil {
			return nil, fmt.Errorf("container mode needs the town's Dolt endpoint for a plugin child: mayor/town.json does not load: %w", err)
		}
		if ok {
			for _, kv := range endpoint {
				key, _, _ := strings.Cut(kv, "=")
				child = append(filterEnvKey(child, key), kv)
			}
		}
	}
	if err := checkDoltEndpointEnv(child); err != nil {
		return nil, fmt.Errorf("container mode needs the town's Dolt endpoint for a plugin child: %w", err)
	}
	return child, nil
}

// maxDoltPort is the highest TCP port an endpoint can name.
const maxDoltPort = 65535

// checkDoltEndpointEnv names the first endpoint variable in child that cannot
// reach a server: a host that is missing or empty, or a port that is missing,
// empty, not ASCII digits, zero or above maxDoltPort. The error names the
// variable and the problem, never the value, since a port may come from a
// variable that carries a secret.
func checkDoltEndpointEnv(child []string) error {
	host, set := lookupEnvIn(child, "BEADS_DOLT_SERVER_HOST")
	switch {
	case !set:
		return errors.New("BEADS_DOLT_SERVER_HOST is missing")
	case host == "":
		return errors.New("BEADS_DOLT_SERVER_HOST is empty")
	}
	port, set := lookupEnvIn(child, "BEADS_DOLT_SERVER_PORT")
	if !set {
		return errors.New("BEADS_DOLT_SERVER_PORT is missing")
	}
	if port == "" {
		return errors.New("BEADS_DOLT_SERVER_PORT is empty")
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return errors.New("BEADS_DOLT_SERVER_PORT is not a number")
		}
	}
	n, err := strconv.Atoi(port)
	switch {
	case err != nil || n > maxDoltPort:
		return errors.New("BEADS_DOLT_SERVER_PORT is out of range")
	case n == 0:
		return errors.New("BEADS_DOLT_SERVER_PORT is zero")
	}
	return nil
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
