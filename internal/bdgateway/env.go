package bdgateway

import "fmt"

// callerEnv is the environment the gateway forwards: only these names, so the
// Dolt endpoint, the tracker credential, HOME and BEADS_DIR are never taken
// from a request. A name outside the list is dropped, never forwarded, which is
// how BD_ALLOW_NEW_DATABASE stays away from bd (gt-m76aet). Before changing the
// list, read the design's "Environment allowlist" section
// (docs/design/bd-container.md), its source.
var callerEnv = map[string]bool{
	"BD_ACTOR":            true,
	"BEADS_ACTOR":         true,
	"BD_MACHINE":          true,
	"BD_JSON_ENVELOPE":    true,
	"BD_READONLY":         true,
	"BD_DOLT_AUTO_COMMIT": true,
}

// FilterEnv returns the caller environment the gateway forwards for class: the
// allowlisted names only, with the read class forcing BD_READONLY=true whatever
// the caller sent. An unknown token class is an error rather than a default,
// since a caller the gateway cannot class must not run at all.
func FilterEnv(class TokenClass, env map[string]string) (map[string]string, error) {
	if _, ok := tokenClassRank(class); !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTokenClass, string(class))
	}
	out := make(map[string]string, len(env)+1)
	for name, value := range env {
		if callerEnv[name] {
			out[name] = value
		}
	}
	if class == TokenRead {
		out["BD_READONLY"] = "true"
	}
	return out, nil
}
