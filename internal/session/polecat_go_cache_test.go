package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// goCacheGrants maps each Go cache a claim-mounted sandbox sets to the
// filesystem_policy list that must grant it. Both caches are writable: the build
// cache as the sandbox compiles (gt-ieuvf), and the module cache so a sandbox can
// fetch a module the seed lacks (gt-pvudz).
//
// Integrity trade-off: a writable module cache lets a sandbox change extracted
// module files that later builds in the same cache compile from. Go checks a
// module against go.sum when it downloads it, not on every build, so such a
// change is not caught by the build. The read-only module cache this replaced
// kept the seeded files unchanged; the trade-off is Sloan's to accept.
var goCacheGrants = []struct{ env, grant string }{
	{"GOMODCACHE", "read_write"},
	{"GOCACHE", "read_write"},
}

// policyIncludeWorkdir reads filesystem_policy.include_workdir: an absent key
// grants nothing, and any other shape is an error naming the field, so a
// malformed policy cannot pass as granted (gt-tmi6n).
func policyIncludeWorkdir(doc map[string]any) (bool, error) {
	fs, ok := doc["filesystem_policy"].(map[string]any)
	if !ok {
		return false, fmt.Errorf("policy has no filesystem_policy")
	}
	v, ok := fs["include_workdir"]
	if !ok || v == nil {
		return false, nil
	}
	include, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("filesystem_policy.include_workdir: want a bool, got %T", v)
	}
	return include, nil
}

// goCacheProblems reports every way the sandbox's Go cache paths disagree with
// the policy: a cache under /opt, which a sandbox denies, or one the policy does
// not grant at the mode the sandbox writes it (hq-gi754). The workdir is a
// read_write grant of its own: filesystem_policy.include_workdir includes it, and
// Landlock grants whole paths, so a cache under the workdir is writable.
func goCacheProblems(policy map[string]any, workdir string, env map[string]string) ([]string, error) {
	grants := make(map[string][]string, len(goCacheGrants))
	for _, cache := range goCacheGrants {
		paths, err := parsePolicyPaths(policy, cache.grant)
		if err != nil {
			return nil, err
		}
		grants[cache.grant] = paths
	}
	include, err := policyIncludeWorkdir(policy)
	if err != nil {
		return nil, err
	}
	if include {
		grants["read_write"] = append(grants["read_write"], workdir)
	}
	var problems []string
	for _, cache := range goCacheGrants {
		path := env[cache.env]
		if path == "" {
			problems = append(problems, fmt.Sprintf("the sandbox sets no %s", cache.env))
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

// A claim-mounted sandbox writes its Go caches on the claim, so the paths the
// create actually hands the sandbox must be writable where the claim is mounted:
// inside the image's WORKDIR, which filesystem_policy.include_workdir grants.
// Reading the path from the production create (not from the constants the
// backend is spelled with) is what catches a cache a sandbox would deny before a
// sandbox runs (hq-gi754, gt-nly1g).
func TestSandboxGoCachesAreGrantedByThePolicy(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	p := openshellTestProfile
	p.Driver = DriverKubernetes
	p.KubernetesCacheClaim = "gt-cache"
	b, _ := openshellTestBackend(town, p, &sandbox.Fake{})
	cs, err := b.createSpec(openshellSpawn(town, work), sandboxTracker{})
	if err != nil {
		t.Fatalf("createSpec: %v", err)
	}
	// The claim is mounted where the driver config says: a cache path the
	// policy grants under a directory the sandbox never gets is no use.
	if !strings.Contains(cs.DriverConfigJSON, `"mount_path":"`+sandboxCacheDir+`"`) {
		t.Fatalf("the create's driver config mounts no claim at %s: %s", sandboxCacheDir, cs.DriverConfigJSON)
	}
	problems, err := goCacheProblems(polecatPolicy(t), dockerfileWorkdir(t), map[string]string{
		"GOCACHE":    cs.Env["GOCACHE"],
		"GOMODCACHE": cs.Env["GOMODCACHE"],
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

// A malformed include_workdir stops the check with an error naming the field,
// so a policy whose grant cannot be read never passes as granted or as absent
// (gt-tmi6n).
func TestGoCacheProblemsRefusesAMalformedIncludeWorkdir(t *testing.T) {
	t.Parallel()
	policy := map[string]any{"filesystem_policy": map[string]any{"include_workdir": "true"}}
	_, err := goCacheProblems(policy, "/sandbox", map[string]string{
		"GOCACHE":    "/sandbox/.cache/gocache",
		"GOMODCACHE": "/sandbox/.cache/gomodcache",
	})
	if err == nil || !strings.Contains(err.Error(), "include_workdir") {
		t.Fatalf("goCacheProblems = %v, want an error naming include_workdir", err)
	}
}

// The check must catch the moves that break a sandbox: a cache back under /opt,
// a cache the policy grants read-only only, a cache outside the included workdir,
// or a cache the backend stops setting (hq-gi754).
func TestGoCacheProblemsRejectsCachesThePolicyDoesNotGrant(t *testing.T) {
	t.Parallel()
	policy := func(readOnly, readWrite []string, includeWorkdir bool) map[string]any {
		list := func(paths []string) []any {
			out := make([]any, len(paths))
			for i, p := range paths {
				out[i] = p
			}
			return out
		}
		fs := map[string]any{
			"read_only":  list(readOnly),
			"read_write": list(readWrite),
		}
		if includeWorkdir {
			fs["include_workdir"] = true
		}
		return map[string]any{"filesystem_policy": fs}
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
		{"granted by read_write", policy([]string{"/usr"}, []string{"/tmp"}, false), env("/tmp/gomodcache", "/tmp/gocache"), ""},
		{"granted by include_workdir", policy([]string{"/usr"}, []string{"/tmp"}, true), env("/sandbox/.cache/gomodcache", "/sandbox/.cache/gocache"), ""},
		{"workdir not included", policy([]string{"/usr"}, []string{"/tmp"}, false), env("/sandbox/.cache/gomodcache", "/sandbox/.cache/gocache"), "GOCACHE"},
		{"module cache under /opt", policy([]string{"/opt"}, []string{"/tmp"}, false), env("/opt/gomod", "/tmp/gocache"), "GOMODCACHE"},
		{"build cache under /opt", policy([]string{"/usr"}, []string{"/opt"}, false), env("/tmp/gomodcache", "/opt/gocache"), "GOCACHE"},
		{"module cache under read_only", policy([]string{"/usr"}, []string{"/tmp"}, false), env("/usr/gomodcache", "/tmp/gocache"), "GOMODCACHE"},
		{"build cache under read_only", policy([]string{"/usr"}, []string{"/tmp"}, false), env("/tmp/gomodcache", "/usr/gocache"), "GOCACHE"},
		{"no GOCACHE", policy([]string{"/usr"}, []string{"/tmp"}, false), env("/tmp/gomodcache", ""), "GOCACHE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems, err := goCacheProblems(tc.policy, "/sandbox", tc.env)
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
