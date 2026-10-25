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

// The image carries seeded Go caches (gt-ieuvf) so a cold sandbox's presubmit
// does not recompile its dependencies (devops: 967 compiles and a 4.8-10 GB peak
// cold, 66 compiles and 1.2 GB seeded). The caches sit OUTSIDE /sandbox: the
// sandbox's workspace-init container copies the image's /sandbox into each
// sandbox's volume, so a seed under it would cost gigabytes of copy and disk per
// sandbox, while a path outside is one shared image layer.
func TestPolecatDockerfileSeedsGoCachesOutsideTheWorkspace(t *testing.T) {
	t.Parallel()
	ins := dockerfileInstructions(t)

	env := map[string]string{}
	for _, in := range ins {
		if rest, ok := strings.CutPrefix(in, "ENV "); ok {
			for _, kv := range strings.Fields(rest) {
				if k, v, ok := strings.Cut(kv, "="); ok {
					env[k] = v
				}
			}
		}
	}
	for _, k := range []string{"GOCACHE", "GOMODCACHE"} {
		v := env[k]
		if v == "" {
			t.Errorf("the image sets no %s", k)
		} else if v == "/sandbox" || strings.HasPrefix(v, "/sandbox/") {
			t.Errorf("%s = %s is under /sandbox, which the sandbox copies into every volume; keep the seed outside it", k, v)
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
	for _, bad := range []string{"-trimpath", "CGO_ENABLED"} {
		if strings.Contains(seed, bad) {
			t.Errorf("the seed RUN sets %q, which changes the cache keys the presubmits use", bad)
		}
	}
	// The chmod is in the seed's own instruction (checked above); a later RUN
	// that re-chmods the cache would copy every file into another layer.
	for i, in := range ins {
		if i > seedAt && strings.HasPrefix(in, "RUN ") && (strings.Contains(in, "/opt/gocache") || strings.Contains(in, "/opt/gomod") || strings.Contains(in, "$GOCACHE") || strings.Contains(in, "$GOMODCACHE")) {
			t.Errorf("instruction %d touches the seeded caches after the seed layer (it would duplicate them): %q", i, in)
		}
	}
}
