//go:build integration

package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The seed RUN in deploy/polecat/Dockerfile is run here against scratch Go
// modules in place of /seed, with real sh and a real go: a module that builds
// seeds the caches and leaves them writable for another uid, a module that does
// not compile fails the image build, and a module only vet objects to is
// seeded anyway. That checks what the instruction does, not how it is spelled
// (TestPolecatImageSeedSetsCachesOutsideTheWorkspace covers its wording).

// seedInstruction returns the shell command of the Dockerfile's seed RUN.
func seedInstruction(t *testing.T) string {
	t.Helper()
	const prefix = "RUN --mount=type=bind,source=seed,target=/seed "
	for _, in := range dockerfileInstructions(t) {
		if cmd, ok := strings.CutPrefix(in, prefix); ok {
			return cmd
		}
	}
	t.Fatal("no seed RUN (RUN --mount=type=bind,source=seed,target=/seed ...) in deploy/polecat/Dockerfile")
	return ""
}

func runSeed(t *testing.T, files map[string]string) (out string, err error, cache, mod string) {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	for name, body := range files {
		p := filepath.Join(seed, "rig", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache, mod = filepath.Join(root, "gocache"), filepath.Join(root, "gomod")
	// /seed is the bind mount; /tmp/seed.log is the RUN's own log file.
	script := strings.ReplaceAll(seedInstruction(t), "find /seed ", "find "+seed+" ")
	cmd := exec.Command("sh", "-c", script)
	// The image is linux/arm64, so its linux steps build for that target whatever
	// host runs this test; the darwin step sets its own GOOS.
	cmd.Env = append(os.Environ(), "GOCACHE="+cache, "GOMODCACHE="+mod, "GOFLAGS=", "GOTOOLCHAIN=local", "GOOS=linux", "GOARCH=arm64")
	b, err := cmd.CombinedOutput()
	return string(b), err, cache, mod
}

const seedGoMod = "module example.invalid/seedfx\n\ngo 1.21\n"

func TestIntegrationSeedRunSeedsAndOpensTheCaches(t *testing.T) {
	t.Parallel()
	out, err, cache, mod := runSeed(t, map[string]string{
		"go.mod":  seedGoMod,
		"main.go": "package main\n\nfunc main() { println(\"hi\") }\n",
	})
	if err != nil {
		t.Fatalf("seed RUN failed: %v\n%s", err, out)
	}
	n := 0
	_ = filepath.WalkDir(cache, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	if n == 0 {
		t.Errorf("the seed left no files in GOCACHE %s\n%s", cache, out)
	}
	for _, dir := range []string{cache, mod, filepath.Join(cache, "00")} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Errorf("stat %s: %v", dir, err)
			continue
		}
		if fi.Mode().Perm()&0o022 != 0o022 {
			t.Errorf("%s is mode %v: another uid cannot write new cache entries there", dir, fi.Mode().Perm())
		}
	}
}

func TestIntegrationSeedRunFailsTheImageWhenTheBuildFails(t *testing.T) {
	t.Parallel()
	out, err, _, _ := runSeed(t, map[string]string{
		"go.mod":  seedGoMod,
		"main.go": "package main\n\nfunc main() { undefinedCall() }\n",
	})
	if err == nil {
		t.Fatalf("seed RUN exited 0 on a module that does not compile; the image would publish an unseeded cache\n%s", out)
	}
	if !strings.Contains(out, "'go build ./...' failed") {
		t.Errorf("the failure does not name the go build step:\n%s", out)
	}
}

func TestIntegrationSeedRunToleratesAVetOnlyFailure(t *testing.T) {
	t.Parallel()
	out, err, cache, _ := runSeed(t, map[string]string{
		"go.mod":  seedGoMod,
		"main.go": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Printf(\"%d\\n\", \"not a number\") }\n",
	})
	if err != nil {
		t.Fatalf("seed RUN failed on a module only vet objects to: %v\n%s", err, out)
	}
	if !strings.Contains(out, "'go vet ./...' failed") {
		t.Errorf("a vet failure is not reported in the build log:\n%s", out)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("no GOCACHE after a vet-only failure: %v", err)
	}
}

// Before adding a darwin step to the seed, read why it has none: the seed
// comment in deploy/polecat/Dockerfile (dv-0qre).
func TestIntegrationSeedRunDoesNotBuildForDarwin(t *testing.T) {
	t.Parallel()
	out, err, _, _ := runSeed(t, map[string]string{
		"go.mod":           seedGoMod,
		"main.go":          "package main\n\nfunc main() { println(\"hi\") }\n",
		"broken_darwin.go": "//go:build darwin\n\npackage main\n\nvar _ = undefinedCall\n",
	})
	if err != nil {
		t.Fatalf("seed RUN failed on a darwin-only broken file: %v\n%s", err, out)
	}
	if strings.Contains(out, "GOOS=darwin") {
		t.Errorf("the seed RUN still runs a darwin step:\n%s", out)
	}
}

// The image is linux: a linux-only compile error must fail it, whatever host runs
// the test. This is the converse of the darwin case above, and it pins the target
// the seed's linux steps build for.
func TestIntegrationSeedRunBuildsForTheImageTarget(t *testing.T) {
	t.Parallel()
	out, err, _, _ := runSeed(t, map[string]string{
		"go.mod":          seedGoMod,
		"main.go":         "package main\n\nfunc main() { println(\"hi\") }\n",
		"broken_linux.go": "//go:build linux\n\npackage main\n\nvar _ = undefinedCall\n",
	})
	if err == nil {
		t.Fatalf("seed RUN passed a linux-only compile error: the linux steps are not building for linux\n%s", out)
	}
	if !strings.Contains(out, "'go build ./...' failed") {
		t.Errorf("the linux build failure is not reported at the build step:\n%s", out)
	}
}

// A module cache the runtime cannot complete must not publish: a go.mod the seed
// cannot resolve fails the image at the download, before any build it might have
// passed (gt-mukre). The version below is deliberately malformed.
func TestIntegrationSeedRunFailsTheImageWhenAModuleCannotBeResolved(t *testing.T) {
	t.Parallel()
	out, err, _, _ := runSeed(t, map[string]string{
		"go.mod":  "module example.invalid/seedfx\n\ngo 1.21\n\nrequire example.invalid/dep v1\n",
		"main.go": "package main\n\nfunc main() { println(\"hi\") }\n",
	})
	if err == nil {
		t.Fatalf("seed RUN exited 0 on a go.mod it could not resolve\n%s", out)
	}
	if !strings.Contains(out, "'go mod download' failed") {
		t.Errorf("the failure is not reported at the download step:\n%s", out)
	}
}
