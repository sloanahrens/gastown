package session

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// dockerfileInstructions returns the polecat Dockerfile's instructions with
// continuation lines joined, one string each.
func dockerfileInstructions(t *testing.T) []string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "polecat", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\\\n", " "), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// dockerfileEnv returns the image environment as the Dockerfile's last
// assignment of each key.
func dockerfileEnv(t *testing.T) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, in := range dockerfileInstructions(t) {
		rest, ok := strings.CutPrefix(in, "ENV ")
		if !ok {
			continue
		}
		for _, kv := range strings.Fields(rest) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			env[k] = strings.Trim(v, `"`)
		}
	}
	return env
}

// seedRun returns the shell command of the Dockerfile's seed RUN.
func seedRun(t *testing.T) string {
	t.Helper()
	const prefix = "RUN --mount=type=bind,source=seed,target=/seed "
	var seed string
	for _, in := range dockerfileInstructions(t) {
		if cmd, ok := strings.CutPrefix(in, prefix); ok {
			seed = cmd
		}
	}
	if seed == "" {
		t.Fatal("no seed RUN (RUN --mount=type=bind,source=seed,target=/seed ...) in deploy/polecat/Dockerfile")
	}
	return seed
}

// The seed builds every module it finds under /seed. Nested example modules are
// in no rig's presubmit build, so the seed has no use for them, and one whose
// go.sum has drifted fails the whole image: the beads pin's
// examples/library-usage and examples/bd-example-extension-go did, on every
// install from 10:28 (gt-9yqcc). So the walk skips them, as it already skips
// testdata.
func TestPolecatImageSeedSkipsExampleModules(t *testing.T) {
	t.Parallel()
	seed := seedRun(t)
	find := seed
	i := strings.Index(find, "find /seed -name go.mod")
	if i < 0 {
		t.Fatalf("the seed RUN does not walk the modules with `find /seed -name go.mod`:\n%s", seed)
	}
	find = find[i:]
	if j := strings.Index(find, "| sort"); j >= 0 {
		find = find[:j]
	}
	for _, excluded := range []string{"*/testdata/*", "*/examples/*"} {
		want := "-not -path '" + excluded + "'"
		if !strings.Contains(find, want) {
			t.Errorf("the seed's walk lacks %q, so it builds the modules under %s: one bad go.sum there fails every install (gt-9yqcc)\n%s", want, excluded, find)
		}
	}
	// The RUN's only report is this tail. At 5 lines a `go build` failure shows
	// the closing lines of its report and not the error that caused them, which is
	// all the 10:28 install log had.
	if !strings.Contains(seed, "tail -n 40 /tmp/seed.log") {
		t.Errorf("the seed RUN does not tail 40 lines of its failure log, so the install log cannot show the failing step's first error lines:\n%s", seed)
	}
}

// The image carries seeded Go caches (gt-ieuvf) so a cold sandbox's presubmit
// does not recompile its dependencies (devops: 967 compiles and a 4.8-10 GB peak
// cold, 66 compiles and 1.2 GB seeded). The caches sit OUTSIDE /sandbox: the
// sandbox's workspace-init container copies the image's /sandbox into each
// sandbox's volume, so a seed under it would cost gigabytes of copy and disk per
// sandbox, while a path outside is one shared image layer.
//
// Each path is also one the sandbox policy grants read_write (deploy/polecat/
// policy.yaml, hq-gi754): both in /tmp, neither under the denied /opt. A writable
// path is what lets a sandbox fetch a module the seed lacks (gt-pvudz).
func TestPolecatImageSeedSetsCachesOutsideTheWorkspace(t *testing.T) {
	t.Parallel()
	ins := dockerfileInstructions(t)
	env := dockerfileEnv(t)
	for _, want := range []struct{ key, path string }{
		{"GOMODCACHE", "/tmp/gomodcache"},
		{"GOCACHE", "/tmp/gocache"},
	} {
		got := env[want.key]
		if got != want.path {
			t.Errorf("%s = %q, want %q: the seed must sit on a path the sandbox policy grants and outside /sandbox", want.key, got, want.path)
		}
		if got == "/sandbox" || strings.HasPrefix(got, "/sandbox/") {
			t.Errorf("%s = %s is under /sandbox, which the sandbox copies into every volume; keep the seed outside it", want.key, got)
		}
	}
	// The sandbox policy denies /opt (hq-gi754): no instruction may reach into it.
	for _, in := range ins {
		if strings.Contains(in, "/opt/") {
			t.Errorf("instruction uses /opt, which the sandbox policy denies: %q", in)
		}
	}

	seedAt, goAt := -1, -1
	for i, in := range ins {
		if strings.HasPrefix(in, "RUN ") && strings.Contains(in, "--mount=type=bind,source=seed,target=/seed") {
			seedAt = i
		}
		if strings.HasPrefix(in, "RUN ") && strings.Contains(in, "go.dev/dl/go") {
			goAt = i
		}
		if strings.HasPrefix(in, "COPY ") && strings.Contains(in, "seed") {
			t.Errorf("%q copies the seed sources into the image; the seed RUN must bind-mount them instead", in)
		}
	}
	if seedAt < 0 {
		t.Fatal("no RUN bind-mounts the staged seed sources (--mount=type=bind,source=seed,target=/seed)")
	}
	if goAt < 0 || seedAt < goAt {
		t.Errorf("the seed RUN (instruction %d) must come after the Go toolchain install (instruction %d)", seedAt, goAt)
	}
	seed := ins[seedAt]
	// What each rig's presubmit runs, with the default flags: a -trimpath or a
	// different CGO setting here would give every action a different key.
	for _, want := range []string{"go build ./...", "go vet ./...", "go test -count=1 -run", "-modcacherw", "chmod -R a+rwX"} {
		if !strings.Contains(seed, want) {
			t.Errorf("the seed RUN lacks %q", want)
		}
	}
	// A linux build fetches only the modules linux imports, so the seed runs go
	// mod download first. Without it the sandbox finds its read-only module cache
	// missing a module the seed never fetched.
	if !strings.Contains(seed, "go mod download") {
		t.Errorf("the seed RUN lacks %q", "go mod download")
	}
	// Before adding a platform step or a build flag to the seed RUN, read why it
	// has no darwin build: the seed comment in deploy/polecat/Dockerfile (dv-0qre).
	for _, bad := range []string{"-trimpath", "CGO_ENABLED", "GOOS=darwin"} {
		if strings.Contains(seed, bad) {
			t.Errorf("the seed RUN sets %q, which changes the cache keys the presubmits use", bad)
		}
	}
	// The chmod is in the seed's own instruction (checked above); a later RUN
	// that touches a cache would copy every file into another layer.
	touches := []string{"$GOCACHE", "$GOMODCACHE"}
	for _, key := range []string{"GOCACHE", "GOMODCACHE"} {
		if v := env[key]; v != "" {
			touches = append(touches, v)
		}
	}
	for i, in := range ins {
		if i <= seedAt || !strings.HasPrefix(in, "RUN ") {
			continue
		}
		for _, path := range touches {
			if strings.Contains(in, path) {
				t.Errorf("instruction %d touches the seeded cache %s after the seed layer (it would duplicate it): %q", i, path, in)
			}
		}
	}
}
