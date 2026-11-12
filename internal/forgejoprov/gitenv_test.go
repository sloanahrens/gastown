package forgejoprov

import (
	"reflect"
	"testing"
)

// TestGitChildEnvDropsTheScrubbedVars checks the environment a git child is built
// from: every name ending in _TOKEN and the three variables that pin a repository
// are removed; every other variable stays, HOME and PATH included, and a name
// that only starts with a token's name is not mistaken for it. It runs no git.
func TestGitChildEnvDropsTheScrubbedVars(t *testing.T) {
	t.Parallel()
	environ := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/user",
		"GT_AUTH_FORGEJO_TOKEN=broker-token",
		"FORGEJO_ADMIN_TOKEN=admin-token",
		"FOO_TOKEN=foo-token",
		"GITHUB_TOKEN=github-token",
		"FORGEJO_ADMIN_ENV=/etc/forgejo/admin.env",
		"FORGEJO_ADMIN_TOKEN_FILE=/run/token",
		"GIT_DIR=/elsewhere/.git",
		"GIT_INDEX_FILE=/elsewhere/index",
		"GIT_WORK_TREE=/elsewhere/tree",
		"GIT_SSH_COMMAND=ssh -i /home/user/.ssh/id",
		"GIT_TERMINAL_PROMPT=0",
	}
	want := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/user",
		"FORGEJO_ADMIN_ENV=/etc/forgejo/admin.env",
		"FORGEJO_ADMIN_TOKEN_FILE=/run/token",
		"GIT_SSH_COMMAND=ssh -i /home/user/.ssh/id",
		"GIT_TERMINAL_PROMPT=0",
	}
	if got := GitChildEnv(environ); !reflect.DeepEqual(got, want) {
		t.Fatalf("GitChildEnv = %q, want %q", got, want)
	}
}
