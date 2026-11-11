package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// configEnvShadowedKey is the attention item's key: one item names every
// shadowed key, so the key is fixed rather than per key.
const configEnvShadowedKey = "config-env-shadowed"

// daemonEnvSeams are the process environment the daemon.env keys are applied
// to: processEnv and os.LookupEnv in production, a fake in the tests.
type daemonEnvSeams struct {
	lookup func(key string) (string, bool)
	set    func(key, value string)
}

// applyDaemonEnv publishes the patrol config's env keys to the process
// environment, and returns the keys it did not apply, sorted.
//
// In container mode the pod spec sets variables the daemon.env may repeat, and
// the pod's value is the one in force: a key the process environment already
// sets to a different value is not applied, and is reported by name only: the
// report never logs a value, since one may hold a secret. Outside container
// mode every key is applied as before.
func applyDaemonEnv(env map[string]string, source string, inContainer bool, seams daemonEnvSeams, logf func(format string, args ...any)) []string {
	var shadowed []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		v := env[k]
		if inContainer {
			if cur, ok := seams.lookup(k); ok && cur != v {
				shadowed = append(shadowed, k)
				logf("Not applying env %s from %s: the container's environment already sets it to a different value, and the pod's value is kept", k, source)
				continue
			}
		}
		seams.set(k, v)
		if agentconfig.LooksLikeSecret(k, v) {
			logf("Set env %s=<redacted> from %s", k, source)
		} else {
			logf("Set env %s=%s from %s", k, v, source)
		}
	}
	return shadowed
}

// collectConfigEnvShadowed raises one item naming the daemon.env keys that
// applyDaemonEnv left out at start. The set is fixed for the daemon's life, so
// the item holds until the daemon restarts on a settings file without them.
func (s *attentionSources) collectConfigEnvShadowed(context.Context) ([]attention.Item, error) {
	if len(s.envShadowed) == 0 {
		return nil, nil
	}
	return []attention.Item{{
		Key:      configEnvShadowedKey,
		Kind:     attention.KindConfigEnvShadowed,
		Severity: attention.SeverityLow,
		Summary: fmt.Sprintf("settings daemon.env sets %s, but the container's environment already sets it to another value, so the pod's value is in force: remove the stale entry from settings",
			strings.Join(s.envShadowed, ", ")),
	}}, nil
}
