package session

import (
	"fmt"
	"strings"
	"testing"
)

// goCacheGrants maps each Go cache the image sets to the filesystem_policy list
// that must grant it: a sandbox writes both, the build cache as it compiles
// (gt-ieuvf) and the module cache to fetch a module the seed lacks (gt-pvudz).
var goCacheGrants = []struct{ env, grant string }{
	{"GOMODCACHE", "read_write"},
	{"GOCACHE", "read_write"},
}

// goCacheProblems reports every way the Dockerfile's Go cache paths disagree
// with the sandbox policy: a cache under /opt, which a sandbox denies, or one
// the policy does not grant at the mode the sandbox mounts it (hq-gi754).
func goCacheProblems(policy map[string]any, env map[string]string) ([]string, error) {
	grants := make(map[string][]string, len(goCacheGrants))
	for _, cache := range goCacheGrants {
		paths, err := parsePolicyPaths(policy, cache.grant)
		if err != nil {
			return nil, err
		}
		grants[cache.grant] = paths
	}
	var problems []string
	for _, cache := range goCacheGrants {
		path := env[cache.env]
		if path == "" {
			problems = append(problems, fmt.Sprintf("the Dockerfile sets no %s", cache.env))
			continue
		}
		if pathWithin(path, "/opt") {
			problems = append(problems, fmt.Sprintf("%s = %s is under /opt, which a sandbox denies", cache.env, path))
		}
		if !policyGrants(grants[cache.grant], path) {
			problems = append(problems, fmt.Sprintf("%s = %s is not under a filesystem_policy.%s entry: %v", cache.env, path, cache.grant, grants[cache.grant]))
		}
	}
	return problems, nil
}

// The Dockerfile names each cache path and the policy grants a directory above
// it; only a test that reads both catches a cache a sandbox would deny, before
// a sandbox runs (hq-gi754).
func TestPolecatImageGoCachesAreGrantedByThePolicy(t *testing.T) {
	t.Parallel()
	problems, err := goCacheProblems(polecatPolicy(t), dockerfileEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

// The check must catch the moves that break a sandbox: a cache back under /opt,
// a cache the policy grants read-only only, or a cache the Dockerfile stops
// setting (hq-gi754).
func TestGoCacheProblemsRejectsCachesThePolicyDoesNotGrant(t *testing.T) {
	t.Parallel()
	policy := func(readOnly, readWrite []string) map[string]any {
		list := func(paths []string) []any {
			out := make([]any, len(paths))
			for i, p := range paths {
				out[i] = p
			}
			return out
		}
		return map[string]any{"filesystem_policy": map[string]any{
			"read_only":  list(readOnly),
			"read_write": list(readWrite),
		}}
	}
	env := func(moduleCache, buildCache string) map[string]string {
		return map[string]string{"GOMODCACHE": moduleCache, "GOCACHE": buildCache}
	}
	cases := []struct {
		name   string
		policy map[string]any
		env    map[string]string
		want   string // env var the problem must name; "" wants no problem
	}{
		{"granted", policy([]string{"/usr"}, []string{"/tmp"}), env("/tmp/gomodcache", "/tmp/gocache"), ""},
		{"module cache under /opt", policy([]string{"/opt"}, []string{"/tmp"}), env("/opt/gomod", "/tmp/gocache"), "GOMODCACHE"},
		{"build cache under /opt", policy([]string{"/usr"}, []string{"/opt"}), env("/tmp/gomodcache", "/opt/gocache"), "GOCACHE"},
		{"module cache under read_only", policy([]string{"/usr"}, []string{"/tmp"}), env("/usr/gomodcache", "/tmp/gocache"), "GOMODCACHE"},
		{"build cache under read_only", policy([]string{"/usr"}, []string{"/tmp"}), env("/tmp/gomodcache", "/usr/gocache"), "GOCACHE"},
		{"no GOCACHE", policy([]string{"/usr"}, []string{"/tmp"}), env("/tmp/gomodcache", ""), "GOCACHE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems, err := goCacheProblems(tc.policy, tc.env)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(problems) != 0 {
					t.Errorf("goCacheProblems = %v, want none", problems)
				}
				return
			}
			for _, problem := range problems {
				if strings.Contains(problem, tc.want) {
					return
				}
			}
			t.Errorf("goCacheProblems = %v, want a problem naming %s", problems, tc.want)
		})
	}
}
