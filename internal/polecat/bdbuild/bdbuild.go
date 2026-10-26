// Package bdbuild compiles the polecat image's bd from one commit of a beads
// checkout — by default the commit the host's installed bd reports — so the
// host and every sandbox run the same schema ceiling (gt-7b7a5).
package bdbuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/util"
)

// The image runs its bd on linux/arm64 (deploy/polecat/Dockerfile), whatever
// host builds it.
const (
	goos   = "linux"
	goarch = "arm64"
	cgo    = "0"
)

// hostEnv names the host variables the compile passes through: those that
// locate the toolchain, the Go caches and the module proxy, so the compile
// reuses what the host already has. Anything else — a shell's GOFLAGS,
// GOEXPERIMENT, GOARM64, GOTOOLCHAIN or GOWORK — would change what gets
// compiled, so it is dropped. buildEnv also sets GOENV=off, so the host's
// `go env -w` file cannot set those variables either.
var hostEnv = map[string]struct{}{
	"PATH": {}, "HOME": {}, "TMPDIR": {},
	"GOROOT": {}, "GOPATH": {}, "GOCACHE": {}, "GOMODCACHE": {},
	"GOPROXY": {}, "GOSUMDB": {}, "GONOSUMDB": {}, "GONOSUMCHECK": {},
	"GOPRIVATE": {}, "GONOPROXY": {}, "GOINSECURE": {}, "GOAUTH": {},
	"SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "NO_PROXY": {},
	"http_proxy": {}, "https_proxy": {}, "no_proxy": {},
}

// buildEnv returns the environment the compile runs with: hostEnv's variables
// from environ, plus the cross-compile settings. A nil environ reads the host's.
// GOENV=off stops the host's `go env -w` file from setting GOFLAGS, GOWORK or
// anything else that changes what is compiled.
func buildEnv(environ []string) []string {
	if environ == nil {
		environ = os.Environ()
	}
	env := make([]string, 0, len(environ)+4)
	for _, kv := range environ {
		if key, _, ok := strings.Cut(kv, "="); ok {
			if _, keep := hostEnv[key]; keep {
				env = append(env, kv)
			}
		}
	}
	return append(env, "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED="+cgo, "GOENV=off")
}

// Git runs git with args in dir and returns its standard output; a directory
// that is not a git checkout fails the way git does.
type Git func(dir string, args ...string) ([]byte, error)

// ExecGit runs the host's git.
func ExecGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, lastLine(stderr.String()))
	}
	return out, nil
}

// Go runs one go command in dir with env as its whole environment and returns
// its standard output.
type Go func(dir string, env []string, args ...string) ([]byte, error)

// ExecGo runs the host's go, with dir as the working directory so the compile
// reads the worktree's module.
func ExecGo(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, lastLine(stderr.String()))
	}
	return out, nil
}

// Commit returns the commit the image's bd is built from: override when it is
// set, otherwise the commit `bd version --json` reports for the installed bd.
// run is the bd runner, so a caller tests the selection without a bd on PATH.
func Commit(ctx context.Context, override string, run deps.BDRunner) (string, error) {
	if c := strings.TrimSpace(override); c != "" {
		if !isCommit(c) {
			return "", fmt.Errorf("commit %q is not a 7+ character hex commit", c)
		}
		return c, nil
	}
	stdout, stderr, err := run(ctx, nil, "version", "--json")
	if err != nil {
		return "", fmt.Errorf("cannot read the installed bd's commit: bd version --json failed: %v%s", err, stderrSuffix(stderr))
	}
	info, err := deps.ParseBDVersionJSON(stdout)
	if err != nil {
		return "", fmt.Errorf("cannot read the installed bd's commit: %w", err)
	}
	if info.Commit == "" {
		return "", fmt.Errorf("the installed bd reports no commit (bd %s, build %s); install a beads fork build that records one, or name the commit (-commit, BD_COMMIT under make)", info.Version, orUnknown(firstNonEmpty(info.BuildID, info.Build)))
	}
	if !isCommit(info.Commit) {
		return "", fmt.Errorf("the installed bd reports commit %q, which is not a 7+ character hex commit; rebuild bd, or name the commit (-commit, BD_COMMIT under make)", info.Commit)
	}
	return info.Commit, nil
}

// Options is one compile of the image's bd.
type Options struct {
	// Src is the beads checkout whose object store holds the commit.
	Src string
	// Out is the bd binary to write.
	Out string
	// Commit is the commit to build, Commit's answer.
	Commit string
	// BuildDir is the directory the clean worktree is made in. Empty makes a
	// temporary one.
	BuildDir string
	// Env is the environment the compile starts from. Nil reads the host's.
	// Compile drops the variables that would change what is compiled and sets
	// GOOS, GOARCH and CGO_ENABLED itself.
	Env []string
}

// Compile writes opt.Out (resolved to an absolute path, so it survives the
// worktree's removal) built from opt.Commit in a clean worktree of opt.Src,
// and returns the commit's full sha. The commit is read from the checkout's
// object store and nothing is fetched, so one the checkout does not hold is
// refused with a message naming it. The worktree is removed afterwards.
func Compile(opt Options, git Git, goBuild Go) (string, error) {
	if opt.Src == "" {
		return "", errors.New("no beads checkout named (make polecat-image BD_SRC=<checkout>)")
	}
	if opt.Out == "" {
		return "", errors.New("no output path named for the bd binary")
	}
	if !isCommit(opt.Commit) {
		return "", fmt.Errorf("cannot build bd from %q: not a 7+ character hex commit", opt.Commit)
	}
	// Resolve the output before entering the worktree: a relative path would
	// otherwise be written inside the worktree, which is deleted afterwards.
	out, err := filepath.Abs(opt.Out)
	if err != nil {
		return "", fmt.Errorf("cannot resolve the bd output path %q: %w", opt.Out, err)
	}
	rev, err := git(opt.Src, "rev-parse", "--verify", opt.Commit+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("the beads checkout %s does not hold commit %s: %w\n  fix: fetch it there, or install a bd built from a commit the checkout holds", opt.Src, opt.Commit, err)
	}
	commit := strings.TrimSpace(string(rev))

	dir, err := os.MkdirTemp(opt.BuildDir, "bd-"+shortCommit(commit)+"-")
	if err != nil {
		return "", fmt.Errorf("cannot make a directory for the bd worktree: %w", err)
	}
	if _, err := git(opt.Src, "worktree", "add", "--detach", dir, commit); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("cannot check commit %s out of %s into %s: %w", commit, opt.Src, dir, err)
	}
	defer removeWorktree(opt.Src, dir, git)

	env := buildEnv(opt.Env)
	// -X main.Commit stamps the commit `bd version --json` reports, the same
	// variable scripts/refresh-bd-command-tree.sh stamps.
	if _, err := goBuild(dir, env, "build", "-trimpath", "-ldflags", "-X main.Commit="+commit, "-o", out, "./cmd/bd"); err != nil {
		return "", fmt.Errorf("cannot build bd at %s: %w", commit, err)
	}
	return commit, nil
}

// removeWorktree unregisters dir from src and deletes it. A removal that fails
// leaves a stale registration for the operator's next `git worktree prune`; the
// build has already succeeded, so it is not the build's error, and pruning here
// could also drop worktrees the operator has registered.
func removeWorktree(src, dir string, git Git) {
	_, _ = git(src, "worktree", "remove", "--force", dir)
	os.RemoveAll(dir)
}

// isCommit reports whether s has the shape of git's abbreviated or full commit
// sha: the shape `git rev-parse HEAD` prints.
func isCommit(s string) bool {
	if len(s) < 7 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// shortCommit is the 12-character abbreviation a worktree directory is named
// with.
func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// stderrSuffix is git's or bd's own reason for a failure, when it printed one.
func stderrSuffix(stderr []byte) string {
	if s := strings.TrimSpace(string(stderr)); s != "" {
		return ": " + util.FirstLine(s)
	}
	return ""
}

// lastLine prints the last non-blank line of s: a tool's reason for a failure
// is its last line, and everything before is noise.
func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no output"
	}
	lines := strings.Split(s, "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
