package forgejoprov

import (
	"slices"
	"strings"
)

// gitScrubbedVars are the variables that point git at a repository. A git child
// never inherits them: run under a git hook, they would send the child's git to a
// repository other than the one it was given.
var gitScrubbedVars = []string{"GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE"}

// GitChildEnv drops the scrubbed variables from environ and keeps the rest. Every
// git child the Forgejo tools start is built from it, so no child inherits a
// credential or a repository pin. A credential is a name ending in _TOKEN. HOME and
// PATH are kept: the rig's hooks, ssh and the credential helpers need the real ones.
//
// Recorded departure (overseer, gt-tpg0p.93): the approved scrub was the admin
// token's two names; this drops every *_TOKEN name, deny by default. It is safe
// because no git child here reads a token from its environment: git takes its
// credential from the remote URL or from a credential helper, and the helper
// (gt git-credential-forgejo) reads a token file named by flag.
// TestIntegrationGitAuthenticatesWithTheTokenVariablesStripped proves a real git
// call still authenticates through gitCommandIn with the variables stripped.
func GitChildEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !isGitScrubbed(kv) {
			out = append(out, kv)
		}
	}
	return out
}

// isGitScrubbed reports whether one KEY=VALUE entry must not reach a git child.
func isGitScrubbed(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	return strings.HasSuffix(key, "_TOKEN") || slices.Contains(gitScrubbedVars, key)
}
