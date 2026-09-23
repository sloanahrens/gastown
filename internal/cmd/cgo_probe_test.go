package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestVerifyCgoIncludePath_FailsLoudOnMissingIcu4c pins the loud half of
// gt-mjll: when the configured include path cannot resolve icu4c's header,
// the error must name the header and the offending directory — instead of
// letting cgo report it from inside a dependency's generated C++.
func TestVerifyCgoIncludePath_FailsLoudOnMissingIcu4c(t *testing.T) {
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
// configures no include directory loses nothing to the HOME redirect, and
// must not be blocked by a diagnosis the harness cannot make.
//
// The regression this guards is real: CGO_CFLAGS defaults to "-O2 -g" on
// every host, so gating on "any cgo flag at all" would run the probe
// everywhere and fail every hermetic package on a machine whose icu4c sits
// on the toolchain's default search path.
func TestVerifyCgoIncludePath_SkipsWithoutIncludeDirs(t *testing.T) {
	t.Setenv("CGO_CPPFLAGS", "")
	t.Setenv("CGO_CXXFLAGS", "")
	t.Setenv("CGO_CFLAGS", "-O2 -g")

	if err := verifyCgoIncludePath(); err != nil {
		t.Errorf("verifyCgoIncludePath with no -I = %v, want nil", err)
	}
}

// TestVerifyCgoIncludePath_AcceptsResolvableHeader is the negative control:
// an include path that does resolve the header must not trip the check.
// Without it, a probe that failed for any reason at all would still look
// green.
func TestVerifyCgoIncludePath_AcceptsResolvableHeader(t *testing.T) {
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

// TestVerifyCgoIncludePath_FailsLoudOnUnrecognizedCompilerError pins the
// other loud half: a compiler failure that is not a missing-header
// diagnosis must not be swallowed as if the probe had passed.
func TestVerifyCgoIncludePath_FailsLoudOnUnrecognizedCompilerError(t *testing.T) {
	if !cgoCompilerPresent() {
		t.Skip("no C compiler on this host; the include-path probe cannot run")
	}
	// An unrecognized flag makes every compiler reject the invocation before
	// it even looks for the header, which is exactly "an error the probe
	// does not recognize" rather than a missing-header diagnosis.
	t.Setenv("CGO_CPPFLAGS", "-I"+t.TempDir())
	t.Setenv("CGO_CXXFLAGS", "--this-flag-does-not-exist-anywhere")
	t.Setenv("CGO_CFLAGS", "")

	err := verifyCgoIncludePath()
	if err == nil {
		t.Fatal("verifyCgoIncludePath with an unrecognized compiler error = nil, want a loud failure")
	}
	if strings.Contains(err.Error(), "go-icu-regex") {
		t.Errorf("error = %q, want the unrecognized-error message, not the missing-header one", err)
	}
}

// TestVerifyCgoIncludePath_FailsLoudOnGoEnvError pins the third loud half:
// a `go env` failure while resolving the effective cgo flags must surface,
// not be treated as "no flags configured".
func TestVerifyCgoIncludePath_FailsLoudOnGoEnvError(t *testing.T) {
	withFailingGoOnPath(t)

	if err := verifyCgoIncludePath(); err == nil {
		t.Fatal("verifyCgoIncludePath with a failing `go env` = nil, want a loud failure")
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

// withFailingGoOnPath points PATH at a stand-in `go` that always fails, so a
// `go env` call inside the function under test returns an error, and
// restores the real PATH via t.Cleanup.
func withFailingGoOnPath(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	name := "go"
	script := "#!/bin/sh\nexit 1\n"
	if runtime.GOOS == "windows" {
		name = "go.bat"
		script = "@exit /b 1\r\n"
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("writing stand-in go: %v", err)
	}
	t.Setenv("PATH", binDir)
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
