package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// gtBinaryOnce guards the single build of the gt binary that the CLI-level
// tests in this package share.
//
// Why a sync.Once and not just a cached string (gt-fo3h): linking ./cmd/gt
// pulls in the whole dependency tree, dolt included, and Go does not cache
// the link step — measured 2026-09-19 at ~100-120s per invocation on a loaded
// host, warm build cache and identical inputs included. A bare `var` cache
// let concurrent t.Parallel callers each start their own `go build` against
// the same output path, so the package paid the link more than once; and
// because a failure left the cache empty, every caller after a failed build
// retried it from scratch. One build, one result — success or failure — per
// test process.
var (
	gtBinaryOnce sync.Once
	gtBinaryPath string
	gtBinaryErr  error
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

	// The output path is shared across every process running as this user
	// (os.TempDir() is per-user, not per-worktree), so two concurrent test
	// runs — a polecat's and the refinery's, say — would otherwise link the
	// binary to the same file and race on it. Key the name on the project
	// root: one worktree, one binary.
	sum := sha256.Sum256([]byte(projectRoot))
	binaryName := "gt-integration-test-" + hex.EncodeToString(sum[:4])
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	tmpBinary := filepath.Join(os.TempDir(), binaryName)

	// Dolt's go-icu-regex cgo build only reaches this point, so only this
	// helper needs the include-path diagnosis (gt-mjll).
	if err := testutil.VerifyCgoIncludePath(); err != nil {
		return "", err
	}

	// Must set BuiltProperly=1 via ldflags, otherwise binary refuses to run
	ldflags := "-X github.com/steveyegge/gastown/internal/cmd.BuiltProperly=1"
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", tmpBinary, "./cmd/gt")
	cmd.Dir = projectRoot
	cmd.Env = append(os.Environ(), icuCgoEnv()...)
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
