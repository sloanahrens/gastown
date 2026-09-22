package testutil

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreserveGoEnv_CarriesFlagsPastHomeRedirect is the regression test for
// gt-mjll.
//
// On a host with a keg-only icu4c the cgo flags live in the Go env FILE
// ($GOENV, itself under $HOME), not in the process environment. The harness
// redirects HOME into a sandbox with no env file, so every `go` subprocess a
// test spawns loses them: buildGT's `go build ./cmd/gt` then compiles Dolt's
// go-icu-regex dependency without icu4c's include path and dies with
// "file.cpp:3:10: fatal error: 'unicode/regex.h' file not found".
//
// The test models that host rather than trusting the machine it runs on: it
// seeds a go env file with sentinel flags, points HOME at it, and asserts both
// halves of the property — that the sandbox HOME really does lose the flags,
// and that preserveGoEnv carries them across the redirect.
func TestPreserveGoEnv_CarriesFlagsPastHomeRedirect(t *testing.T) {
	goenvPath := goEnvPathUnderHome(t)
	realHome := os.Getenv("HOME")
	rel, under := strings.CutPrefix(goenvPath, realHome+string(filepath.Separator))
	if !under {
		t.Skipf("GOENV %q is not under HOME %q; cannot model the redirect", goenvPath, realHome)
	}

	// Read the cache locations while HOME is still the real one, so the
	// stand-in below does not drag them into its own temp tree. They are
	// carried by the same function, and this test is about the cgo flags.
	cacheVars := map[string]string{}
	for _, v := range []string{"GOPATH", "GOCACHE", "GOMODCACHE"} {
		cacheVars[v] = goEnvValue(t, v)
	}

	devHome := t.TempDir()
	wantCPP := "-I" + filepath.Join(devHome, "icu4c", "include")
	wantLDFLAGS := "-L" + filepath.Join(devHome, "icu4c", "lib")
	seedGoEnvFile(t, filepath.Join(devHome, rel), map[string]string{
		"CGO_CPPFLAGS": wantCPP,
		"CGO_LDFLAGS":  wantLDFLAGS,
	})

	// The developer's machine, as preserveGoEnv is called on it: a HOME whose
	// env file holds the flags, with nothing exported into the environment.
	// The harness has not swapped HOME out yet.
	withSavedEnv(t)
	for v, value := range cacheVars {
		os.Setenv(v, value)
	}
	os.Setenv("HOME", devHome)
	os.Setenv("USERPROFILE", devHome)
	for _, v := range []string{"CGO_CPPFLAGS", "CGO_CFLAGS", "CGO_LDFLAGS"} {
		os.Unsetenv(v)
	}

	// The harness's sandbox: an empty HOME with no env file at all. Every
	// test-spawned `go` invocation lands here.
	sandboxHome := t.TempDir()

	// Half one — reproduce. With HOME swapped out and nothing carried, the
	// flags are simply gone. That is the bug.
	if got := goEnvWith(t, sandboxHome, "CGO_CPPFLAGS")["CGO_CPPFLAGS"]; got != "" {
		t.Fatalf("precondition failed: an empty HOME resolved CGO_CPPFLAGS=%q", got)
	}

	if err := preserveGoEnv(); err != nil {
		t.Fatalf("preserveGoEnv: %v", err)
	}

	// Half two — the fix. What the developer's env file held is now in the
	// process environment, so a `go` subprocess pointed at the sandbox HOME
	// still resolves it.
	if got := os.Getenv("CGO_CPPFLAGS"); got != wantCPP {
		t.Errorf("CGO_CPPFLAGS after preserveGoEnv = %q, want %q", got, wantCPP)
	}
	if got := os.Getenv("CGO_LDFLAGS"); got != wantLDFLAGS {
		t.Errorf("CGO_LDFLAGS after preserveGoEnv = %q, want %q", got, wantLDFLAGS)
	}
	got := goEnvWith(t, sandboxHome, "CGO_CPPFLAGS", "CGO_LDFLAGS")
	if got["CGO_CPPFLAGS"] != wantCPP {
		t.Errorf("go env CGO_CPPFLAGS under the sandbox HOME = %q, want %q: a test-spawned `go build` still loses icu4c",
			got["CGO_CPPFLAGS"], wantCPP)
	}
	if got["CGO_LDFLAGS"] != wantLDFLAGS {
		t.Errorf("go env CGO_LDFLAGS under the sandbox HOME = %q, want %q", got["CGO_LDFLAGS"], wantLDFLAGS)
	}
}

// goEnvPathUnderHome returns the path the go tool reads its env file from,
// asked with HOME as it currently stands.
func goEnvPathUnderHome(t *testing.T) string {
	t.Helper()
	return goEnvValue(t, "GOENV")
}

// goEnvValue asks the go tool for one variable's effective value.
func goEnvValue(t *testing.T, name string) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go tool on PATH: %v", err)
	}
	out, err := exec.Command(goBin, "env", name).Output()
	if err != nil {
		t.Skipf("go env %s: %v", name, err)
	}
	return strings.TrimSpace(string(out))
}

// seedGoEnvFile writes a go env file in the toolchain's own KEY=VALUE format.
func seedGoEnvFile(t *testing.T, path string, values map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	var b strings.Builder
	for k, v := range values {
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("writing go env file: %v", err)
	}
}

// goEnvWith runs `go env -json <names>` in a subprocess whose HOME is home and
// which otherwise inherits the process environment. That is what a
// test-spawned `go build` sees after the harness redirects HOME.
func goEnvWith(t *testing.T, home string, names ...string) map[string]string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go tool on PATH: %v", err)
	}
	cmd := exec.Command(goBin, append([]string{"env", "-json"}, names...)...)
	cmd.Env = envWith(map[string]string{"HOME": home, "USERPROFILE": home})
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env under HOME=%s: %v", home, err)
	}
	values := map[string]string{}
	if err := json.Unmarshal(out, &values); err != nil {
		t.Fatalf("parsing go env JSON: %v\n%s", err, out)
	}
	return values
}

// envWith returns the process environment with overrides applied. It replaces
// rather than appends, because a duplicated variable leaves the child's
// getenv() free to return either copy.
func envWith(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok {
			if _, replaced := overrides[name]; replaced {
				continue
			}
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

// TestGoEnvCarryVars_CoverCgoIncludePath guards the list itself. The carry
// list is the fix, and dropping an entry from it fails silently by
// construction — nothing else would notice until a cgo build broke.
func TestGoEnvCarryVars_CoverCgoIncludePath(t *testing.T) {
	for _, want := range []string{"GOPATH", "GOCACHE", "GOMODCACHE", "CGO_CPPFLAGS", "CGO_CFLAGS", "CGO_LDFLAGS"} {
		found := false
		for _, v := range goEnvCarryVars {
			if v == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("goEnvCarryVars is missing %s: the HOME redirect drops it for every `go` subprocess", want)
		}
	}
}

// TestVerifyCgoIncludePath_FailsLoudOnMissingIcu4c pins the loud half of
// gt-mjll: when the configured include path cannot resolve icu4c's header, the
// error must name the header, the offending directory, and the repair —
// instead of letting cgo report it from inside a dependency's generated C++.
func TestVerifyCgoIncludePath_FailsLoudOnMissingIcu4c(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-icu4c")
	t.Setenv("CGO_CPPFLAGS", "-I"+missing)
	t.Setenv("CGO_CFLAGS", "")

	err := verifyCgoIncludePath()
	if err == nil {
		// Distinguish "the check declined to fire because this host finds the
		// header by default" from "the check is gone": the former is a skip,
		// the latter is the bug this test exists to catch.
		if systemIcu4cResolvable(t) {
			t.Skip("icu4c is on the toolchain's default search path, so a bogus -I cannot make the probe fail")
		}
		t.Fatal("verifyCgoIncludePath accepted an include path with no icu4c in it: the cgo build would fail obscurely instead")
	}
	for _, want := range []string{icu4cHeader, missing, "brew"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// systemIcu4cResolvable reports whether the C toolchain finds icu4c's header
// with no include flags at all.
func systemIcu4cResolvable(t *testing.T) bool {
	t.Helper()
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "cc"
	}
	if _, err := exec.LookPath(cc); err != nil {
		return false
	}
	probe := exec.Command(cc, "-fsyntax-only", "-x", "c", "-")
	probe.Stdin = strings.NewReader("#include " + icu4cHeader + "\n")
	return probe.Run() == nil
}

// TestVerifyCgoIncludePath_SkipsWithoutIncludeDirs pins the gate. A host that
// configures no include directory loses nothing to the HOME redirect, and must
// not be blocked by a diagnosis the harness cannot make.
//
// The regression this guards is real: CGO_CFLAGS defaults to "-O2 -g" on every
// host, so gating on "any cgo flag at all" would run the probe everywhere and
// fail every hermetic package on a machine whose icu4c sits on the toolchain's
// default search path.
func TestVerifyCgoIncludePath_SkipsWithoutIncludeDirs(t *testing.T) {
	t.Setenv("CGO_CPPFLAGS", "")
	t.Setenv("CGO_CFLAGS", "-O2 -g")

	if err := verifyCgoIncludePath(); err != nil {
		t.Errorf("verifyCgoIncludePath with no -I = %v, want nil", err)
	}
}

// TestVerifyCgoIncludePath_AcceptsResolvableHeader is the negative control: an
// include path that does resolve the header must not trip the check. Without
// it, a probe that failed for any reason at all would still look green.
func TestVerifyCgoIncludePath_AcceptsResolvableHeader(t *testing.T) {
	real := goEnvValue(t, "CGO_CPPFLAGS")
	if real == "" {
		t.Skip("host needs no cgo include path; there is nothing for the probe to accept")
	}
	t.Setenv("CGO_CPPFLAGS", real)
	t.Setenv("CGO_CFLAGS", "")

	if err := verifyCgoIncludePath(); err != nil {
		t.Errorf("verifyCgoIncludePath with the host's resolvable include path = %v, want nil", err)
	}
}
