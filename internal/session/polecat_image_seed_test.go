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

// The image carries no seeded Go caches: a cold cluster build of it died with an
// out-of-memory error inside the seed RUN, which compiled every rig (gt-yvnsa).
// The shared cache claim carries what the seed built now, and the openshell
// contract points Go at it (gt-nposw), so the image needs neither the RUN nor
// its sources.
func TestDockerfileHasNoSeedRun(t *testing.T) {
	t.Parallel()
	for _, in := range dockerfileInstructions(t) {
		if strings.Contains(in, "--mount=type=bind,source=seed") {
			t.Errorf("the Dockerfile bind-mounts the seed sources again: %q", in)
		}
		if strings.HasPrefix(in, "COPY ") && strings.Contains(in, "seed") {
			t.Errorf("%q copies the seed sources into the image; they belong to the cache claim, not to an image layer", in)
		}
	}
}

// The seed RUN's removal must leave the Go toolchain install and the
// golangci-lint install with its cleanup in place, and their pins unchanged:
// make lint refuses to run without golangci-lint, and every sandbox builds with
// the image's Go (gt-yvnsa).
func TestDockerfileKeepsGoToolchainAndLint(t *testing.T) {
	t.Parallel()
	ins := dockerfileInstructions(t)
	args := map[string]string{}
	var goInstall, lintInstall, floorCheck bool
	for _, in := range ins {
		if rest, ok := strings.CutPrefix(in, "ARG "); ok {
			if k, v, ok := strings.Cut(rest, "="); ok {
				args[k] = strings.Trim(v, `"`)
			}
			continue
		}
		if !strings.HasPrefix(in, "RUN ") {
			continue
		}
		switch {
		case strings.Contains(in, "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz"):
			goInstall = true
			for _, want := range []string{"sha256sum -c -", "tar -C /usr/local -xzf /tmp/go.tgz"} {
				if !strings.Contains(in, want) {
					t.Errorf("the Go install RUN lacks %q, so the tarball is neither verified nor unpacked to /usr/local: %q", want, in)
				}
			}
		case strings.Contains(in, `go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}"`):
			lintInstall = true
			if !strings.Contains(in, "rm -rf /root/go /root/.cache") {
				t.Errorf("the golangci-lint install RUN lacks its `rm -rf /root/go /root/.cache` cleanup: %q", in)
			}
		case strings.Contains(in, `go-floor.sh check "$GO_FLOOR"`):
			floorCheck = true
		}
	}
	if !goInstall {
		t.Error("no RUN installs Go from the pinned go.dev tarball")
	}
	if !lintInstall {
		t.Error("no RUN installs golangci-lint at GOLANGCI_LINT_VERSION")
	}
	if !floorCheck {
		t.Error(`no RUN checks the image's Go against "$GO_FLOOR"`)
	}
	for key, want := range map[string]string{
		"GO_VERSION":            "1.27.1",
		"GO_SHA256_ARM64":       "3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec",
		"GO_SHA256_AMD64":       "63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445",
		"GOLANGCI_LINT_VERSION": "v2.13.2",
	} {
		if got, ok := args[key]; !ok {
			t.Errorf("the Dockerfile has no ARG %s", key)
		} else if got != want {
			t.Errorf("ARG %s = %q, want %q: the removal left the pins alone", key, got, want)
		}
	}
}
