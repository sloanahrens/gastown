package imagectx

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// fixture values are placeholders, not credentials; the test only compares them
// to what the environment carries.
var credentialHost = []string{
	"PATH=/fx/bin", "HOME=/fx/home", "GOFLAGS=-mod=mod",
	"GIT_CONFIG_GLOBAL=/fx/gitconfig", "SOURCES_USER=fx-user", "SOURCES_TOKEN=fx-token",
}

func envHas(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// TestBuildEnvGitKeepsTheCredentialHelperVariables checks the git environment
// keeps the helper's config file and its two variables when the host has them,
// carries none of them (and no empty value) when it does not, and always tells
// git never to prompt (gt-y1p1np).
func TestBuildEnvGitKeepsTheCredentialHelperVariables(t *testing.T) {
	t.Parallel()
	t.Run("carries all three and never prompts when the host has them", func(t *testing.T) {
		env := gitEnv(credentialHost)
		for _, want := range []string{"GIT_CONFIG_GLOBAL=/fx/gitconfig", "SOURCES_USER=fx-user", "SOURCES_TOKEN=fx-token", "GIT_TERMINAL_PROMPT=0"} {
			if !contains(env, want) {
				t.Errorf("git environment lacks %s", strings.SplitN(want, "=", 2)[0])
			}
		}
		if _, ok := envHas(env, "GOFLAGS"); ok {
			t.Error("git environment gained GOFLAGS, which bdbuild.Env drops")
		}
	})
	t.Run("carries none when the host has none, and no empty value", func(t *testing.T) {
		env := gitEnv([]string{"PATH=/fx/bin", "GIT_CONFIG_GLOBAL=", "SOURCES_USER=", "SOURCES_TOKEN="})
		for _, name := range gitCredentialVars {
			if _, ok := envHas(env, name); ok {
				t.Errorf("git environment carries %s with nothing set on the host", name)
			}
		}
		if !contains(env, "GIT_TERMINAL_PROMPT=0") {
			t.Error("git environment lacks GIT_TERMINAL_PROMPT=0")
		}
	})
}

// TestBuildEnvCompileDoesNotGainTheCredentialVariables checks the compile
// environment stays as it was: with the helper's variables in the host list,
// neither bdbuild.Env nor buildEnvFrom takes them.
func TestBuildEnvCompileDoesNotGainTheCredentialVariables(t *testing.T) {
	t.Parallel()
	for name, env := range map[string][]string{"bdbuild.Env": bdbuild.Env(credentialHost), "buildEnvFrom": buildEnvFrom(credentialHost)} {
		for _, key := range gitCredentialVars {
			if _, ok := envHas(env, key); ok {
				t.Errorf("%s carries %s", name, key)
			}
		}
	}
}

// TestBuildEnvBuildGivesTheHelperToGitReadsOnly checks Build hands the
// credential-helper variables to the ls-remote reads and to no compile.
func TestBuildEnvBuildGivesTheHelperToGitReadsOnly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	seams := w.seams()
	seams.Environ = credentialHost
	if _, err := Build(context.Background(), w.inputs(t.TempDir()), seams); err != nil {
		t.Fatalf("Build: %v\n%s", err, w.dump())
	}
	if len(w.gitEnvs) == 0 || len(w.goEnvs) == 0 {
		t.Fatalf("no git read or compile ran\n%s", w.dump())
	}
	for i, env := range w.gitEnvs {
		for _, key := range gitCredentialVars {
			if _, ok := envHas(env, key); !ok {
				t.Errorf("%s ran without %s", w.gitEnvCalls[i], key)
			}
		}
	}
	for _, env := range w.goEnvs {
		for _, key := range gitCredentialVars {
			if _, ok := envHas(env, key); ok {
				t.Errorf("a compile ran with %s", key)
			}
		}
	}
}
