//go:build integration || e2e

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/buildflags"
)

// buildGT returns the path to a gt binary built from this checkout, with
// BuiltProperly=1 set (without it the binary refuses to run).
func buildGT(t *testing.T) string {
	t.Helper()

	gtBinaryOnce.Do(func() {
		gtBinaryPath, gtBinaryErr = buildGTBinary()
	})
	if gtBinaryErr != nil {
		t.Fatalf("failed to build gt: %v", gtBinaryErr)
	}
	return gtBinaryPath
}

// sweepGTBinEnv names the gt binary the tier sweep builds once per run and hands
// to the tests (internal/tiersweep's EnvGTBin). It is not a GT_ name.
const sweepGTBinEnv = "TIER_SWEEP_GT_BIN"

// goBuild runs the `go build` that buildGTBinary makes and returns its combined
// output. A test replaces it to observe the build without linking a binary.
var goBuild = func(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

// sweepBuiltGT returns the sweep's prebuilt gt when TIER_SWEEP_GT_BIN names an
// executable regular file. Any other value is logged and ignored: a path the
// sweep did not hand over is not trusted, and the caller builds its own copy.
func sweepBuiltGT() (string, bool) {
	path := os.Getenv(sweepGTBinEnv)
	if path == "" {
		return "", false
	}
	info, err := os.Stat(path)
	if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
		return path, true
	}
	fmt.Fprintf(os.Stderr, "buildGT: ignoring %s=%q: not an executable regular file; building gt here\n", sweepGTBinEnv, path)
	return "", false
}

func buildGTBinary() (string, error) {
	if path, ok := sweepBuiltGT(); ok {
		return path, nil
	}

	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting working directory: %w", err)
	}

	projectRoot := wd
	for {
		if _, err := os.Stat(filepath.Join(projectRoot, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(projectRoot)
		if parent == projectRoot {
			return "", fmt.Errorf("could not find project root (go.mod) above %s", wd)
		}
		projectRoot = parent
	}

	// Link into a directory private to this test process. The output path
	// used to be $TMPDIR/gt-integration-test-<hash of projectRoot>: one file
	// per checkout path, shared by every process testing that path. A
	// checkout path is not one tree — the refinery rig checks out branch
	// after branch in the same directory — so a process still linking an
	// older tree could replace the binary under a newer run, which then
	// exec'd a gt without the code under test: TestBootSendKeysGuard_Integration
	// saw exit 0 and empty stderr from a gt that predated the guard. go build
	// renames its output into place, so the swap was silent. The per-path
	// files were also never deleted (~180MB each).
	sweepStaleGTBinaries()
	dir, err := os.MkdirTemp("", fmt.Sprintf("%s%d-", gtBinaryDirPrefix, os.Getpid()))
	if err != nil {
		return "", fmt.Errorf("creating gt build dir: %w", err)
	}
	gtBinaryDir = dir
	binaryName := "gt"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	tmpBinary := filepath.Join(dir, binaryName)

	// Dolt's go-icu-regex cgo build only reaches this point, so only this
	// helper needs the include-path diagnosis (gt-mjll). icuEnv is what the
	// `go build` below adds to its own environment; the probe checks flags
	// with the same override applied, so a stale $GOENV that icuEnv would
	// correct does not block a build that would otherwise succeed.
	icuEnv := buildflags.ICUCgoEnv(os.Environ())
	if err := verifyCgoIncludePath(icuEnv...); err != nil {
		return "", err
	}

	// Must set BuiltProperly=1 via ldflags, otherwise binary refuses to run
	if output, err := goBuild(projectRoot, append(os.Environ(), icuEnv...), "build", "-ldflags", buildflags.LDFlags, "-o", tmpBinary, "./cmd/gt"); err != nil {
		return "", fmt.Errorf("%w\nOutput: %s", err, output)
	}
	return tmpBinary, nil
}

// verifyCgoIncludePath fails loud when the cgo include flags that would
// reach the `go build` in buildGTBinary cannot resolve icu4c's
// unicode/regex.h. It reads CGO_CPPFLAGS/CGO_CXXFLAGS from the go tool
// (covering a value carried only through preserveGoEnv's pinned GOENV
// file), applies overrides — buildGTBinary passes icuCgoEnv's
// brew-discovered flags here, matching the precedence cmd/go itself gives a
// process-env value over the go env file — then asks the C++ compiler
// whether the header resolves under the result, reporting the flags and a
// repair when it does not (gt-mjll).
//
// Skipped with a stderr notice when no include directory is configured or
// no compiler is reachable: CGO_CFLAGS defaults to "-O2 -g" on every host,
// so gating on any cgo flag would fire everywhere. A compiler failure that
// is not a missing-header diagnosis still fails loud — it is not evidence
// the header resolves, and the `go build` a moment later would hit the
// same failure with a worse message.
func verifyCgoIncludePath(overrides ...string) error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return nil
	}
	out, err := exec.Command(goBin, "env", "-json", "CGO_CPPFLAGS", "CGO_CXXFLAGS").Output()
	if err != nil {
		return fmt.Errorf("asking the go tool for the effective cgo flags: %w", err)
	}
	values := map[string]string{}
	if err := json.Unmarshal(out, &values); err != nil {
		return fmt.Errorf("parsing `go env -json CGO_CPPFLAGS CGO_CXXFLAGS`: %w", err)
	}
	for _, kv := range overrides {
		if name, val, ok := strings.Cut(kv, "="); ok {
			values[name] = val
		}
	}

	cgoFlags := shellSplit(values["CGO_CPPFLAGS"] + " " + values["CGO_CXXFLAGS"])
	if !hasIncludeDir(cgoFlags) {
		return nil
	}

	cxxParts, err := probeCompiler()
	if err != nil {
		fmt.Fprintf(os.Stderr, "buildGT: %v; skipping the icu4c include-path probe\n", err)
		return nil
	}

	// -x c++ -std=c++17: go-icu-regex's own #cgo CXXFLAGS pragma (internal/icu/icu.go)
	// requires C++17, and CGO_CXXFLAGS may carry other C++-only flags a C
	// compile would reject before it even looked for the header. Without
	// -std=c++17 here, a keg-only-icu4c host whose clang defaults to an
	// older standard fails this probe on valid, buildable flags: ICU's
	// headers use C++17 features (uversion.h, char16ptr.h) that only parse
	// under that standard or newer.
	args := append(append([]string{"-std=c++17"}, cgoFlags...), "-fsyntax-only", "-x", "c++", "-")
	probe := exec.Command(cxxParts[0], args...)
	probe.Stdin = strings.NewReader("#include " + icu4cHeader + "\n")
	probeOut, err := probe.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := string(probeOut)
	if strings.Contains(msg, "file not found") || strings.Contains(msg, "No such file or directory") {
		return fmt.Errorf(`buildGT: cgo include path cannot resolve icu4c's %s, so Dolt's go-icu-regex dependency will not compile:
  CGO_CPPFLAGS=%s
  CGO_CXXFLAGS=%s
  %s: %s
icu4c installs keg-only or into a prefix the compiler does not search, and
upgrading or reinstalling moves the prefix without updating $GOENV. Repair it
with:
  go env -w CGO_CPPFLAGS="-I<icu4c-prefix>/include" CGO_LDFLAGS="-L<icu4c-prefix>/lib"`,
			icu4cHeader, values["CGO_CPPFLAGS"], values["CGO_CXXFLAGS"],
			strings.Join(cxxParts, " "), firstLine(probeOut))
	}
	return fmt.Errorf("buildGT: %s failed the icu4c include-path probe with an error it does not recognize, so the cgo flags cannot be verified:\n%s",
		strings.Join(cxxParts, " "), strings.TrimSpace(msg))
}

// probeCompiler resolves the C++ compiler verifyCgoIncludePath's probe uses
// — go env CXX (process env, then the go env file, then a "c++" default),
// quote-split the way cmd/go splits CC/CXX ("ccache clang++" is one field) —
// and confirms it is on PATH. Factored out so the probe and its tests
// (cgoCompilerPresent, systemIcu4cResolvable in cgo_probe_integration_test.go) agree on
// which compiler is being exercised.
func probeCompiler() ([]string, error) {
	cxx := ""
	if goBin, err := exec.LookPath("go"); err == nil {
		if out, err := exec.Command(goBin, "env", "CXX").Output(); err == nil {
			cxx = strings.TrimSpace(string(out))
		}
	}
	if cxx == "" {
		cxx = "c++"
	}
	parts := shellSplit(cxx)
	if len(parts) == 0 {
		return nil, fmt.Errorf("CXX=%q does not name a compiler", cxx)
	}
	if _, err := exec.LookPath(parts[0]); err != nil {
		return nil, fmt.Errorf("no C++ compiler at %q", parts[0])
	}
	return parts, nil
}

// hasIncludeDir reports whether cgo flags name at least one include
// directory, in either the joined (-I/path) or separated (-I /path)
// spelling. Both match on the prefix, so the bare forms need no special case.
func hasIncludeDir(cgoFlags []string) bool {
	for _, f := range cgoFlags {
		if strings.HasPrefix(f, "-I") || strings.HasPrefix(f, "-isystem") {
			return true
		}
	}
	return false
}

// firstLine trims compiler output to its first non-empty line, so a probe
// failure reads as one actionable sentence instead of a clang banner.
func firstLine(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "(no output)"
}

// stubGoBuild isolates one buildGTBinary call. TMPDIR moves into t.TempDir so
// the build directory lands there, goBuild becomes a recorder that links
// nothing, and gtBinaryDir is restored so the process's real build directory
// is not forgotten. It returns the argument list of each go build the call made.
func stubGoBuild(t *testing.T) *[][]string {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	prevDir, prevBuild := gtBinaryDir, goBuild
	t.Cleanup(func() {
		gtBinaryDir, goBuild = prevDir, prevBuild
	})
	var calls [][]string
	goBuild = func(dir string, env []string, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, nil
	}
	return &calls
}

// TestIntegrationBuildGTUsesTheSweepBinary: with TIER_SWEEP_GT_BIN naming an
// executable file, buildGTBinary returns that path and runs no go build.
func TestIntegrationBuildGTUsesTheSweepBinary(t *testing.T) {
	builds := stubGoBuild(t)
	sweepBin := filepath.Join(t.TempDir(), "gt")
	if err := os.WriteFile(sweepBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing sweep gt stand-in: %v", err)
	}
	t.Setenv(sweepGTBinEnv, sweepBin)

	got, err := buildGTBinary()
	if err != nil {
		t.Fatalf("buildGTBinary: %v", err)
	}
	if got != sweepBin {
		t.Errorf("buildGTBinary = %q, want the sweep's binary %q", got, sweepBin)
	}
	if len(*builds) != 0 {
		t.Errorf("buildGTBinary ran go build %d time(s) with the sweep binary set; want none: %v", len(*builds), *builds)
	}
}

// TestIntegrationBuildGTIgnoresAnUnusableSweepBinary: a missing path, a
// directory and a file without an execute bit are not trusted, so
// buildGTBinary builds gt as it does when the variable is unset.
func TestIntegrationBuildGTIgnoresAnUnusableSweepBinary(t *testing.T) {
	notExecutable := filepath.Join(t.TempDir(), "gt")
	if err := os.WriteFile(notExecutable, []byte("not a program\n"), 0o644); err != nil {
		t.Fatalf("writing non-executable stand-in: %v", err)
	}
	cases := []struct {
		name string
		path string
	}{
		{"missing", filepath.Join(t.TempDir(), "gt")},
		{"directory", t.TempDir()},
		{"not executable", notExecutable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builds := stubGoBuild(t)
			t.Setenv(sweepGTBinEnv, tc.path)

			got, err := buildGTBinary()
			if err != nil {
				t.Fatalf("buildGTBinary: %v", err)
			}
			if len(*builds) != 1 {
				t.Fatalf("go build ran %d time(s); want 1, as when the variable is unset: %v", len(*builds), *builds)
			}
			args := (*builds)[0]
			i := slices.Index(args, "-o")
			if i < 0 || i+1 >= len(args) {
				t.Fatalf("go build args name no -o output: %v", args)
			}
			if got != args[i+1] {
				t.Errorf("buildGTBinary = %q, want the path go build wrote %q", got, args[i+1])
			}
		})
	}
}
