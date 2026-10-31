//go:build integration || e2e

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/buildflags"
	"github.com/steveyegge/gastown/internal/buildpaths"
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

// sweepNotice is where buildGTBinary reports a TIER_SWEEP_GT_BIN it did not
// use. A test replaces it to read the notice.
var sweepNotice io.Writer = os.Stderr

// sweepBuiltGT returns the sweep's prebuilt gt when TIER_SWEEP_GT_BIN names an
// executable regular file no older than the build inputs under srcRoot. Any
// other value is noted and ignored: a path the sweep did not hand over, or a
// binary the inputs have outrun, is not trusted, and the caller builds its own.
func sweepBuiltGT(srcRoot string) (string, bool) {
	path := os.Getenv(sweepGTBinEnv)
	if path == "" {
		return "", false
	}
	reason := sweepBinaryUnusable(path, srcRoot)
	if reason == "" {
		return path, true
	}
	fmt.Fprintf(sweepNotice, "buildGT: ignoring %s=%q: %s; building gt here\n", sweepGTBinEnv, path, reason)
	return "", false
}

// sweepBinaryUnusable returns why the binary at path is not one to trust, and
// "" when it is. The freshness check compares its modification time with the
// build inputs: a binary built before one of them changed may predate the code
// under test, the stale-gt failure this helper exists to avoid. Inputs that
// cannot be read, or no inputs at all, are not trusted either.
func sweepBinaryUnusable(path, srcRoot string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "not an executable regular file"
	}
	newest, newestTime, err := newestBuildInput(srcRoot)
	if err != nil {
		return fmt.Sprintf("cannot read the build inputs under %s: %v", srcRoot, err)
	}
	if newest == "" {
		return fmt.Sprintf("no build inputs under %s", srcRoot)
	}
	if newestTime.After(info.ModTime()) {
		return "older than the build input " + newest
	}
	return ""
}

// newestBuildInput returns the path, relative to root, of the file that
// buildpaths.AffectsBuild counts as binary-affecting and that was modified last,
// with its modification time. That covers Go sources, go.mod and what the
// //go:embed directives compile in, the same rule the staleness check and the
// daemon's rebuild job use (gt-p62ku). Hidden directories such as .git are
// skipped. buildGT calls it once per test process, so the walk runs once.
func newestBuildInput(root string) (string, time.Time, error) {
	var newest string
	var newestTime time.Time
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !buildpaths.AffectsBuild(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(newestTime) {
			newest, newestTime = rel, info.ModTime()
		}
		return nil
	})
	return newest, newestTime, err
}

// moduleRoot returns the nearest directory at or above the working directory
// that holds go.mod. A test replaces it with a tree it made.
var moduleRoot = func() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting working directory: %w", err)
	}
	root := wd
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root, nil
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("could not find project root (go.mod) above %s", wd)
		}
		root = parent
	}
}

// buildEnv is the cgo setup buildGTBinary's go build runs under: the icu4c
// flags buildflags.ICUCgoEnv adds, once verifyCgoIncludePath has confirmed the
// header resolves under them. A test replaces it so no host state is read (brew,
// the go env file, a C++ compiler).
var buildEnv = func() ([]string, error) {
	icuEnv := buildflags.ICUCgoEnv(os.Environ())
	return icuEnv, verifyCgoIncludePath(icuEnv...)
}

func buildGTBinary() (string, error) {
	projectRoot, err := moduleRoot()
	if err != nil {
		return "", err
	}
	if path, ok := sweepBuiltGT(projectRoot); ok {
		return path, nil
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
	icuEnv, err := buildEnv()
	if err != nil {
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

// buildStub isolates buildGTBinary calls from the host and records what they
// do: src is the module root it sees, builds the argument list of each go build
// it made, and notice what it wrote to sweepNotice.
type buildStub struct {
	src    string
	builds [][]string
	notice bytes.Buffer
}

// stubBuild isolates one buildGTBinary call. TMPDIR moves into t.TempDir so the
// build directory lands there, the module root and the cgo preflight become
// fakes so no host state is read, goBuild becomes a recorder that links nothing,
// and the process's globals are restored afterwards. gtBinaryDir is among them
// because buildGTBinary's fallback sets it to the build directory it made. Its
// module root holds a go.mod and one source file, both dated an hour back.
func stubBuild(t *testing.T) *buildStub {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	stub := &buildStub{src: t.TempDir()}
	dated := time.Now().Add(-time.Hour)
	writeAt(t, filepath.Join(stub.src, "go.mod"), "module example.com/gt\n", 0o644, dated)
	writeAt(t, filepath.Join(stub.src, "cmd", "gt", "main.go"), "package main\n", 0o644, dated)

	prevDir, prevBuild, prevRoot, prevEnv, prevNotice := gtBinaryDir, goBuild, moduleRoot, buildEnv, sweepNotice
	t.Cleanup(func() {
		gtBinaryDir, goBuild, moduleRoot, buildEnv, sweepNotice = prevDir, prevBuild, prevRoot, prevEnv, prevNotice
	})
	moduleRoot = func() (string, error) { return stub.src, nil }
	buildEnv = func() ([]string, error) { return nil, nil }
	sweepNotice = &stub.notice
	goBuild = func(dir string, env []string, args ...string) ([]byte, error) {
		stub.builds = append(stub.builds, args)
		return nil, nil
	}
	return stub
}

// writeAt writes a stand-in file and sets its modification time, so a test can
// date a sweep binary against the sources it is checked against.
func writeAt(t *testing.T, path, body string, mode os.FileMode, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("dating %s: %v", path, err)
	}
}

// TestIntegrationBuildGTUsesTheSweepBinary: with TIER_SWEEP_GT_BIN naming an
// executable file no older than the sources, buildGTBinary returns that path,
// runs no go build and writes no notice.
func TestIntegrationBuildGTUsesTheSweepBinary(t *testing.T) {
	stub := stubBuild(t)
	sweepBin := filepath.Join(t.TempDir(), "gt")
	writeAt(t, sweepBin, "#!/bin/sh\nexit 0\n", 0o755, time.Now().Add(-30*time.Minute))
	t.Setenv(sweepGTBinEnv, sweepBin)

	got, err := buildGTBinary()
	if err != nil {
		t.Fatalf("buildGTBinary: %v", err)
	}
	if got != sweepBin {
		t.Errorf("buildGTBinary = %q, want the sweep's binary %q", got, sweepBin)
	}
	if len(stub.builds) != 0 {
		t.Errorf("buildGTBinary ran go build %d time(s) with the sweep binary set; want none: %v", len(stub.builds), stub.builds)
	}
	if stub.notice.Len() != 0 {
		t.Errorf("buildGTBinary wrote a notice for a usable sweep binary: %q", stub.notice.String())
	}
}

// TestIntegrationBuildGTIgnoresAnUnusableSweepBinary: a missing path, a
// directory, a file without an execute bit, a file older than a build input (a
// Go source or an embedded file), and build inputs that cannot be read are not
// trusted. buildGTBinary builds gt as it does with the variable unset, and its
// notice says why.
func TestIntegrationBuildGTIgnoresAnUnusableSweepBinary(t *testing.T) {
	cases := []struct {
		name       string
		path       func(t *testing.T, stub *buildStub) string
		wantReason string
	}{
		{
			name:       "missing",
			path:       func(t *testing.T, _ *buildStub) string { return filepath.Join(t.TempDir(), "gt") },
			wantReason: "not an executable regular file",
		},
		{
			name:       "directory",
			path:       func(t *testing.T, _ *buildStub) string { return t.TempDir() },
			wantReason: "not an executable regular file",
		},
		{
			name: "not executable",
			path: func(t *testing.T, _ *buildStub) string {
				p := filepath.Join(t.TempDir(), "gt")
				writeAt(t, p, "not a program\n", 0o644, time.Now().Add(-30*time.Minute))
				return p
			},
			wantReason: "not an executable regular file",
		},
		{
			name: "older than the sources",
			path: func(t *testing.T, _ *buildStub) string {
				p := filepath.Join(t.TempDir(), "gt")
				writeAt(t, p, "#!/bin/sh\nexit 0\n", 0o755, time.Now().Add(-2*time.Hour))
				return p
			},
			wantReason: "older than the build input",
		},
		{
			name: "older than an embedded input",
			path: func(t *testing.T, stub *buildStub) string {
				writeAt(t, filepath.Join(stub.src, "internal", "config", "roles", "mayor.toml"), "name = \"mayor\"\n", 0o644, time.Now().Add(-10*time.Minute))
				p := filepath.Join(t.TempDir(), "gt")
				writeAt(t, p, "#!/bin/sh\nexit 0\n", 0o755, time.Now().Add(-30*time.Minute))
				return p
			},
			wantReason: "older than the build input internal/config/roles/mayor.toml",
		},
		{
			name: "build inputs unreadable",
			path: func(t *testing.T, stub *buildStub) string {
				if err := os.RemoveAll(stub.src); err != nil {
					t.Fatalf("removing the module root: %v", err)
				}
				p := filepath.Join(t.TempDir(), "gt")
				writeAt(t, p, "#!/bin/sh\nexit 0\n", 0o755, time.Now().Add(-30*time.Minute))
				return p
			},
			wantReason: "cannot read the build inputs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := stubBuild(t)
			path := tc.path(t, stub)
			t.Setenv(sweepGTBinEnv, path)

			got, err := buildGTBinary()
			if err != nil {
				t.Fatalf("buildGTBinary: %v", err)
			}
			if len(stub.builds) != 1 {
				t.Fatalf("go build ran %d time(s); want 1, as when the variable is unset: %v", len(stub.builds), stub.builds)
			}
			args := stub.builds[0]
			i := slices.Index(args, "-o")
			if i < 0 || i+1 >= len(args) {
				t.Fatalf("go build args name no -o output: %v", args)
			}
			if got != args[i+1] {
				t.Errorf("buildGTBinary = %q, want the path go build wrote %q", got, args[i+1])
			}
			wantNotice := fmt.Sprintf("ignoring %s=%q: ", sweepGTBinEnv, path)
			if !strings.Contains(stub.notice.String(), wantNotice) || !strings.Contains(stub.notice.String(), tc.wantReason) {
				t.Errorf("notice = %q, want it to name %q and the reason %q", stub.notice.String(), wantNotice, tc.wantReason)
			}
		})
	}
}
