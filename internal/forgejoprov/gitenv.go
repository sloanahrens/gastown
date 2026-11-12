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
