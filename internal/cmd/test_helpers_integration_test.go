//go:build integration || e2e

package cmd

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

func buildGTBinary() (string, error) {
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
	icuEnv := icuCgoEnv()
	if err := verifyCgoIncludePath(icuEnv...); err != nil {
		return "", err
	}

	// Must set BuiltProperly=1 via ldflags, otherwise binary refuses to run
	ldflags := "-X github.com/steveyegge/gastown/internal/cmd.BuiltProperly=1"
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", tmpBinary, "./cmd/gt")
	cmd.Dir = projectRoot
	cmd.Env = append(os.Environ(), icuEnv...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w\nOutput: %s", err, output)
	}
	return tmpBinary, nil
}

// icuCgoEnv returns CGO_CPPFLAGS/CGO_LDFLAGS additions pointing at a keg-only
// Homebrew icu4c, which go-icu-regex (a dolt dependency) needs on macOS.
//
// The Makefile detects the prefix and exports these for `make build` and
// `make test`, so a build under make works by inheritance; a bare `go test`
// gets no such help and dies with "unicode/regex.h file not found", failing
// every buildGT caller. Returns nil when the flags are already set, when not
// on darwin, or when brew cannot find icu4c.
func icuCgoEnv() []string {
	if runtime.GOOS != "darwin" || os.Getenv("CGO_CPPFLAGS") != "" || os.Getenv("CGO_LDFLAGS") != "" {
		return nil
	}
	out, err := exec.Command("brew", "--prefix", "icu4c").Output()
	if err != nil {
		return nil
	}
	prefix := strings.TrimSpace(string(out))
	if prefix == "" {
		return nil
	}
	return []string{
		"CGO_CPPFLAGS=-I" + filepath.Join(prefix, "include"),
		"CGO_LDFLAGS=-L" + filepath.Join(prefix, "lib"),
	}
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
