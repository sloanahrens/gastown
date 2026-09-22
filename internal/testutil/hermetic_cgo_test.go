package testutil

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPreserveGoEnv_CarriesFlagsPastHomeRedirect is the regression test for
// gt-mjll.
//
// On a host with a keg-only icu4c the cgo flags live in the Go env FILE
// ($GOENV, itself under $HOME — or under $XDG_CONFIG_HOME on Linux), not in
// the process environment. The harness redirects HOME into a sandbox with no
// env file, so every `go` subprocess a test spawns loses them: buildGT's
// `go build ./cmd/gt` then compiles Dolt's go-icu-regex dependency without
// icu4c's include path and dies with "file.cpp:3:10: fatal error:
// 'unicode/regex.h' file not found".
//
// The test models that host rather than trusting the machine it runs on: it
// seeds a go env file with sentinel flags, pins GOENV to it, and asserts both
// halves of the property — that the sandbox HOME really does lose the flags,
// and that preserveGoEnv carries them across the redirect.
func TestPreserveGoEnv_CarriesFlagsPastHomeRedirect(t *testing.T) {
	// The host's real go env file, asked with the real HOME, must hold no
	// carried variable: once the stand-in HOME is in place, a leftover real
	// file would resolve the "missing" values and break the precondition.
	if realEnv := goEnvValue(t, "GOENV"); realEnv != "" {
		for _, v := range goEnvCarryVars {
			if v == "GOENV" {
				continue
			}
			if real := goEnvValue(t, v); real != "" {
				t.Skipf("the host's real go env file %s already sets %s=%q; cannot model an empty file", realEnv, v, real)
			}
		}
	}

	// Read the cache locations while HOME is still the real one, so the
	// stand-in below does not drag them into its own temp tree. They are
	// carried by the same function, and this test is about the cgo flags.
	cacheVars := map[string]string{}
	for _, v := range []string{"GOPATH", "GOCACHE", "GOMODCACHE"} {
		cacheVars[v] = goEnvValue(t, v)
	}

	devHome := t.TempDir()
	seedGoEnvFile(t, filepath.Join(devHome, "go-env"), map[string]string{
		"CGO_CPPFLAGS": "-I" + filepath.Join(devHome, "icu4c", "include"),
		"CGO_CXXFLAGS": "-I" + filepath.Join(devHome, "icu4c", "include"),
		"CGO_LDFLAGS":  "-L" + filepath.Join(devHome, "icu4c", "lib"),
	})

	// The developer's machine, as preserveGoEnv is called on it: a HOME and
	// XDG_CONFIG_HOME holding the seeded env file, GOENV pinned to it, and
	// nothing exported into the environment. The harness has not swapped
	// HOME out yet.
	withSavedEnv(t)
	for v, value := range cacheVars {
		os.Setenv(v, value)
	}
	home := map[string]string{"HOME": devHome, "USERPROFILE": devHome, "XDG_CONFIG_HOME": filepath.Join(devHome, ".config")}
	for k, v := range home {
		os.Setenv(k, v)
	}
	os.Setenv("GOENV", filepath.Join(devHome, "go-env"))
	for _, v := range goEnvCarryVars {
		if v == "GOENV" {
			continue
		}
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
	want := map[string]string{
		"CGO_CPPFLAGS": "-I" + filepath.Join(devHome, "icu4c", "include"),
		"CGO_CXXFLAGS": "-I" + filepath.Join(devHome, "icu4c", "include"),
		"CGO_LDFLAGS":  "-L" + filepath.Join(devHome, "icu4c", "lib"),
	}
	for name, w := range want {
		if got := os.Getenv(name); got != w {
			t.Errorf("%s after preserveGoEnv = %q, want %q", name, got, w)
		}
	}
	got := goEnvWith(t, sandboxHome, "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS")
	for name, w := range want {
		if got[name] != w {
			t.Errorf("go env %s under the sandbox HOME = %q, want %q: a test-spawned `go build` still loses icu4c", name, got[name], w)
		}
	}
}

// gotool returns the go tool's path, skipping the test when there is none.
func gotool(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go tool on PATH: %v", err)
	}
	return goBin
}

// goEnvValue asks the go tool for one variable's effective value.
func goEnvValue(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command(gotool(t), "env", name).Output()
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
	cmd := exec.Command(gotool(t), append([]string{"env", "-json"}, names...)...)
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

// TestVerifyCgoIncludePath_FailsLoudOnMissingIcu4c pins the loud half of
// gt-mjll: when the configured include path cannot resolve icu4c's header,
// the error must name the header and the offending directory — instead of
// letting cgo report it from inside a dependency's generated C++.
func TestVerifyCgoIncludePath_FailsLoudOnMissingIcu4c(t *testing.T) {
	// No compiler is the same as a host that cannot run the probe at all:
	// the check declines rather than blocking, and so must the test.
	if !cgoCompilerPresent() {
		t.Skip("no C compiler on this host; the include-path probe cannot run")
	}
	missing := filepath.Join(t.TempDir(), "definitely-not-icu4c")
	t.Setenv("CGO_CPPFLAGS", "-I"+missing)
	t.Setenv("CGO_CXXFLAGS", "")
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
	for _, want := range []string{icu4cHeader, missing} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
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
	t.Setenv("CGO_CXXFLAGS", "")
	t.Setenv("CGO_CFLAGS", "-O2 -g")

	if err := verifyCgoIncludePath(); err != nil {
		t.Errorf("verifyCgoIncludePath with no -I = %v, want nil", err)
	}
}

// TestVerifyCgoIncludePath_AcceptsResolvableHeader is the negative control: an
// include path that does resolve the header must not trip the check. Without
// it, a probe that failed for any reason at all would still look green.
func TestVerifyCgoIncludePath_AcceptsResolvableHeader(t *testing.T) {
	// No compiler means the probe skips, which still proves the configured
	// path does not fail the suite; a resolvable-header assertion needs one.
	if !cgoCompilerPresent() {
		t.Skip("no C compiler on this host; the include-path probe cannot run")
	}
	if runtime.GOOS != "darwin" || os.Getenv("CGO_CPPFLAGS") == "" {
		// The host's own flags are the only include path known to resolve the
		// header here; without them there is nothing to assert on.
		t.Skip("no host-configured icu4c include path to assert against")
	}
	t.Setenv("CGO_CXXFLAGS", "")

	if err := verifyCgoIncludePath(); err != nil {
		t.Errorf("verifyCgoIncludePath with the host's resolvable include path = %v, want nil", err)
	}
}

// cgoCompilerPresent reports whether a C compiler the probe would use is
// reachable, splitting CC the way the probe does.
func cgoCompilerPresent() bool {
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "cc"
	}
	parts := shellSplit(cc)
	if len(parts) == 0 {
		return false
	}
	_, err := exec.LookPath(parts[0])
	return err == nil
}

// systemIcu4cResolvable reports whether the C toolchain finds icu4c's header
// with no include flags at all.
func systemIcu4cResolvable(t *testing.T) bool {
	t.Helper()
	if !cgoCompilerPresent() {
		return false
	}
	cc := os.Getenv("CC")
	if cc == "" {
		cc = "cc"
	}
	parts := shellSplit(cc)
	probe := exec.Command(parts[0], "-fsyntax-only", "-x", "c", "-")
	probe.Stdin = strings.NewReader("#include " + icu4cHeader + "\n")
	return probe.Run() == nil
}

// TestShellSplit_PinssCgoQuoting pins the quoting rules the probe relies on:
// a quoted include path with spaces must stay one field, and a CC value with
// arguments must split into a binary plus its defaults.
func TestShellSplit_PinssCgoQuoting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{`-I /opt/icu4c/include`, []string{"-I", "/opt/icu4c/include"}},
		{`-I"/opt/icu 4c/include" -O2`, []string{"-I/opt/icu 4c/include", "-O2"}},
		{`"ccache clang" -I/a`, []string{"ccache clang", "-I/a"}},
		{`ccache clang -I/a`, []string{"ccache", "clang", "-I/a"}},
		{`-I'/opt/icu 4c/include'`, []string{"-I/opt/icu 4c/include"}},
		{"", nil},
		{"   ", nil},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := shellSplit(c.in)
			if !equalStrings(got, c.want) {
				t.Errorf("shellSplit(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}