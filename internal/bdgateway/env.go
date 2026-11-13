package bdgateway

import "fmt"

// callerEnv is the environment the gateway forwards from the caller: exactly
// the names the design (docs/design/bd-container.md, "Environment allowlist")
// accepts. Everything else, including the gateway's own BEADS_DIR, the Dolt
// endpoint, the tracker credential and HOME, is the gateway's to compute and is
// never taken from a request.
//
// A name outside the list is dropped, never forwarded, so a caller with a stale
// environment still gets its call rather than an error about a variable it did
// not mean to send. The design's protocol paragraph says the gateway refuses
// such a name; this policy drops it. BD_ALLOW_NEW_DATABASE therefore never
// reaches bd, and init is not served to any class anyway.
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
