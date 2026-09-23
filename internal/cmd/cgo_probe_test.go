package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// withCleanGoEnv points GOENV at an empty temp file for the duration of the
// test, so `go env` resolves cgo flags only from what the test sets via
// t.Setenv — not from the developer's real go env file, which this
// package's TestMain (testutil.HermeticMain) pins into GOENV for the whole
// binary run so a real gt build keeps working (gt-mjll). Without this,
// t.Setenv("CGO_CPPFLAGS", "") cannot clear a value: cmd/go treats an empty
// variable as unset and falls back to the file.
func withCleanGoEnv(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "go-env")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("writing empty go env file: %v", err)
	}
	t.Setenv("GOENV", empty)
}

// writeStubIcu4cHeader creates a throwaway include directory containing a
// stub unicode/regex.h, so a test can assert the probe accepts a resolvable
// path without depending on the host actually having icu4c installed.
func writeStubIcu4cHeader(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "unicode"), 0o755); err != nil {
		t.Fatalf("creating stub include dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unicode", "regex.h"), []byte("// stub\n"), 0o644); err != nil {
		t.Fatalf("writing stub header: %v", err)
	}
	return dir
}

// TestVerifyCgoIncludePath_FailsLoudOnMissingIcu4c: when the configured
// include path cannot resolve icu4c's header, the error must name the
// header and the offending directory — instead of letting cgo report it
// from inside a dependency's generated C++.
func TestVerifyCgoIncludePath_FailsLoudOnMissingIcu4c(t *testing.T) {
	if !cgoCompilerPresent() {
		t.Skip("no C++ compiler on this host; the include-path probe cannot run")
	}
	withCleanGoEnv(t)
	missing := filepath.Join(t.TempDir(), "definitely-not-icu4c")
	t.Setenv("CGO_CPPFLAGS", "-I"+missing)
	t.Setenv("CGO_CXXFLAGS", "")

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
// configures no include directory loses nothing to the HOME redirect, and
// must not be blocked by a diagnosis the harness cannot make.
//
// The regression this guards is real: CGO_CFLAGS defaults to "-O2 -g" on
// every host, so gating on "any cgo flag at all" would run the probe
// everywhere and fail every hermetic package on a machine whose icu4c sits
// on the toolchain's default search path.
func TestVerifyCgoIncludePath_SkipsWithoutIncludeDirs(t *testing.T) {
	withCleanGoEnv(t)
	t.Setenv("CGO_CPPFLAGS", "")
	t.Setenv("CGO_CXXFLAGS", "")
	t.Setenv("CGO_CFLAGS", "-O2 -g")

	if err := verifyCgoIncludePath(); err != nil {
		t.Errorf("verifyCgoIncludePath with no -I = %v, want nil", err)
	}
}

// TestVerifyCgoIncludePath_AcceptsResolvableHeader is the negative control:
// an include path that does resolve the header must not trip the check. It
// seeds a stub header rather than depending on the host's own icu4c
// install, so it runs on exactly the keg-only hosts gt-mjll targets — where
// the flags live in the go env file, not the process environment, and a
// gate on os.Getenv would always skip.
func TestVerifyCgoIncludePath_AcceptsResolvableHeader(t *testing.T) {
	if !cgoCompilerPresent() {
		t.Skip("no C++ compiler on this host; the include-path probe cannot run")
	}
	withCleanGoEnv(t)
	dir := writeStubIcu4cHeader(t)
	t.Setenv("CGO_CPPFLAGS", "-I"+dir)
	t.Setenv("CGO_CXXFLAGS", "")

	if err := verifyCgoIncludePath(); err != nil {
		t.Errorf("verifyCgoIncludePath with a resolvable include path = %v, want nil", err)
	}
}

// TestVerifyCgoIncludePath_OverrideWinsOverStaleGoEnv pins the brew-fallback
// half of gt-mjll: when the pinned GOENV file names a stale icu4c prefix —
// the case an upgrade or reinstall leaves behind — an override such as
// icuCgoEnv's brew-discovered path must still let the build through, since
// that is the same value buildGTBinary applies to `go build` itself.
func TestVerifyCgoIncludePath_OverrideWinsOverStaleGoEnv(t *testing.T) {
	if !cgoCompilerPresent() {
		t.Skip("no C++ compiler on this host; the include-path probe cannot run")
	}
	withCleanGoEnv(t)
	stale := filepath.Join(t.TempDir(), "stale-prefix", "include")
	t.Setenv("CGO_CPPFLAGS", "-I"+stale)
	t.Setenv("CGO_CXXFLAGS", "")

	if err := verifyCgoIncludePath(); err == nil {
		if systemIcu4cResolvable(t) {
			t.Skip("icu4c is on the toolchain's default search path, so a stale -I alone cannot fail the probe")
		}
		t.Fatal("precondition failed: verifyCgoIncludePath accepted the stale path with no override")
	}

	good := writeStubIcu4cHeader(t)
	if err := verifyCgoIncludePath("CGO_CPPFLAGS=-I" + good); err != nil {
		t.Errorf("verifyCgoIncludePath with a brew-fallback override = %v, want nil: the override should win over the stale go env value", err)
	}
}

// TestVerifyCgoIncludePath_FailsLoudOnUnrecognizedCompilerError: a compiler
// failure that is not a missing-header diagnosis must not be swallowed as
// if the probe had passed.
func TestVerifyCgoIncludePath_FailsLoudOnUnrecognizedCompilerError(t *testing.T) {
	if !cgoCompilerPresent() {
		t.Skip("no C++ compiler on this host; the include-path probe cannot run")
	}
	withCleanGoEnv(t)
	// An unrecognized flag makes every compiler reject the invocation before
	// it even looks for the header, which is exactly "an error the probe
	// does not recognize" rather than a missing-header diagnosis.
	t.Setenv("CGO_CPPFLAGS", "-I"+t.TempDir())
	t.Setenv("CGO_CXXFLAGS", "--this-flag-does-not-exist-anywhere")

	err := verifyCgoIncludePath()
	if err == nil {
		t.Fatal("verifyCgoIncludePath with an unrecognized compiler error = nil, want a loud failure")
	}
	if strings.Contains(err.Error(), "go-icu-regex") {
		t.Errorf("error = %q, want the unrecognized-error message, not the missing-header one", err)
	}
}

// TestVerifyCgoIncludePath_FailsLoudOnGoEnvError: a `go env` failure while
// resolving the effective cgo flags must surface, not be treated as "no
// flags configured".
func TestVerifyCgoIncludePath_FailsLoudOnGoEnvError(t *testing.T) {
	testutil.WithFailingGoOnPath(t)

	if err := verifyCgoIncludePath(); err == nil {
		t.Fatal("verifyCgoIncludePath with a failing `go env` = nil, want a loud failure")
	}
}

// cgoCompilerPresent reports whether the C++ compiler the probe would use is
// reachable.
func cgoCompilerPresent() bool {
	_, err := probeCompiler()
	return err == nil
}

// systemIcu4cResolvable reports whether the C++ toolchain finds icu4c's
// header with no include flags at all.
func systemIcu4cResolvable(t *testing.T) bool {
	t.Helper()
	parts, err := probeCompiler()
	if err != nil {
		return false
	}
	probe := exec.Command(parts[0], "-fsyntax-only", "-x", "c++", "-")
	probe.Stdin = strings.NewReader("#include " + icu4cHeader + "\n")
	return probe.Run() == nil
}

// TestShellSplit_PinsCgoQuoting pins the quoting rules the probe relies on:
// a quoted include path with spaces must stay one field, and a CC value with
// arguments must split into a binary plus its defaults.
func TestShellSplit_PinsCgoQuoting(t *testing.T) {
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
