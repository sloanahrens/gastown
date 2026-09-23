package testutil

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreserveGoEnv_PinsGoEnvPastHomeRedirect is the regression test for
// gt-mjll: on a host with a keg-only icu4c, CGO_CPPFLAGS/CGO_LDFLAGS live in
// the go env FILE ($GOENV, itself under $HOME), and the harness's HOME
// redirect points `go env` at an empty file under the sandbox, losing them
// from every `go` subprocess a test spawns.
//
// It models the host rather than trusting the machine it runs on: seeds a
// go env file with sentinel flags under a stand-in HOME and XDG_CONFIG_HOME
// (StartHermetic redirects both), calls preserveGoEnv while that pair is
// still current, then swaps both to a second, unrelated sandbox and asserts
// a `go` subprocess under it still resolves the sentinel flags — and that
// the flags never landed in the process environment itself, which is what a
// caller like buildGTBinary's brew fallback checks before running.
func TestPreserveGoEnv_PinsGoEnvPastHomeRedirect(t *testing.T) {
	withSavedEnv(t)
	devHome := t.TempDir()
	devConfigHome := filepath.Join(devHome, ".config")
	os.Setenv("HOME", devHome)
	os.Setenv("USERPROFILE", devHome)
	os.Setenv("XDG_CONFIG_HOME", devConfigHome)
	os.Unsetenv("GOENV")
	for _, v := range append([]string{"CGO_CPPFLAGS", "CGO_CFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS"}, goEnvCarryVars...) {
		os.Unsetenv(v)
	}

	// The default go env file location for this stand-in HOME/XDG_CONFIG_HOME,
	// resolved the same way the go tool resolves it: nothing overrides GOENV
	// yet.
	devGoEnvPath := goEnvValue(t, "GOENV")
	seedGoEnvFile(t, devGoEnvPath, map[string]string{
		"CGO_CPPFLAGS": "-I" + filepath.Join(devHome, "icu4c", "include"),
		"CGO_CXXFLAGS": "-I" + filepath.Join(devHome, "icu4c", "include"),
		"CGO_LDFLAGS":  "-L" + filepath.Join(devHome, "icu4c", "lib"),
	})

	// The harness's sandbox: an empty HOME/XDG_CONFIG_HOME with no env file
	// at all. Every test-spawned `go` invocation lands here once HOME is
	// redirected.
	sandboxHome := t.TempDir()

	// Half one — reproduce. Swapping HOME (and XDG_CONFIG_HOME, which is
	// where go env GOENV actually resolves on Linux) with nothing carried
	// loses the flags entirely. That is the bug.
	if got := goEnvWith(t, sandboxHome, "CGO_CPPFLAGS")["CGO_CPPFLAGS"]; got != "" {
		t.Fatalf("precondition failed: an empty HOME resolved CGO_CPPFLAGS=%q", got)
	}

	if err := preserveGoEnv(); err != nil {
		t.Fatalf("preserveGoEnv: %v", err)
	}
	if pinned := os.Getenv("GOENV"); pinned != devGoEnvPath {
		t.Fatalf("GOENV after preserveGoEnv = %q, want %q", pinned, devGoEnvPath)
	}

	// The redirect itself: what StartHermetic does next. GOENV, now an
	// absolute path fixed before the redirect, is unaffected by it.
	os.Setenv("HOME", sandboxHome)
	os.Setenv("USERPROFILE", sandboxHome)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(sandboxHome, ".config"))

	// Half two — the fix. A `go` subprocess spawned under the sandbox HOME
	// still resolves the sentinel flags, because GOENV still points at the
	// developer's real file.
	want := map[string]string{
		"CGO_CPPFLAGS": "-I" + filepath.Join(devHome, "icu4c", "include"),
		"CGO_CXXFLAGS": "-I" + filepath.Join(devHome, "icu4c", "include"),
		"CGO_LDFLAGS":  "-L" + filepath.Join(devHome, "icu4c", "lib"),
	}
	got := goEnvWith(t, sandboxHome, "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS")
	for name, w := range want {
		if got[name] != w {
			t.Errorf("go env %s under the sandbox HOME = %q, want %q: a test-spawned `go build` still loses icu4c", name, got[name], w)
		}
	}

	// The flags themselves must stay out of the process environment: a
	// caller like buildGTBinary's brew fallback only runs when
	// CGO_CPPFLAGS/CGO_LDFLAGS are unset (gt-mjll).
	for _, v := range []string{"CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS"} {
		if got := os.Getenv(v); got != "" {
			t.Errorf("%s in the process environment after preserveGoEnv = %q, want unset", v, got)
		}
	}
}

// TestPreserveGoEnv_FailsLoudOnGoEnvError pins the loud half: a `go env`
// failure while resolving GOENV must surface as an error, not be treated as
// "nothing to carry".
func TestPreserveGoEnv_FailsLoudOnGoEnvError(t *testing.T) {
	withSavedEnv(t)
	os.Unsetenv("GOENV")
	WithFailingGoOnPath(t)

	if err := preserveGoEnv(); err == nil {
		t.Fatal("preserveGoEnv with a failing `go env` = nil error, want a loud failure")
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
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("writing go env file: %v", err)
	}
}

// goEnvWith runs `go env -json <names>` in a subprocess whose HOME (and, to
// match StartHermetic's own redirect, XDG_CONFIG_HOME) is under home, and
// which otherwise inherits the process environment. That is what a
// test-spawned `go build` sees after the harness redirects HOME.
func goEnvWith(t *testing.T, home string, names ...string) map[string]string {
	t.Helper()
	cmd := exec.Command(gotool(t), append([]string{"env", "-json"}, names...)...)
	cmd.Env = envWith(map[string]string{
		"HOME":            home,
		"USERPROFILE":     home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
	})
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

// envWith returns the process environment with overrides applied. It
// replaces rather than appends, because a duplicated variable leaves the
// child's getenv() free to return either copy.
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
