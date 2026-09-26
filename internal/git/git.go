// Package git provides a wrapper for git operations via subprocess.
package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

var errNoComparisonRefs = errors.New("no comparison refs resolved")

// GitError contains raw output from a git command for agent observation.
// ZFC: Callers observe the raw output and decide what to do.
// The error interface methods provide human-readable messages, but agents
// should use Stdout/Stderr for programmatic observation.
type GitError struct {
	Command string // The git command that failed (e.g., "merge", "push")
	Args    []string
	Stdout  string // Raw stdout output
	Stderr  string // Raw stderr output
	Err     error  // Underlying error (e.g., exit code)
}

func (e *GitError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("git %s: %s", e.Command, e.Stderr)
	}
	return fmt.Sprintf("git %s: %v", e.Command, e.Err)
}

func (e *GitError) Unwrap() error {
	return e.Err
}

// moveDir moves a directory from src to dest. It first tries os.Rename for
// efficiency, but falls back to copy+delete if src and dest are on different
// filesystems (which causes EXDEV error on rename).
func moveDir(src, dest string) error {
	// Try rename first - works if same filesystem
	if err := os.Rename(src, dest); err == nil {
		return nil
	}

	// Rename failed, use platform-specific copy for cross-filesystem moves
	if err := copyDirPreserving(src, dest); err != nil {
		return fmt.Errorf("copying directory: %w", err)
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("removing source after copy: %w", err)
	}
	return nil
}

// Git wraps git operations for a working directory.
type Git struct {
	workDir string
	gitDir  string // Optional: explicit git directory (for bare repos)
}

// ErrUnsafeTownRootGitMutation is returned when a mutating git operation would
// act on the Gas Town town-root repository or town-root runtime paths.
var ErrUnsafeTownRootGitMutation = errors.New("unsafe git mutation targets Gas Town town root")

// NewGit creates a new Git wrapper for the given directory.
func NewGit(workDir string) *Git {
	return &Git{workDir: workDir}
}

// NewGitWithDir creates a Git wrapper with an explicit git directory.
// This is used for bare repos where gitDir points to the .git directory
// and workDir may be empty or point to a worktree.
func NewGitWithDir(gitDir, workDir string) *Git {
	return &Git{gitDir: gitDir, workDir: workDir}
}

// WorkDir returns the working directory for this Git instance.
func (g *Git) WorkDir() string {
	return g.workDir
}

// IsRepo returns true if the workDir is a git repository.
func (g *Git) IsRepo() bool {
	_, err := g.run("rev-parse", "--git-dir")
	return err == nil
}

// GitDir returns the absolute path of the git directory backing workDir. For a
// linked worktree that is the per-worktree directory
// (…/.repo.git/worktrees/<name>), not the shared git dir, so a file written
// there belongs to this worktree alone.
func (g *Git) GitDir() (string, error) {
	return g.run("rev-parse", "--absolute-git-dir")
}

// TopLevel returns the root of the git worktree containing the working
// directory, or an error when there is none.
//
// Git resolves upward, so a directory that merely sits *inside* a repository
// returns that repository's root — the enclosing repo's state, not the
// directory's own. Callers that must know whether a path is itself a worktree
// (a polecat worktree, say) have to compare the result against the path they
// asked about rather than treating a successful call as proof.
func (g *Git) TopLevel() (string, error) {
	out, err := g.run("rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	top := strings.TrimSpace(out)
	if top == "" {
		return "", fmt.Errorf("git rev-parse --show-toplevel returned no path for %s", g.workDir)
	}
	return filepath.Clean(top), nil
}

// run executes a git command and returns trimmed stdout.
func (g *Git) run(args ...string) (string, error) {
	out, err := g.runOutput(args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// runOutput executes a git command and returns RAW, untrimmed stdout.
//
// Most callers want run()'s trimmed contract. This exists for output formats
// where a leading or trailing character is meaningful, not incidental
// whitespace — `git status --porcelain`'s first column can be a literal
// space (index clean, worktree dirty), and TrimSpace on the whole blob eats
// exactly that space when it starts the first line, shifting every column
// and truncating the path by one character (" M README.md" -> "M README.md"
// -> parsed as code "M " path "EADME.md"). Status() uses this instead of run().
func (g *Git) runOutput(args ...string) (string, error) {
	if err := g.guardUnsafeTownRootMutation(args); err != nil {
		return "", err
	}

	// A missing workDir makes exec.Cmd fail its internal chdir during
	// fork/exec, and Go folds that failure into a PathError blamed on the
	// git binary itself: "fork/exec /opt/homebrew/bin/git: no such file or
	// directory". That reads as a broken git install when the real problem
	// is a gone working directory (e.g. a polecat whose worktree was
	// removed) — check for it up front so the error says what actually
	// happened.
	if g.workDir != "" {
		if info, statErr := os.Stat(g.workDir); statErr != nil {
			if os.IsNotExist(statErr) {
				return "", g.wrapError(fmt.Errorf("working directory does not exist: %s", g.workDir), "", "", args)
			}
			return "", g.wrapError(fmt.Errorf("checking working directory %s: %w", g.workDir, statErr), "", "", args)
		} else if !info.IsDir() {
			return "", g.wrapError(fmt.Errorf("working directory is not a directory: %s", g.workDir), "", "", args)
		}
	}

	// If gitDir is set (bare repo), prepend --git-dir flag
	if g.gitDir != "" {
		args = append([]string{"--git-dir=" + g.gitDir}, args...)
	}

	cmd := exec.Command("git", args...)
	util.SetDetachedProcessGroup(cmd)
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", g.wrapError(err, stdout.String(), stderr.String(), args)
	}

	return stdout.String(), nil
}

// pushTimeout is the maximum time a git push is allowed to run before being
// killed. This prevents gt done from hanging indefinitely when the remote
// (e.g. GitLab) is unreachable or slow.
const pushTimeout = 60 * time.Second

// remoteQueryTimeout bounds read-only network queries against a remote (e.g.
// ls-remote). Without this, a slow or unreachable remote hangs callers
// indefinitely — including patrol-loop code paths (witness zombie/completion
// detection call this once per polecat with an active MR) where a single
// stuck call blocks the entire scan (gt-ftt).
const remoteQueryTimeout = 30 * time.Second

// notesFetchTimeout bounds the fetch of the remote notes ref taken on the
// PushNotes retry path, so a race with another notes writer can never turn a
// push into an unbounded hang.
const notesFetchTimeout = 30 * time.Second

// timedCommandWaitDelay bounds how long a timed-out git command may keep
// Run() waiting on its output pipes after the context is done. The process
// group kill (SIGTERM, then SIGKILL after util.ProcessGroupKillGrace) reaches
// every helper; this is the backstop should one still hold a pipe.
var timedCommandWaitDelay = util.ProcessGroupKillGrace + 3*time.Second

// boundRemoteCommand makes a context-bound git command killable as a whole.
// A remote command is not one process: over http(s) git forks
// git-remote-http, which inherits the stdout/stderr pipes. The default
// context cancel kills only git, so the helper — still waiting on a stalled
// server — keeps the pipe open and cmd.Run() never returns. Canceling the
// process group kills the helper too, and WaitDelay caps the pipe wait.
func boundRemoteCommand(cmd *exec.Cmd) {
	util.SetProcessGroup(cmd)
	cmd.WaitDelay = timedCommandWaitDelay
}

// runWithTimeout executes a git command with a deadline. If the command does
// not finish within the timeout, its whole process group (git and any remote
// helper) is killed and an error is returned.
func (g *Git) runWithTimeout(timeout time.Duration, args ...string) (_ string, _ error) { //nolint:unparam // string return kept for consistency with Run()
	if err := g.guardUnsafeTownRootMutation(args); err != nil {
		return "", err
	}

	if g.gitDir != "" {
		args = append([]string{"--git-dir=" + g.gitDir}, args...)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	boundRemoteCommand(cmd)
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git %s timed out after %v (remote may be unreachable)", args[0], timeout)
		}
		return "", g.wrapError(err, stdout.String(), stderr.String(), args)
	}

	return strings.TrimSpace(stdout.String()), nil
}

// runWithEnv executes a git command with additional environment variables.
func (g *Git) runWithEnv(args []string, extraEnv []string) (_ string, _ error) { //nolint:unparam // string return kept for consistency with Run()
	return g.runWithEnvAndTimeout(args, extraEnv, 0)
}

// runWithEnvAndTimeout executes a git command with extra env vars and an
// optional timeout. Pass 0 for no timeout.
func (g *Git) runWithEnvAndTimeout(args []string, extraEnv []string, timeout time.Duration) (_ string, _ error) {
	if err := g.guardUnsafeTownRootMutation(args); err != nil {
		return "", err
	}

	if g.gitDir != "" {
		args = append([]string{"--git-dir=" + g.gitDir}, args...)
	}

	var cmd *exec.Cmd
	var cancel context.CancelFunc
	ctx := context.Background()
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		cmd = exec.CommandContext(ctx, "git", args...)
	} else {
		cmd = exec.Command("git", args...)
	}
	if cancel != nil {
		defer cancel()
		boundRemoteCommand(cmd)
	} else {
		util.SetDetachedProcessGroup(cmd)
	}

	if g.workDir != "" {
		cmd.Dir = g.workDir
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if timeout > 0 {
			// Check if the context's deadline was exceeded
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", fmt.Errorf("git %s timed out after %v (remote may be unreachable)", args[0], timeout)
			}
		}
		return "", g.wrapError(err, stdout.String(), stderr.String(), args)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runWithStdin executes a git command, feeding it stdin, and returns stdout.
// Used for piping one git command's output into another (e.g. diff | patch-id)
// without shelling out to a real shell pipeline.
func (g *Git) runWithStdin(stdin string, args ...string) (string, error) {
	if err := g.guardUnsafeTownRootMutation(args); err != nil {
		return "", err
	}

	if g.gitDir != "" {
		args = append([]string{"--git-dir=" + g.gitDir}, args...)
	}

	cmd := exec.Command("git", args...)
	util.SetDetachedProcessGroup(cmd)
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}
	cmd.Stdin = strings.NewReader(stdin)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", g.wrapError(err, stdout.String(), stderr.String(), args)
	}

	return strings.TrimSpace(stdout.String()), nil
}

func (g *Git) guardUnsafeTownRootMutation(args []string) error {
	cmd, rest := gitSubcommand(args)
	if cmd == "" {
		return nil
	}
	effectiveWorkDir := gitEffectiveWorkDir(args, g.workDir)

	if gitSubcommandMutatesWorktree(cmd, rest) {
		if err := EnsureSafeMutationWorkDir(effectiveWorkDir); err != nil {
			return fmt.Errorf("%w: git %s", err, strings.Join(args, " "))
		}
	}

	for _, target := range protectedWorktreeTargets(cmd, rest, effectiveWorkDir) {
		return fmt.Errorf("%w: git worktree target %s", ErrUnsafeTownRootGitMutation, target)
	}

	return nil
}

func gitEffectiveWorkDir(args []string, workDir string) string {
	effective := workDir
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-C" && i+1 < len(args):
			effective = gitPathAbs(args[i+1], effective)
			i++
		case arg == "--work-tree" && i+1 < len(args):
			effective = gitPathAbs(args[i+1], effective)
			i++
		case strings.HasPrefix(arg, "--work-tree="):
			effective = gitPathAbs(strings.TrimPrefix(arg, "--work-tree="), effective)
		case arg == "-c" || arg == "--git-dir" || arg == "--namespace" || arg == "--config-env" || arg == "--exec-path":
			i++
		case strings.HasPrefix(arg, "--git-dir=") || strings.HasPrefix(arg, "--namespace=") || strings.HasPrefix(arg, "--config-env=") || strings.HasPrefix(arg, "--exec-path="):
			continue
		case strings.HasPrefix(arg, "-"):
			continue
		default:
			return effective
		}
	}
	return effective
}

// EnsureSafeMutationWorkDir fails when workDir's effective git worktree is the
// Gas Town town root. Raw git callsites use this before mutating commands.
func EnsureSafeMutationWorkDir(workDir string) error {
	if workDir == "" {
		return nil
	}

	topLevel, ok := gitTopLevel(workDir)
	if !ok {
		return nil
	}
	if isTownRoot(topLevel) {
		return fmt.Errorf("%w: %s resolves to town root git worktree %s", ErrUnsafeTownRootGitMutation, workDir, topLevel)
	}
	return nil
}

func gitTopLevel(workDir string) (string, bool) {
	cmd := exec.Command("git", "-C", workDir, "rev-parse", "--show-toplevel")
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	topLevel := strings.TrimSpace(string(out))
	if topLevel == "" {
		return "", false
	}
	abs, err := filepath.Abs(topLevel)
	if err != nil {
		return filepath.Clean(topLevel), true
	}
	return filepath.Clean(abs), true
}

func isTownRoot(path string) bool {
	return fileExists(filepath.Join(path, "mayor", "town.json")) || fileExists(filepath.Join(path, "mayor", "rigs.json"))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func gitSubcommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-C" || arg == "-c" || arg == "--git-dir" || arg == "--work-tree" || arg == "--namespace" || arg == "--config-env" || arg == "--exec-path":
			i++
			continue
		case strings.HasPrefix(arg, "--git-dir=") || strings.HasPrefix(arg, "--work-tree=") || strings.HasPrefix(arg, "--namespace=") || strings.HasPrefix(arg, "--config-env=") || strings.HasPrefix(arg, "--exec-path="):
			continue
		case arg == "--no-pager" || arg == "--bare" || arg == "--literal-pathspecs" || arg == "--no-replace-objects":
			continue
		case strings.HasPrefix(arg, "-"):
			continue
		default:
			return arg, args[i+1:]
		}
	}
	return "", nil
}

func gitSubcommandMutatesWorktree(cmd string, args []string) bool {
	switch cmd {
	case "checkout", "switch", "restore", "reset", "clean", "merge", "rebase", "pull", "rm", "mv", "cherry-pick", "revert", "am", "apply", "checkout-index", "read-tree", "sparse-checkout":
		return true
	case "stash":
		return stashArgsMutate(args)
	case "submodule":
		return submoduleArgsMutate(args)
	case "branch":
		return branchArgsMutate(args)
	case "worktree":
		return worktreeArgsMutate(args)
	case "symbolic-ref":
		return symbolicRefArgsMutate(args)
	case "update-ref":
		return true
	default:
		return false
	}
}

func stashArgsMutate(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "list", "show":
		return false
	default:
		return true
	}
}

func submoduleArgsMutate(args []string) bool {
	cmd := firstNonOptionSubcommand(args)
	if cmd == "" {
		return false
	}
	switch cmd {
	case "update", "add", "deinit", "sync", "set-url", "set-branch", "absorbgitdirs":
		return true
	default:
		return false
	}
}

func firstNonOptionSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

func branchArgsMutate(args []string) bool {
	if len(args) == 0 {
		return false
	}
	readOnly := false
	for _, arg := range args {
		switch arg {
		case "--show-current", "--list", "-l", "-r", "-a", "--contains", "--merged", "--no-merged", "--points-at":
			readOnly = true
		case "-d", "-D", "-f", "-m", "-M", "-c", "-C", "--delete", "--move", "--copy", "--force", "--set-upstream-to", "--unset-upstream", "--track":
			return true
		}
		if strings.HasPrefix(arg, "--format") {
			readOnly = true
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return !readOnly
	}
	return false
}

func worktreeArgsMutate(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "add", "remove", "move", "prune":
		return true
	default:
		return false
	}
}

func symbolicRefArgsMutate(args []string) bool {
	nonOptions := 0
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			nonOptions++
		}
	}
	return nonOptions > 1
}

func protectedWorktreeTargets(cmd string, args []string, baseDir string) []string {
	if cmd != "worktree" || len(args) == 0 {
		return nil
	}

	var targets []string
	switch args[0] {
	case "add":
		if target := firstWorktreeAddTarget(args[1:]); target != "" {
			targets = append(targets, target)
		}
	case "remove":
		if target := firstNonOptionPath(args[1:], nil); target != "" {
			targets = append(targets, target)
		}
	case "move":
		targets = append(targets, nonOptionPaths(args[1:], nil, 2)...)
	}

	protected := make([]string, 0, len(targets))
	for _, target := range targets {
		abs := gitPathAbs(target, baseDir)
		if protectedTownRuntimePath(abs) {
			protected = append(protected, abs)
		}
	}
	return protected
}

func firstWorktreeAddTarget(args []string) string {
	valueOptions := map[string]bool{"-b": true, "-B": true, "--orphan": true, "--reason": true}
	return firstNonOptionPath(args, valueOptions)
}

func firstNonOptionPath(args []string, valueOptions map[string]bool) string {
	paths := nonOptionPaths(args, valueOptions, 1)
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

func nonOptionPaths(args []string, valueOptions map[string]bool, limit int) []string {
	paths := make([]string, 0, limit)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if valueOptions[arg] {
			i++
			continue
		}
		if strings.HasPrefix(arg, "--reason=") || strings.HasPrefix(arg, "--orphan=") {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		paths = append(paths, arg)
		if len(paths) == limit {
			return paths
		}
	}
	return paths
}

func gitPathAbs(path, baseDir string) string {
	if baseDir == "" {
		baseDir = "."
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(resolveExistingSymlinkAncestors(abs))
}

func resolveExistingSymlinkAncestors(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil || rel == "." {
				return resolved
			}
			return filepath.Join(resolved, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
	}
}

func protectedTownRuntimePath(path string) bool {
	abs := filepath.Clean(path)
	for dir := abs; ; dir = filepath.Dir(dir) {
		if isTownRoot(dir) {
			if samePath(abs, dir) {
				return true
			}
			rel, err := filepath.Rel(dir, abs)
			if err != nil {
				return false
			}
			first := rel
			if idx := strings.IndexRune(rel, filepath.Separator); idx >= 0 {
				first = rel[:idx]
			}
			switch first {
			case "mayor", ".dolt-data", ".runtime", ".beads", "daemon":
				return true
			default:
				return false
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
	}
}

func samePath(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
}

// wrapError wraps git errors with context.
// ZFC: Returns GitError with raw output for agent observation.
// Does not detect or interpret error types - agents should observe and decide.
func (g *Git) wrapError(err error, stdout, stderr string, args []string) error {
	stdout = strings.TrimSpace(stdout)
	stderr = strings.TrimSpace(stderr)

	// Determine command name (first arg, or first non-flag arg)
	command := ""
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			command = arg
			break
		}
	}
	if command == "" && len(args) > 0 {
		command = args[0]
	}

	return &GitError{
		Command: command,
		Args:    args,
		Stdout:  stdout,
		Stderr:  stderr,
		Err:     err,
	}
}

// cloneOptions configures a clone operation for cloneInternal.
type cloneOptions struct {
	bare         bool   // Pass --bare to git clone
	reference    string // Pass --reference-if-able <path> to git clone
	singleBranch bool   // Pass --single-branch to git clone (only fetch default branch)
	depth        int    // Pass --depth N to git clone (shallow clone); 0 means full history
	branch       string // Pass --branch <name> to git clone (checkout specific branch)
	filter       string // Pass --filter=<spec> to git clone (e.g. "blob:none", "tree:0")
}

// cloneInternal runs `git clone` in an isolated temp directory, moves the result
// to dest, and applies post-clone configuration (hooks or refspec).
func (g *Git) cloneInternal(url, dest string, opts cloneOptions) error {
	dest = gitPathAbs(dest, "")
	if protectedTownRuntimePath(dest) {
		return fmt.Errorf("%w: clone destination %s", ErrUnsafeTownRootGitMutation, dest)
	}

	// Ensure destination directory's parent exists
	destParent := filepath.Dir(dest)
	if err := os.MkdirAll(destParent, 0755); err != nil {
		return fmt.Errorf("creating destination parent: %w", err)
	}
	// Run clone from a temporary directory to completely isolate from any
	// git repo at the process cwd. Then move the result to the destination.
	tmpDir, err := os.MkdirTemp("", "gt-clone-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tmpDest := filepath.Join(tmpDir, filepath.Base(dest))

	// Build clone args
	var args []string
	// Windows symlink fix for non-bare reference clones
	if opts.reference != "" && !opts.bare && runtime.GOOS == "windows" {
		args = append(args, "-c", "core.symlinks=true")
	}
	args = append(args, "clone")
	if opts.bare {
		args = append(args, "--bare")
	}
	if opts.singleBranch {
		args = append(args, "--single-branch")
	}
	if opts.filter != "" {
		args = append(args, "--filter="+opts.filter)
	}
	if opts.depth > 0 {
		args = append(args, "--depth", fmt.Sprintf("%d", opts.depth))
	}
	if opts.branch != "" {
		args = append(args, "--branch", opts.branch)
	}
	if opts.reference != "" {
		args = append(args, "--reference-if-able", opts.reference)
	}
	args = append(args, url, tmpDest)

	cmd := exec.Command("git", args...)
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = tmpDir
	cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+tmpDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return g.wrapError(err, stdout.String(), stderr.String(), args)
	}

	// Move to final destination (handles cross-filesystem moves)
	if err := moveDir(tmpDest, dest); err != nil {
		return fmt.Errorf("moving clone to destination: %w", err)
	}

	// Post-clone configuration
	if opts.bare {
		// Configure refspec so worktrees can fetch and see origin/* refs.
		// For single-branch shallow clones, only set the config without
		// fetching all branches (which would defeat the purpose of --single-branch).
		return configureRefspec(dest, opts.singleBranch)
	}
	// Configure hooks path for Gas Town clones
	if err := configureHooksPath(dest); err != nil {
		return err
	}
	// Initialize submodules if present
	return InitSubmodules(dest)
}

// Clone clones a repository to the destination.
// Uses --single-branch --depth 1 for efficiency on repos with many branches.
func (g *Git) Clone(url, dest string) error {
	return g.cloneInternal(url, dest, cloneOptions{singleBranch: true, depth: 1})
}

// CloneWithReference clones a repository using a local repo as an object reference.
// This saves disk by sharing objects without changing remotes.
// Uses --single-branch --depth 1 for efficiency on repos with many branches.
func (g *Git) CloneWithReference(url, dest, reference string) error {
	return g.cloneInternal(url, dest, cloneOptions{reference: reference, singleBranch: true, depth: 1})
}

// CloneBranch clones a specific branch with --single-branch --depth 1.
// Use this when you know which branch you need (avoids fetching all branches).
func (g *Git) CloneBranch(url, dest, branch string) error {
	return g.cloneInternal(url, dest, cloneOptions{singleBranch: true, depth: 1, branch: branch})
}

// CloneBranchWithReference clones a specific branch using a local repo as reference.
func (g *Git) CloneBranchWithReference(url, dest, branch, reference string) error {
	return g.cloneInternal(url, dest, cloneOptions{singleBranch: true, depth: 1, branch: branch, reference: reference})
}

// CloneBare clones a repository as a bare repo (no working directory).
// This is used for the shared repo architecture where all worktrees share a single git database.
func (g *Git) CloneBare(url, dest string) error {
	return g.cloneInternal(url, dest, cloneOptions{bare: true, singleBranch: true, depth: 1})
}

// CloneBareWithBranch clones a bare repo, checking out a specific branch.
// Use this when the desired default branch differs from the remote HEAD.
func (g *Git) CloneBareWithBranch(url, dest, branch string) error {
	return g.cloneInternal(url, dest, cloneOptions{bare: true, singleBranch: true, depth: 1, branch: branch})
}

// CloneBarePartial clones a bare repo with a partial clone filter (e.g. "blob:none", "tree:0").
// Does not use --depth since partial clones handle size reduction via the filter.
func (g *Git) CloneBarePartial(url, dest, filter string) error {
	return g.cloneInternal(url, dest, cloneOptions{bare: true, singleBranch: true, filter: filter})
}

// CloneBarePartialWithBranch clones a bare repo with a partial clone filter and specific branch.
func (g *Git) CloneBarePartialWithBranch(url, dest, filter, branch string) error {
	return g.cloneInternal(url, dest, cloneOptions{bare: true, singleBranch: true, filter: filter, branch: branch})
}

// CloneBarePartialWithReference clones a bare repo with a partial clone filter and local reference.
func (g *Git) CloneBarePartialWithReference(url, dest, filter, reference string) error {
	return g.cloneInternal(url, dest, cloneOptions{bare: true, singleBranch: true, filter: filter, reference: reference})
}

// CloneBarePartialWithReferenceAndBranch clones a bare repo with a partial clone filter, local reference, and specific branch.
func (g *Git) CloneBarePartialWithReferenceAndBranch(url, dest, filter, reference, branch string) error {
	return g.cloneInternal(url, dest, cloneOptions{bare: true, singleBranch: true, filter: filter, reference: reference, branch: branch})
}

// CloneBranchPartialWithReference clones a specific branch with a partial clone filter and reference.
func (g *Git) CloneBranchPartialWithReference(url, dest, branch, filter, reference string) error {
	return g.cloneInternal(url, dest, cloneOptions{singleBranch: true, filter: filter, branch: branch, reference: reference})
}

// CloneBranchPartial clones a specific branch with a partial clone filter.
func (g *Git) CloneBranchPartial(url, dest, branch, filter string) error {
	return g.cloneInternal(url, dest, cloneOptions{singleBranch: true, filter: filter, branch: branch})
}

// configureHooksPath sets core.hooksPath to use the repo's .githooks directory
// if it exists. This ensures Gas Town agents use the pre-push hook that blocks
// pushes to non-main branches (internal PRs are not allowed).
func configureHooksPath(repoPath string) error {
	hooksDir := filepath.Join(repoPath, ".githooks")
	if _, err := os.Stat(hooksDir); os.IsNotExist(err) {
		// No .githooks directory, nothing to configure
		return nil
	}

	cmd := exec.Command("git", "-C", repoPath, "config", "core.hooksPath", ".githooks")
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("configuring hooks path: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ConfigureHooksPath sets core.hooksPath for the repo/worktree if .githooks exists.
func (g *Git) ConfigureHooksPath() error {
	return configureHooksPath(g.workDir)
}

// configureRefspec sets remote.origin.fetch to the standard refspec for bare repos.
// Bare clones don't have this set by default, which breaks worktrees that need to
// fetch and see origin/* refs. Without this, `git fetch` only updates FETCH_HEAD
// and origin/main never appears in refs/remotes/origin/main.
// See: https://github.com/anthropics/gastown/issues/286
//
// When singleBranch is true, fetches only the default branch's ref instead of all
// branches. This prevents failures on repos with many branches where a full fetch
// would error with "some local refs could not be updated".
func configureRefspec(repoPath string, singleBranch bool) error {
	gitDir := repoPath
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil {
		gitDir = filepath.Join(repoPath, ".git")
	}
	gitDir = filepath.Clean(gitDir)

	var stderr bytes.Buffer
	configCmd := exec.Command("git", "--git-dir", gitDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	util.SetDetachedProcessGroup(configCmd)
	configCmd.Stderr = &stderr
	if err := configCmd.Run(); err != nil {
		return fmt.Errorf("configuring refspec: %s", strings.TrimSpace(stderr.String()))
	}

	// Empty remotes clone successfully but have no refs to fetch. Let callers
	// perform their own empty-repository validation instead of returning a
	// misleading "couldn't find remote ref" error from the fetch below.
	var refsStderr bytes.Buffer
	refsCmd := exec.Command("git", "--git-dir", gitDir, "show-ref", "--quiet")
	util.SetDetachedProcessGroup(refsCmd)
	refsCmd.Stderr = &refsStderr
	if err := refsCmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("checking refs: %s", strings.TrimSpace(refsStderr.String()))
	}

	if singleBranch {
		// For shallow single-branch clones, fetch only the HEAD branch to create
		// the origin/<branch> ref that worktrees need. A full `git fetch origin`
		// would try to fetch ALL remote branches (due to the refspec we just set),
		// which fails on repos with many branches.
		//
		// Detect HEAD branch name, then fetch only that specific branch.
		var headOut bytes.Buffer
		headCmd := exec.Command("git", "--git-dir", gitDir, "symbolic-ref", "HEAD")
		util.SetDetachedProcessGroup(headCmd)
		headCmd.Stdout = &headOut
		headCmd.Stderr = &stderr
		if err := headCmd.Run(); err != nil {
			// Fallback: if HEAD is detached, try fetching all (shouldn't happen for clones)
			fetchCmd := exec.Command("git", "--git-dir", gitDir, "fetch", "--depth", "1", "origin")
			util.SetDetachedProcessGroup(fetchCmd)
			fetchCmd.Stderr = &stderr
			if fetchErr := fetchCmd.Run(); fetchErr != nil {
				return fmt.Errorf("fetching origin: %s", strings.TrimSpace(stderr.String()))
			}
			return nil
		}
		headRef := strings.TrimSpace(headOut.String())       // e.g. "refs/heads/main"
		branch := strings.TrimPrefix(headRef, "refs/heads/") // e.g. "main"
		refspec := branch + ":refs/remotes/origin/" + branch // e.g. "main:refs/remotes/origin/main"

		fetchCmd := exec.Command("git", "--git-dir", gitDir, "fetch", "--depth", "1", "origin", refspec)
		util.SetDetachedProcessGroup(fetchCmd)
		fetchCmd.Stderr = &stderr
		if err := fetchCmd.Run(); err != nil {
			return fmt.Errorf("fetching origin %s: %s", branch, strings.TrimSpace(stderr.String()))
		}
		return nil
	}

	fetchCmd := exec.Command("git", "--git-dir", gitDir, "fetch", "origin")
	util.SetDetachedProcessGroup(fetchCmd)
	fetchCmd.Stderr = &stderr
	if err := fetchCmd.Run(); err != nil {
		return fmt.Errorf("fetching origin: %s", strings.TrimSpace(stderr.String()))
	}

	return nil
}

// CloneBareWithReference clones a bare repository using a local repo as an object reference.
// Uses --single-branch --depth 1 for efficiency on repos with many branches.
func (g *Git) CloneBareWithReference(url, dest, reference string) error {
	return g.cloneInternal(url, dest, cloneOptions{bare: true, reference: reference, singleBranch: true, depth: 1})
}

// CloneBareWithReferenceAndBranch clones a bare repo using a local reference, checking out a specific branch.
func (g *Git) CloneBareWithReferenceAndBranch(url, dest, reference, branch string) error {
	return g.cloneInternal(url, dest, cloneOptions{bare: true, reference: reference, singleBranch: true, depth: 1, branch: branch})
}

// Checkout checks out the given ref.
func (g *Git) Checkout(ref string) error {
	_, err := g.run("checkout", ref)
	return err
}

// CheckoutDetach checks out the given ref without attaching to a local branch.
// This is useful in shared-worktree repos where the branch may already be
// checked out by another worktree, but this worktree only needs that commit.
func (g *Git) CheckoutDetach(ref string) error {
	_, err := g.run("checkout", "--detach", ref)
	return err
}

// CheckoutDetachForce detaches HEAD at ref, discarding index and working-tree
// changes that would block the switch (gt-0kk2).
func (g *Git) CheckoutDetachForce(ref string) error {
	_, err := g.run("checkout", "--detach", "--force", ref)
	return err
}

// CheckoutNewBranch creates a new branch from startPoint and checks it out.
// Equivalent to: git checkout -b <branch> <startPoint>
func (g *Git) CheckoutNewBranch(branch, startPoint string) error {
	_, err := g.run("checkout", "-b", branch, startPoint)
	return err
}

// CheckoutResetBranch creates or resets a branch to startPoint and checks it out.
// Equivalent to: git checkout -B <branch> <startPoint>. Unlike CheckoutNewBranch
// this does not fail when the branch already exists locally — useful when reusing
// a worktree that previously had the same branch checked out.
func (g *Git) CheckoutResetBranch(branch, startPoint string) error {
	_, err := g.run("checkout", "-B", branch, startPoint)
	return err
}

// Fetch fetches from the remote.
func (g *Git) Fetch(remote string) error {
	_, err := g.run("fetch", remote)
	return err
}

// FetchPrune fetches from the remote and prunes stale remote-tracking refs.
// This removes remote-tracking branches for branches that no longer exist on the remote.
func (g *Git) FetchPrune(remote string) error {
	_, err := g.run("fetch", "--prune", remote)
	return err
}

// RemoteQueryTimeout is the bound on read-only remote queries (ls-remote and
// small, targeted fetches), exported for callers that pick their own bound.
const RemoteQueryTimeout = remoteQueryTimeout

// FetchRefspecWithTimeout fetches one refspec from remote, killing git after
// timeout. A timeout is an error: callers that judge state from the fetched
// ref must treat it as "unknown", never as "absent".
func (g *Git) FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error {
	_, err := g.runWithTimeout(timeout, "fetch", remote, refspec)
	return err
}

// FetchBranch fetches a specific branch from the remote.
func (g *Git) FetchBranch(remote, branch string) error {
	_, err := g.run("fetch", remote, branch)
	return err
}

// FetchDefaultBranchWithTimeout refreshes only the remote's default branch,
// bounded by timeout.
//
// Scan loops that compare local work against the default branch need a fresh
// origin/<default> or they judge today's work by last month's main — but a
// plain Fetch can block forever on an unreachable remote, which is how one
// stuck call takes down a whole patrol scan (gt-ftt).
func (g *Git) FetchDefaultBranchWithTimeout(remote string, timeout time.Duration) error {
	_, err := g.runWithTimeout(timeout, "fetch", remote, g.RemoteDefaultBranch())
	return err
}

// FetchBranchShallow fetches a single branch with --depth 1 and creates the
// remote tracking ref (e.g. origin/<branch>). Use this on shallow single-branch
// clones to add a branch that wasn't included in the initial clone.
func (g *Git) FetchBranchShallow(remote, branch string) error {
	refspec := branch + ":refs/remotes/" + remote + "/" + branch
	_, err := g.run("fetch", "--depth", "1", remote, refspec)
	return err
}

// Pull pulls from the remote branch.
func (g *Git) Pull(remote, branch string) error {
	_, err := g.run("pull", remote, branch)
	return err
}

// ConfigurePushURL sets the push URL for a remote while keeping the fetch URL.
// This is useful for read-only upstream repos where you want to push to a fork.
// Example: ConfigurePushURL("origin", "https://github.com/user/fork.git")
func (g *Git) ConfigurePushURL(remote, pushURL string) error {
	_, err := g.run("remote", "set-url", remote, "--push", pushURL)
	return err
}

// ClearPushURL removes a custom push URL for a remote, reverting to the fetch URL.
// If no custom push URL is set, this is a no-op.
// Uses --unset-all to handle multi-valued pushurl entries; with --unset-all,
// exit code 5 unambiguously means "key not found" (safe to ignore).
func (g *Git) ClearPushURL(remote string) error {
	_, err := g.run("config", "--unset-all", fmt.Sprintf("remote.%s.pushurl", remote))
	if err != nil {
		// git config --unset-all returns exit code 5 if the key doesn't exist — that's fine.
		var ge *GitError
		if errors.As(err, &ge) {
			var exitErr *exec.ExitError
			if errors.As(ge.Err, &exitErr) && exitErr.ExitCode() == 5 {
				return nil
			}
		}
		return err
	}
	return nil
}

// GetPushURL returns the effective push URL for a remote.
// Note: git returns the fetch URL when no custom push URL is configured, so this
// never returns empty for a valid remote. Compare with RemoteURL to detect custom push URLs.
func (g *Git) GetPushURL(remote string) (string, error) {
	out, err := g.run("remote", "get-url", "--push", remote)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ForkBackedRemote reports whether pushes to remote land somewhere other than
// the canonical fetch base. This covers both split push URLs and fork remotes
// with a distinct upstream remote.
func (g *Git) ForkBackedRemote(remote string) bool {
	fetchURL, fetchErr := g.RemoteURL(remote)
	if fetchErr != nil {
		return false
	}
	pushURL, pushErr := g.GetPushURL(remote)
	if pushErr == nil && pushURL != "" && !sameGitRemoteURL(fetchURL, pushURL) {
		return true
	}
	upstreamURL, upstreamErr := g.GetUpstreamURL()
	return upstreamErr == nil && upstreamURL != "" && !sameGitRemoteURL(fetchURL, upstreamURL)
}

// CleanDefaultBranchBaseRef returns the ref that should be used as a clean base
// for default-branch work. In split push-url setups origin still fetches from
// upstream, so origin/<default> is clean. When origin itself is a fork and a
// distinct upstream remote is present, upstream/<default> is the clean base.
func (g *Git) CleanDefaultBranchBaseRef(remote, defaultBranch string) string {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	fetchURL, fetchErr := g.RemoteURL(remote)
	upstreamURL, upstreamErr := g.GetUpstreamURL()
	if fetchErr == nil && upstreamErr == nil && upstreamURL != "" && !sameGitRemoteURL(fetchURL, upstreamURL) {
		return "upstream/" + defaultBranch
	}
	return remote + "/" + defaultBranch
}

// CleanBaseRef returns a fully qualified base ref for a target branch. Explicit
// origin/ or upstream/ refs are preserved; default-branch targets use the clean
// fork-aware base.
func (g *Git) CleanBaseRef(remote, defaultBranch, target string) string {
	target = strings.TrimSpace(target)
	if target == "" || target == defaultBranch {
		return g.CleanDefaultBranchBaseRef(remote, defaultBranch)
	}
	if strings.HasPrefix(target, "origin/") || strings.HasPrefix(target, "upstream/") {
		return target
	}
	return remote + "/" + target
}

// RemoteForRef returns the remote prefix from refs like origin/main or
// upstream/main. It returns an empty string for local branch names.
func RemoteForRef(ref string) string {
	remote, _, ok := strings.Cut(strings.TrimSpace(ref), "/")
	if !ok || (remote != "origin" && remote != "upstream") {
		return ""
	}
	return remote
}

// RefuseForkBackedDefaultPush fails closed before default-branch pushes in a
// fork/upstream topology. Feature branch pushes to the fork remain allowed.
func (g *Git) RefuseForkBackedDefaultPush(remote, refspec, defaultBranch string) error {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	destination := pushDestinationBranch(refspec)
	if destination != defaultBranch || !g.ForkBackedRemote(remote) {
		return nil
	}
	return fmt.Errorf("refusing direct push to %s/%s: fork/upstream rig detected; push a feature branch and use the Mayor-managed fork PR flow to upstream %s (no refs were pushed)", remote, destination, defaultBranch)
}

func pushDestinationBranch(refspec string) string {
	refspec = strings.TrimSpace(refspec)
	for strings.HasPrefix(refspec, "+") {
		refspec = strings.TrimPrefix(refspec, "+")
	}
	if _, dst, ok := strings.Cut(refspec, ":"); ok {
		refspec = dst
	}
	refspec = strings.TrimPrefix(refspec, "refs/heads/")
	return strings.TrimSpace(refspec)
}

func sameGitRemoteURL(a, b string) bool {
	return normalizeGitRemoteURL(a) == normalizeGitRemoteURL(b)
}

func normalizeGitRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "ssh://")
	s = strings.TrimPrefix(s, "git://")
	if strings.HasPrefix(s, "git@") {
		s = strings.TrimPrefix(s, "git@")
		s = strings.Replace(s, ":", "/", 1)
	} else if at := strings.LastIndex(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	return strings.ToLower(strings.TrimSuffix(s, "/"))
}

// Push pushes to the remote branch with a timeout to prevent indefinite hangs
// when the remote is unreachable.
func (g *Git) Push(remote, branch string, force bool) error {
	if err := g.RefuseForkBackedDefaultPush(remote, branch, g.RemoteDefaultBranch()); err != nil {
		return err
	}
	args := []string{"push", remote, branch}
	if force {
		args = append(args, "--force")
	}
	_, err := g.runWithTimeout(pushTimeout, args...)
	return err
}

// EnvRefineryMerge and EnvDoneDirectMerge are the two allow signals the
// pre-push hook accepts for a deliberate, gate-checked push to the default
// branch made from a polecat's session (gt-ibt8). Every other push to the
// default branch from a polecat context is refused by that hook, so a landing
// path that runs inside a polecat session (a direct-merge convoy's `gt done`,
// or a Refinery merge) MUST pass one of these to PushWithEnv; a plain Push
// will be refused. As of gt-9tf9, EnvRefineryMerge alone is not enough: the
// hook also requires a Refinery identity signal (GT_REFINERY=1 or
// GT_ROLE=*/refinery) in the same environment, and refuses a polecat-shaped
// GT_ROLE outright regardless of that signal - so a caller running outside an
// actual Refinery session (a manual `gt mq run`, a non-session engineer path)
// will be refused even with EnvRefineryMerge set.
const (
	// EnvRefineryMerge marks the Refinery's own merge onto the default branch
	// (internal/refinery/batch.go, internal/refinery/engineer.go). Requires a
	// Refinery identity signal alongside it (gt-9tf9); see the doc comment above.
	EnvRefineryMerge = "GT_REFINERY_MERGE=1"
	// EnvDoneDirectMerge marks `gt done` landing a convoy whose merge_strategy
	// is "direct" (internal/cmd/done.go).
	EnvDoneDirectMerge = "GT_DONE_DIRECT_MERGE=1"
)

// PushWithEnv pushes with additional environment variables.
// Used by gt mq integration land to set GT_INTEGRATION_LAND=1, which the
// pre-push hook checks to allow integration branch content landing on main,
// and by the landing paths named on EnvRefineryMerge/EnvDoneDirectMerge above.
func (g *Git) PushWithEnv(remote, branch string, force bool, env []string) error {
	if err := g.RefuseForkBackedDefaultPush(remote, branch, g.RemoteDefaultBranch()); err != nil {
		return err
	}
	args := []string{"push", remote, branch}
	if force {
		args = append(args, "--force")
	}
	_, err := g.runWithEnvAndTimeout(args, env, pushTimeout)
	return err
}

// PushForceWithLease pushes refspec to remote, but only if remote's current
// value for branchRef is still expectedSHA (git push remote refspec
// --force-with-lease=branchRef:expectedSHA). Unlike a blind --force, this
// aborts instead of clobbering if something else moved the ref after
// expectedSHA was observed — the caller passes the origin SHA it just
// fetched, so this only overwrites the exact state it inspected.
func (g *Git) PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error {
	if err := g.RefuseForkBackedDefaultPush(remote, refspec, g.RemoteDefaultBranch()); err != nil {
		return err
	}
	args := []string{"push", remote, refspec, fmt.Sprintf("--force-with-lease=%s:%s", branchRef, expectedSHA)}
	_, err := g.runWithTimeout(pushTimeout, args...)
	return err
}

// ErrNoNote is returned by NotesShow when commit has no note under ref.
var ErrNoNote = errors.New("no note")

// MergeBase returns the best common ancestor of a and b (git merge-base a b),
// trimmed of trailing whitespace.
func (g *Git) MergeBase(a, b string) (string, error) {
	out, err := g.run("merge-base", a, b)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// FirstParentLog returns the first-parent commits in base..head, oldest
// first (git rev-list --first-parent --reverse base..head). When head was
// built by merging N branches onto base one at a time (git merge --no-ff),
// this returns exactly the N merge commits in merge order — used by the
// editorial push precondition to find the actual commit that lands for
// each stacked MR, as opposed to that MR's submitted branch tip.
func (g *Git) FirstParentLog(base, head string) ([]string, error) {
	out, err := g.run("rev-list", "--first-parent", "--reverse", base+".."+head)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSpace(out), "\n"), nil
}

// PatchID returns the stable patch-id of the diff between base and head
// (git diff base..head | git patch-id --stable), i.e. the first field of the
// tool's output. Unlike a commit sha, the patch-id is unchanged by a rebase
// that leaves the diff content identical, and changes whenever the content
// does — used to detect whether a reviewed range still matches its target.
func (g *Git) PatchID(base, head string) (string, error) {
	diff, err := g.run("diff", base+".."+head)
	if err != nil {
		return "", err
	}
	out, err := g.runWithStdin(diff, "patch-id", "--stable")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("git patch-id produced no output for range %s..%s", base, head)
	}
	return fields[0], nil
}

// PatchIDs returns the stable patch-id of each non-merge commit in base..head
// (git log --no-merges -p base..head | git patch-id --stable), one entry per
// commit, in git log's order (newest first).
//
// PatchID collapses a whole range into a single id; this keeps the commits
// separate so a caller can ask whether every change one branch carries is also
// present in another — a rebase preserves each commit's patch-id even though
// it rewrites every sha, and a branch that adds commits on top of another's
// keeps the ones it inherited. That question is what distinguishes "same work,
// plus new commits" from "someone else's work this branch does not have".
//
// An empty range yields an empty slice rather than an error: "this branch has
// no commits of its own" is a state callers need to reason about, unlike a
// diff that failed to compute.
func (g *Git) PatchIDs(base, head string) ([]string, error) {
	log, err := g.run("log", "--no-merges", "-p", "--no-color", base+".."+head)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(log) == "" {
		return nil, nil
	}
	out, err := g.runWithStdin(log, "patch-id", "--stable")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		// Each line is "<patch-id> <commit-id>"; only the patch-id matters here.
		if fields := strings.Fields(line); len(fields) > 0 {
			ids = append(ids, fields[0])
		}
	}
	return ids, nil
}

// PatchIDCommit pairs a commit with the patch-id of its own diff (against its
// first parent).
type PatchIDCommit struct {
	PatchID string
	Commit  string
}

// FirstParentPatchIDs returns each first-parent commit in base..head paired
// with the stable patch-id of its own diff against its first parent, newest
// first (git log --first-parent -p base..head | git patch-id --stable).
//
// Two differences from PatchIDs, both deliberate. Merge commits are kept:
// --first-parent makes git log print each one's diff against its first
// parent, so a landing merge commit's patch-id is the whole branch's
// cumulative diff — the value a reviewed range's note is keyed to
// (PatchID(base, head) over that range). And the walk follows only the
// first-parent chain, which is the sequence of landings on the target.
// Together they answer "which commit on this branch carries the content of
// that patch?", a question per-commit branch comparison (PatchIDs) cannot:
// a rebase or cherry-pick rewrites every sha, so the commit that landed a
// given reviewed range is identifiable only by patch-id (gt-9t0p).
//
// An empty range yields an empty slice, as in PatchIDs. Commits whose diff
// is empty (an empty commit) produce no patch-id line and are omitted.
func (g *Git) FirstParentPatchIDs(base, head string) ([]PatchIDCommit, error) {
	log, err := g.run("log", "--first-parent", "-p", "--no-color", base+".."+head)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(log) == "" {
		return nil, nil
	}
	out, err := g.runWithStdin(log, "patch-id", "--stable")
	if err != nil {
		return nil, err
	}
	var pairs []PatchIDCommit
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		// Each line is "<patch-id> <commit-id>", in git log's order.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pairs = append(pairs, PatchIDCommit{PatchID: fields[0], Commit: fields[1]})
	}
	return pairs, nil
}

// NotesAdd attaches content as a note on commit under the given notes ref,
// overwriting any note already there (git notes --ref <ref> add -f -m).
func (g *Git) NotesAdd(ref, commit, content string) error {
	_, err := g.run("notes", "--ref", ref, "add", "-f", "-m", content, commit)
	return err
}

// NotesShow returns the note content on commit under ref, or ErrNoNote if
// commit has no note there.
func (g *Git) NotesShow(ref, commit string) (string, error) {
	out, err := g.run("notes", "--ref", ref, "show", commit)
	if err != nil {
		var ge *GitError
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "no note found") {
			return "", ErrNoNote
		}
		return "", err
	}
	return out, nil
}

// NotesCopy copies the note on from to on under ref, overwriting any note
// already on to (git notes --ref <ref> copy -f).
func (g *Git) NotesCopy(ref, from, to string) error {
	_, err := g.run("notes", "--ref", ref, "copy", "-f", from, to)
	return err
}

// NoteEntry pairs a note's annotated object with its content under a notes ref.
type NoteEntry struct {
	// Annotated is the object (usually a commit) the note is attached to.
	Annotated string
	// Content is the note's content.
	Content string
}

// NotesList returns every note in ref as (annotated object, content) pairs.
//
// Unlike reading a note on a known sha, this finds notes whose annotated
// object is not reachable from any branch — a review can be keyed to a
// rehearsal commit that was later discarded, and that note is exactly what a
// backfill needs to find. So it reads the notes ref directly (git notes
// --ref <ref> list) instead of walking history.
//
// An absent notes ref is not an error: it yields an empty list, so callers
// treat "no notes at all" and "no matching note" the same way.
func (g *Git) NotesList(ref string) ([]NoteEntry, error) {
	out, err := g.run("notes", "--ref", ref, "list")
	if err != nil {
		return nil, err
	}
	entries := make([]NoteEntry, 0, len(out)/48)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		annotated := fields[1]
		content, err := g.NotesShow(ref, annotated)
		if err != nil {
			if errors.Is(err, ErrNoNote) {
				continue
			}
			return nil, fmt.Errorf("read note on %s: %w", annotated, err)
		}
		entries = append(entries, NoteEntry{Annotated: annotated, Content: content})
	}
	return entries, nil
}

// ErrNotesPushConflict is returned by PushNotes when the push was rejected
// non-fast-forward and merging the remote's notes ref into the local one
// conflicts — both writers re-keyed the same annotated commit, which no
// automatic resolution can decide between.
var ErrNotesPushConflict = errors.New("notes push rejected and remote notes conflict")

// PushNotes pushes the notes ref to remote (git push <remote> refs/notes/<ref>),
// with the same hang-prevention timeout as Push.
//
// A notes ref is an append-only tree keyed per annotated commit, so writers
// that each add notes for different commits never logically conflict — but git
// rejects the second push non-fast-forward, because the ref itself moved. With
// several writers (batch rekey/backfill, per-rig refineries, concurrent
// `gt mq review` runs) that is routine rather than exceptional, and it used to
// surface as a bare push failure: the note is written locally, never published,
// and the caller records record_failed for a race it could have resolved
// (gt-2rcx). So a non-fast-forward rejection is retried once after merging the
// remote ref in. The merge is clean whenever the two sides touch different
// commits; if they re-key the same commit the merge is aborted, the local ref
// restored, and ErrNotesPushConflict returned — fail-closed, because silently
// picking a winner would publish one verdict over another.
//
// Other push failures (auth, protected ref, unreachable remote) are returned
// unchanged: retrying them would only repeat the failure.
func (g *Git) PushNotes(remote, ref string) error {
	_, err := g.runWithTimeout(pushTimeout, "push", remote, "refs/notes/"+ref)
	if err == nil || !isNonFastForwardPush(err) {
		return err
	}
	if mergeErr := g.mergeRemoteNotes(remote, ref); mergeErr != nil {
		return fmt.Errorf("%w: %s refs/notes/%s: %v", ErrNotesPushConflict, remote, ref, mergeErr)
	}
	if _, err = g.runWithTimeout(pushTimeout, "push", remote, "refs/notes/"+ref); err != nil {
		// Another writer won again between the merge and this push. Say so,
		// or the retry looks like the rejection it was meant to resolve.
		return fmt.Errorf("push refs/notes/%s after merging %s's notes: %w", ref, remote, err)
	}
	return nil
}

// isNonFastForwardPush reports whether err is git's non-fast-forward
// rejection. Both spellings are matched because git words the rejection
// differently depending on whether it knows the remote ref's history
// ("non-fast-forward") or only that it moved ("fetch first").
func isNonFastForwardPush(err error) bool {
	var ge *GitError
	if !errors.As(err, &ge) {
		return false
	}
	return strings.Contains(ge.Stderr, "non-fast-forward") ||
		strings.Contains(ge.Stderr, "fetch first")
}

// scratchNotesRef is the ref remote notes are fetched into before a push
// retry. It is deliberately not refs/notes/<ref>: FetchNotes force-updates
// that ref, which would discard the local note this push exists to publish.
func scratchNotesRef(ref string) string {
	return "refs/notes/" + ref + "-push-scratch"
}

// mergeRemoteNotes fetches remote's notes ref into a scratch ref and merges it
// into the local one, so a push rejected non-fast-forward can be retried with
// both writers' notes present. The scratch ref is deleted on the way out.
func (g *Git) mergeRemoteNotes(remote, ref string) error {
	scratch := scratchNotesRef(ref)
	if _, err := g.runWithTimeout(notesFetchTimeout, "fetch", remote, "+refs/notes/"+ref+":"+scratch); err != nil {
		var ge *GitError
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref") {
			// The ref the rejected push raced against is gone (deleted, or
			// just recreated) — there is nothing to merge, and the retry
			// push will be judged on its own.
			return nil
		}
		return fmt.Errorf("fetch %s %s: %w", remote, scratch, err)
	}
	// Best effort: the scratch ref is private to this retry and carries
	// nothing a later reader needs.
	defer func() {
		_, _ = g.run("update-ref", "-d", scratch)
	}()

	// -s manual: a conflict means both sides re-keyed the same commit, and
	// manual is the only strategy that refuses to guess. git then leaves the
	// merge in progress, so abort to restore the pre-merge notes ref.
	if _, err := g.run("notes", "--ref", ref, "merge", "-s", "manual", scratch); err != nil {
		if _, abortErr := g.run("notes", "--ref", ref, "merge", "--abort"); abortErr != nil {
			return fmt.Errorf("merge %s: %w (abort failed, refs/notes/%s may need manual repair: %v)", scratch, err, ref, abortErr)
		}
		return fmt.Errorf("merge %s: %w", scratch, err)
	}
	return nil
}

// ErrNoRemoteNotes is returned by FetchNotes when remote has no notes under
// ref yet, distinct from a real fetch failure (network, auth, missing
// remote).
var ErrNoRemoteNotes = errors.New("no notes on remote")

// FetchNotes fetches the notes ref from remote into refs/notes/<ref> locally
// (git fetch <remote> +refs/notes/<ref>:refs/notes/<ref>), overwriting any
// local notes ref so a stale local copy never masks the remote's current
// state. Returns ErrNoRemoteNotes if the remote has no notes under ref yet —
// callers should fall back to whatever local ref they may already have.
func (g *Git) FetchNotes(remote, ref string) error {
	refspec := "+refs/notes/" + ref + ":refs/notes/" + ref
	_, err := g.run("fetch", remote, refspec)
	if err != nil {
		var ge *GitError
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref") {
			return ErrNoRemoteNotes
		}
		return err
	}
	return nil
}

// Add stages files for commit.
func (g *Git) Add(paths ...string) error {
	args := append([]string{"add"}, paths...)
	_, err := g.run(args...)
	return err
}

// Commit creates a commit with the given message.
func (g *Git) Commit(message string) error {
	_, err := g.run("commit", "-m", message)
	return err
}

// CommitAll stages all changes and commits.
func (g *Git) CommitAll(message string) error {
	_, err := g.run("commit", "-am", message)
	return err
}

// ResetFiles unstages files without modifying the working tree.
// Equivalent to: git reset HEAD -- <paths>
func (g *Git) ResetFiles(paths ...string) error {
	args := append([]string{"reset", "HEAD", "--"}, paths...)
	_, err := g.run(args...)
	return err
}

// StagedDeletions returns the list of tracked files staged for deletion.
// Used by auto-save to unstage deletions — safety nets should preserve work, not destroy it.
func (g *Git) StagedDeletions() ([]string, error) {
	out, err := g.run("diff", "--cached", "--name-only", "--diff-filter=D")
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

// ShowFile returns the contents of a file at a given ref (e.g., "origin/main:CLAUDE.md").
// Returns empty string and no error if the file does not exist at that ref.
func (g *Git) ShowFile(ref, path string) (string, error) {
	out, err := g.run("show", ref+":"+path)
	if err != nil {
		// "does not exist" or "exists on disk, but not in" are expected for missing files
		return "", err
	}
	return out, nil
}

// CheckoutFileFromRef restores a file from a given ref (e.g., "origin/main").
// Equivalent to: git checkout <ref> -- <path>
func (g *Git) CheckoutFileFromRef(ref string, paths ...string) error {
	args := append([]string{"checkout", ref, "--"}, paths...)
	_, err := g.run(args...)
	return err
}

// RmCached removes files from the index without deleting from the working tree.
// Equivalent to: git rm --cached --force <paths>
func (g *Git) RmCached(paths ...string) error {
	args := append([]string{"rm", "--cached", "--force", "--ignore-unmatch"}, paths...)
	_, err := g.run(args...)
	return err
}

// DiffNameOnly returns filenames changed between two refs.
// Equivalent to: git diff --name-only <base>...<head>
func (g *Git) DiffNameOnly(base, head string) ([]string, error) {
	out, err := g.run("diff", "--name-only", base+"..."+head)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSpace(out), "\n"), nil
}

// GitStatus represents the status of the working directory.
type GitStatus struct {
	Clean     bool
	Modified  []string
	Added     []string
	Deleted   []string
	Untracked []string
	Unmerged  []string
	// StagedOnly lists paths whose index differs from HEAD but whose working
	// tree matches the index exactly (porcelain worktree column is clean).
	// These are index-skew candidates (see CheckUncommittedWork): content
	// staged in the index with nothing further pending in the editor.
	// Renames/copies and untracked/unmerged entries are never included.
	StagedOnly []string
}

type porcelainStatusEntry struct {
	Code       string
	Path       string
	SourcePath string
	Unmerged   bool
}

// Status returns the current git status.
func (g *Git) Status() (*GitStatus, error) {
	return g.status("--porcelain", "-uall")
}

// StatusIgnoringSubmodules is Status() with submodule entries dropped entirely
// from every column.
//
// A superproject reports a submodule as modified whenever the commit checked
// out inside it differs from the gitlink it records — which is how a submodule
// looks for most of its life, including after any merge that moves its pointer.
// A caller asking "has anyone changed this working tree?" wants the trees it
// reads, not that bookkeeping.
func (g *Git) StatusIgnoringSubmodules() (*GitStatus, error) {
	return g.status("--porcelain", "-uall", "--ignore-submodules=all")
}

func (g *Git) status(args ...string) (*GitStatus, error) {
	// Raw output, not run()'s trimmed contract: the porcelain format's first
	// column can be a meaningful leading space (see runOutput's doc comment),
	// and TrimSpace on the whole blob corrupts the first line when it starts
	// with one.
	out, err := g.runOutput(append([]string{"status"}, args...)...)
	if err != nil {
		return nil, err
	}

	status := &GitStatus{Clean: true}
	if out == "" {
		return status, nil
	}

	// Get skip-worktree files once (sparse checkout). These appear as 'D' in
	// --porcelain output but are not real deletions — they are hidden by the
	// sparse-checkout cone. Filtering them prevents gt done from blocking on
	// 897+ phantom deletions in polecat sparse worktrees.
	skipWorktree := g.skipWorktreeFiles()

	status.Clean = false
	for _, line := range strings.Split(out, "\n") {
		entry, ok := parsePorcelainStatusEntry(line)
		if !ok {
			continue
		}
		code := entry.Code
		file := entry.Path

		var appended []string
		switch {
		case entry.Unmerged:
			status.Unmerged = append(status.Unmerged, entry.paths()...)
		case strings.Contains(code, "?"):
			status.Untracked = append(status.Untracked, file)
		case strings.ContainsAny(code, "RC"):
			status.Modified = append(status.Modified, entry.paths()...)
			appended = entry.paths()
		case strings.Contains(code, "M"):
			status.Modified = append(status.Modified, file)
			appended = []string{file}
		case strings.Contains(code, "A"):
			status.Added = append(status.Added, file)
			appended = []string{file}
		case strings.Contains(code, "D"):
			// Skip files hidden by sparse-checkout (skip-worktree bit set).
			if !skipWorktree[file] {
				status.Deleted = append(status.Deleted, file)
				appended = []string{file}
			}
		default:
			// Unknown porcelain statuses still represent local work. Returning the
			// path is safer than letting cleanup/recovery treat the worktree as clean.
			status.Modified = append(status.Modified, file)
			appended = []string{file}
		}

		if len(appended) > 0 && isStagedOnlyCode(code) {
			status.StagedOnly = append(status.StagedOnly, appended...)
		}
	}

	// Recheck clean: if all entries were skip-worktree deletions, we're actually clean.
	if len(status.Modified) == 0 && len(status.Added) == 0 &&
		len(status.Deleted) == 0 && len(status.Untracked) == 0 && len(status.Unmerged) == 0 {
		status.Clean = true
	}

	return status, nil
}

// isStagedOnlyCode reports whether a 2-character porcelain status code is an
// index-skew candidate: something is staged (index column differs from HEAD)
// but the working tree exactly matches the index (worktree column is clean).
// Renames/copies, untracked ("?") and unmerged ("U") codes are excluded to
// keep the classifier conservative — see GitStatus.StagedOnly.
func isStagedOnlyCode(code string) bool {
	if len(code) != 2 {
		return false
	}
	if strings.ContainsAny(code, "RC?U") {
		return false
	}
	return code[0] != ' ' && code[1] == ' '
}

func parsePorcelainStatusEntry(line string) (porcelainStatusEntry, bool) {
	if len(line) < 3 {
		return porcelainStatusEntry{}, false
	}

	entry := porcelainStatusEntry{
		Code:     line[:2],
		Path:     line[3:],
		Unmerged: isUnmergedPorcelainStatus(line[:2]),
	}
	if strings.ContainsAny(entry.Code, "RC") {
		entry.SourcePath, entry.Path = porcelainRenameCopyPaths(entry.Path)
		entry.SourcePath = unquoteGitPath(entry.SourcePath)
	}
	entry.Path = unquoteGitPath(entry.Path)
	return entry, true
}

// unquoteGitPath reverses git's C-style quoting of a porcelain path. Git
// wraps a path in double quotes and escapes it (backslash, double-quote,
// control characters as \a \b \f \n \r \t \v, and — unless
// core.quotepath=false — any non-ASCII byte as \NNN octal) whenever it
// contains a character quotepath decides is unsafe to print raw. Left
// quoted, the string is not the real filename: passing it back to git as a
// pathspec (as classifyIndexSkew does) looks for a literal file whose name
// contains quote and backslash characters, which matches nothing (gt-ui2x).
// Go's octal/backslash escape set is the same as git's, so strconv.Unquote
// round-trips it; a string that fails to unquote is returned unchanged
// rather than dropped, since the paths list must stay 1:1 with git's output.
func unquoteGitPath(path string) string {
	if len(path) < 2 || path[0] != '"' || path[len(path)-1] != '"' {
		return path
	}
	unquoted, err := strconv.Unquote(path)
	if err != nil {
		return path
	}
	return unquoted
}

func (e porcelainStatusEntry) paths() []string {
	if e.SourcePath == "" || e.SourcePath == e.Path {
		return []string{e.Path}
	}
	return []string{e.SourcePath, e.Path}
}

func porcelainRenameCopyPaths(path string) (string, string) {
	if idx := strings.LastIndex(path, " -> "); idx >= 0 {
		return path[:idx], path[idx+4:]
	}
	return "", path
}

func isUnmergedPorcelainStatus(code string) bool {
	switch code {
	case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
		return true
	default:
		return strings.Contains(code, "U")
	}
}

// skipWorktreeFiles returns a set of file paths that have the skip-worktree
// bit set (sparse-checkout hidden files). Uses `git ls-files -v` and filters
// for lines starting with 'S' (uppercase = skip-worktree). Non-fatal: returns
// empty map on error so callers degrade gracefully.
func (g *Git) skipWorktreeFiles() map[string]bool {
	out, err := g.run("ls-files", "-v")
	if err != nil || out == "" {
		return nil
	}
	result := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		// Format: "<flag> <path>" where flag is uppercase letter for skip-worktree
		if len(line) < 3 || line[0] != 'S' {
			continue
		}
		result[line[2:]] = true
	}
	return result
}

// CurrentBranch returns the current branch name.
func (g *Git) CurrentBranch() (string, error) {
	return g.run("rev-parse", "--abbrev-ref", "HEAD")
}

// DefaultBranch returns the default branch name (what HEAD points to).
// This works for both regular and bare repositories.
// Returns "main" as fallback if detection fails.
func (g *Git) DefaultBranch() string {
	// Try symbolic-ref first (works for bare repos)
	branch, err := g.run("symbolic-ref", "--short", "HEAD")
	if err == nil && branch != "" {
		return branch
	}
	// Fallback to main
	return "main"
}

// RemoteDefaultBranch returns the default branch from the remote (origin).
// This is useful in worktrees where HEAD may not reflect the repo's actual default.
// Checks origin/HEAD first, then falls back to checking if master/main exists.
// Returns "main" as final fallback.
func (g *Git) RemoteDefaultBranch() string {
	// Try to get from origin/HEAD symbolic ref
	out, err := g.run("symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil && out != "" {
		// Returns refs/remotes/origin/main -> extract branch name
		parts := strings.Split(out, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}

	// Fallback: check if origin/master exists
	_, err = g.run("rev-parse", "--verify", "origin/master")
	if err == nil {
		return "master"
	}

	// Fallback: check if origin/main exists
	_, err = g.run("rev-parse", "--verify", "origin/main")
	if err == nil {
		return "main"
	}

	return "main" // final fallback
}

// HasUncommittedChanges returns true if there are uncommitted changes.
func (g *Git) HasUncommittedChanges() (bool, error) {
	status, err := g.Status()
	if err != nil {
		return false, err
	}
	return !status.Clean, nil
}

// RemoteURL returns the URL for the given remote.
func (g *Git) RemoteURL(remote string) (string, error) {
	return g.run("remote", "get-url", remote)
}

// AddRemote adds a new remote with the given name and URL.
func (g *Git) AddRemote(name, url string) (string, error) {
	return g.run("remote", "add", name, url)
}

// SetRemoteURL updates the URL for an existing remote.
func (g *Git) SetRemoteURL(name, url string) (string, error) {
	return g.run("remote", "set-url", name, url)
}

// AddUpstreamRemote adds or updates the 'upstream' git remote.
// This is idempotent - if the remote already exists with the same URL, it's a no-op.
// If the remote exists with a different URL, it's updated.
func (g *Git) AddUpstreamRemote(upstreamURL string) error {
	has, err := g.HasUpstreamRemote()
	if err != nil {
		return err
	}
	if has {
		current, err := g.GetUpstreamURL()
		if err != nil {
			return err
		}
		if current == upstreamURL {
			return nil
		}
		_, err = g.run("remote", "set-url", "upstream", upstreamURL)
		return err
	}
	_, err = g.run("remote", "add", "upstream", upstreamURL)
	return err
}

// GetUpstreamURL returns the URL of the upstream remote.
// Returns empty string if upstream remote doesn't exist.
func (g *Git) GetUpstreamURL() (string, error) {
	out, err := g.run("remote", "get-url", "upstream")
	if err != nil {
		if strings.Contains(err.Error(), "No such remote") {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// HasUpstreamRemote returns true if an upstream remote is configured.
func (g *Git) HasUpstreamRemote() (bool, error) {
	_, err := g.run("remote", "get-url", "upstream")
	if err != nil {
		if strings.Contains(err.Error(), "No such remote") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// FetchUpstream fetches from the upstream remote.
func (g *Git) FetchUpstream() error {
	_, err := g.run("fetch", "upstream")
	return err
}

// Remotes returns the list of configured remote names.
func (g *Git) Remotes() ([]string, error) {
	out, err := g.run("remote")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// ConfigGet returns the value of a git config key.
// Returns empty string if the key is not set.
func (g *Git) ConfigGet(key string) (string, error) {
	out, err := g.run("config", "--get", key)
	if err != nil {
		// git config --get returns exit code 1 if key not found
		return "", nil
	}
	return out, nil
}

// Merge merges the given branch into the current branch.
func (g *Git) Merge(branch string) error {
	_, err := g.run("merge", branch)
	return err
}

// MergeNoFF merges the given branch with --no-ff flag and a custom message.
func (g *Git) MergeNoFF(branch, message string) error {
	_, err := g.run("merge", "--no-ff", "-m", message, branch)
	return err
}

// MergeFFOnly performs a fast-forward-only merge of the given ref into the current branch.
// This ensures what you tested is exactly what lands — no merge commits are created.
// Returns an error if the merge cannot be performed as a fast-forward.
func (g *Git) MergeFFOnly(ref string) error {
	_, err := g.run("merge", "--ff-only", ref)
	return err
}

// MergeSquash performs a squash merge of the given branch and commits with the provided message.
// This stages all changes from the branch without creating a merge commit, then commits them
// as a single commit with the given message. This eliminates redundant merge commits while
// preserving the original commit message from the source branch.
func (g *Git) MergeSquash(branch, message string) error {
	// Stage all changes from the branch without committing
	if _, err := g.run("merge", "--squash", branch); err != nil {
		return err
	}
	// Commit the staged changes with the provided message
	_, err := g.run("commit", "-m", message)
	return err
}

// GetBranchCommitMessage returns the commit message of the HEAD commit on the given branch.
// This is useful for preserving the original conventional commit message (feat:/fix:) when
// performing squash merges.
func (g *Git) GetBranchCommitMessage(branch string) (string, error) {
	return g.run("log", "-1", "--format=%B", branch)
}

// RecentCommits returns the last n commits as one-line summaries (hash + subject).
// Returns empty string if there are no commits or the repo is empty.
func (g *Git) RecentCommits(n int) (string, error) {
	return g.run("log", "--oneline", fmt.Sprintf("-%d", n))
}

// DeleteRemoteBranch deletes a branch on the remote.
func (g *Git) DeleteRemoteBranch(remote, branch string) error {
	_, err := g.runWithTimeout(pushTimeout, "push", remote, "--delete", branch)
	return err
}

// DeleteRemoteBranchIfAt deletes a remote branch only if it still points at expectedHash.
func (g *Git) DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error {
	ref := "refs/heads/" + branch
	_, err := g.runWithTimeout(pushTimeout, "push", "--force-with-lease="+ref+":"+expectedHash, remote, ":"+ref)
	return err
}

// HasOpenPR checks whether the given branch has an open pull request on GitHub.
// Errors and ambiguous branch lookups protect the branch from deletion.
func (g *Git) HasOpenPR(branch string) bool {
	return g.HasOpenPullRequest(PullRequestRef{Branch: branch})
}

// FindPRNumber returns the GitHub PR number for the given branch, or 0 if none exists.
func (g *Git) FindPRNumber(branch string) (int, error) {
	return g.FindPRNumberForRef(PullRequestRef{Branch: branch})
}

// FindPRNumberForRef returns an open GitHub PR number using recorded PR identity
// before falling back to an unambiguous target-repo branch lookup.
func (g *Git) FindPRNumberForRef(ref PullRequestRef) (int, error) {
	pr, err := g.LookupPullRequest(ref)
	if err != nil {
		if errors.Is(err, ErrPullRequestNotFound) {
			return 0, nil
		}
		return 0, err
	}
	if !pr.Open() {
		return 0, nil
	}
	return pr.Number, nil
}

// IsPRApproved checks whether a GitHub PR has at least one approving review.
// Returns true if approved, false if not (or on error).
func (g *Git) IsPRApproved(prNumber int) (bool, error) {
	return g.IsPullRequestApproved(&PullRequestInfo{Number: prNumber})
}

// IsPullRequestApproved checks whether a resolved GitHub PR has at least one approving review.
func (g *Git) IsPullRequestApproved(pr *PullRequestInfo) (bool, error) {
	if pr == nil || (pr.Number == 0 && pr.URL == "") {
		return false, fmt.Errorf("pull request identity is missing")
	}
	// Use gh pr view which includes review decision
	args := []string{"pr", "view", pullRequestSelector(pr), "--json", "reviewDecision"}
	if pr.BaseRepo != "" {
		args = append(args, "--repo", pr.BaseRepo)
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = g.workDir
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("gh pr view failed: %w", err)
	}
	var result struct {
		ReviewDecision string `json:"reviewDecision"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &result); err != nil {
		return false, fmt.Errorf("failed to parse gh pr view output: %w", err)
	}
	// APPROVED is the GitHub review decision when at least one approving review exists
	return result.ReviewDecision == "APPROVED", nil
}

// GhPrMerge merges a GitHub PR using the gh CLI, respecting branch protection rules.
// The method parameter should be "merge", "squash", or "rebase".
// Returns the merge commit SHA on success.
func (g *Git) GhPrMerge(prNumber int, method string) (string, error) {
	return g.GhPrMergePullRequest(&PullRequestInfo{Number: prNumber}, method)
}

// GhPrMergePullRequest merges a resolved GitHub PR using its URL when available.
func (g *Git) GhPrMergePullRequest(pr *PullRequestInfo, method string) (string, error) {
	if pr == nil || (pr.Number == 0 && pr.URL == "") {
		return "", fmt.Errorf("pull request identity is missing")
	}
	args := []string{"pr", "merge", pullRequestSelector(pr), "--" + method}
	if head := strings.TrimSpace(pr.HeadSHA); head != "" {
		args = append(args, "--match-head-commit", head)
	}
	if pr.BaseRepo != "" {
		args = append(args, "--repo", pr.BaseRepo)
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = g.workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr merge failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// After merge, pull the target branch to get the merge commit locally
	if _, pullErr := g.run("pull", "origin"); pullErr != nil {
		// Non-fatal: the merge succeeded on GitHub, we just can't get the SHA locally
		return "", nil
	}
	// Get the latest commit on HEAD (should be the merge commit)
	sha, revErr := g.Rev("HEAD")
	if revErr != nil {
		return "", nil // Merge succeeded, just can't determine SHA
	}
	return sha, nil
}

func pullRequestSelector(pr *PullRequestInfo) string {
	if pr != nil && pr.URL != "" {
		return pr.URL
	}
	if pr != nil {
		return fmt.Sprintf("%d", pr.Number)
	}
	return ""
}

// FindBitbucketPullRequest returns the open Bitbucket PR for branch.
// It includes the source commit hash so refinery can merge only the submitted head.
func (g *Git) FindBitbucketPullRequest(workspace, repoSlug, branch, headSHA string) (*PullRequestInfo, error) {
	// Use curl since there is no official Bitbucket CLI equivalent to gh.
	// The BITBUCKET_TOKEN env var provides authentication.
	token := os.Getenv("BITBUCKET_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("BITBUCKET_TOKEN is required for Bitbucket PR operations")
	}
	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s/pullrequests?q=source.branch.name%%3D%%22%s%%22+AND+state%%3D%%22OPEN%%22&pagelen=1",
		workspace, repoSlug, branch)
	cmd := exec.Command("curl", "-s", "-H", "Authorization: Bearer "+token, url)
	cmd.Dir = g.workDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bitbucket API request failed: %w", err)
	}
	var resp struct {
		Values []struct {
			ID    int    `json:"id"`
			State string `json:"state"`
			Links struct {
				HTML struct {
					Href string `json:"href"`
				} `json:"html"`
			} `json:"links"`
			Source struct {
				Branch struct {
					Name string `json:"name"`
				} `json:"branch"`
				Commit struct {
					Hash string `json:"hash"`
				} `json:"commit"`
			} `json:"source"`
		} `json:"values"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		return nil, fmt.Errorf("failed to parse Bitbucket response: %w", err)
	}
	if len(resp.Values) == 0 {
		return nil, nil
	}
	pr := resp.Values[0]
	info := &PullRequestInfo{
		Number:       pr.ID,
		URL:          pr.Links.HTML.Href,
		State:        strings.ToUpper(pr.State),
		HeadRefName:  pr.Source.Branch.Name,
		HeadSHA:      strings.TrimSpace(pr.Source.Commit.Hash),
		LookupSource: "bitbucket-head",
	}
	if info.State == "" {
		info.State = "OPEN"
	}
	if err := validatePullRequestHead(info, headSHA); err != nil {
		return nil, err
	}
	return info, nil
}

// IsBitbucketPRApproved checks whether a Bitbucket PR has at least one approving reviewer.
func (g *Git) IsBitbucketPRApproved(workspace, repoSlug string, prID int) (bool, error) {
	token := os.Getenv("BITBUCKET_TOKEN")
	if token == "" {
		return false, fmt.Errorf("BITBUCKET_TOKEN is required for Bitbucket PR operations")
	}
	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s/pullrequests/%d",
		workspace, repoSlug, prID)
	cmd := exec.Command("curl", "-s", "-H", "Authorization: Bearer "+token, url)
	cmd.Dir = g.workDir
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("bitbucket API request failed: %w", err)
	}
	var pr struct {
		Participants []struct {
			Role     string `json:"role"`
			Approved bool   `json:"approved"`
		} `json:"participants"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &pr); err != nil {
		return false, fmt.Errorf("failed to parse Bitbucket response: %w", err)
	}
	for _, p := range pr.Participants {
		if p.Role == "REVIEWER" && p.Approved {
			return true, nil
		}
	}
	return false, nil
}

// BitbucketPRMerge merges a Bitbucket PR via the REST API.
// The strategy parameter should be "merge_commit", "squash", or "fast_forward".
// Returns the merge commit SHA on success (if available).
func (g *Git) BitbucketPRMerge(workspace, repoSlug string, prID int, strategy string) (string, error) {
	token := os.Getenv("BITBUCKET_TOKEN")
	if token == "" {
		return "", fmt.Errorf("BITBUCKET_TOKEN is required for Bitbucket PR operations")
	}
	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s/pullrequests/%d/merge",
		workspace, repoSlug, prID)
	body := fmt.Sprintf(`{"merge_strategy":"%s","close_source_branch":false}`, strategy)
	cmd := exec.Command("curl", "-s", "-X", "POST",
		"-H", "Authorization: Bearer "+token,
		"-H", "Content-Type: application/json",
		"-d", body, url)
	cmd.Dir = g.workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("bitbucket merge failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var resp struct {
		MergeCommit struct {
			Hash string `json:"hash"`
		} `json:"merge_commit"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		// Merge may have succeeded but response parsing failed — pull to get SHA.
		if _, pullErr := g.run("pull", "origin"); pullErr == nil {
			if sha, revErr := g.Rev("HEAD"); revErr == nil {
				return sha, nil
			}
		}
		return "", nil
	}

	// Sync local state after remote merge.
	if _, pullErr := g.run("pull", "origin"); pullErr != nil {
		return resp.MergeCommit.Hash, nil
	}
	if resp.MergeCommit.Hash != "" {
		return resp.MergeCommit.Hash, nil
	}
	sha, _ := g.Rev("HEAD")
	return sha, nil
}

// RemoteRef is a ref observed through ls-remote.
type RemoteRef struct {
	Hash string
	Name string
}

// ListRemoteRefsWithHashes returns remote refs matching a prefix using ls-remote.
// The prefix filters refs (e.g., "refs/heads/polecat/" for all polecat branches).
// Returns full ref names like "refs/heads/polecat/furiosa-abc123".
func (g *Git) ListRemoteRefsWithHashes(remote, prefix string) ([]RemoteRef, error) {
	return g.ListRemoteRefsWithHashesTimeout(remote, prefix, remoteQueryTimeout)
}

// ListRemoteRefsWithHashesTimeout is ListRemoteRefsWithHashes with an explicit
// bound; the command (and any remote helper) is killed when it expires.
func (g *Git) ListRemoteRefsWithHashesTimeout(remote, prefix string, timeout time.Duration) ([]RemoteRef, error) {
	out, err := g.runWithTimeout(timeout, "ls-remote", "--refs", remote, prefix+"*")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var refs []RemoteRef
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// ls-remote output format: <sha>\t<refname>
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			refs = append(refs, RemoteRef{Hash: parts[0], Name: parts[1]})
		}
	}
	return refs, nil
}

// ListRemoteRefs returns remote ref names matching a prefix using ls-remote.
func (g *Git) ListRemoteRefs(remote, prefix string) ([]string, error) {
	refsWithHashes, err := g.ListRemoteRefsWithHashes(remote, prefix)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(refsWithHashes))
	for _, ref := range refsWithHashes {
		refs = append(refs, ref.Name)
	}
	return refs, nil
}

// RemoteHasRefs reports whether a remote has any refs at all. It deliberately
// includes tags so callers can distinguish a truly empty repo from a non-empty
// repo with no branch refs or a broken remote HEAD.
func (g *Git) RemoteHasRefs(remote string) (bool, error) {
	out, err := g.runWithTimeout(remoteQueryTimeout, "ls-remote", "--refs", remote)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// ListPushRemoteRefs lists remote refs from the push URL when it differs from
// the fetch URL. With a fork-based workflow (pushurl configured), branches are
// pushed to the fork but ls-remote reads from the fetch URL (upstream). This
// method queries the push URL so cleanup can find branches that were pushed.
// Falls back to ListRemoteRefs if no custom push URL is configured.
func (g *Git) ListPushRemoteRefs(remote, prefix string) ([]string, error) {
	refsWithHashes, err := g.ListPushRemoteRefsWithHashes(remote, prefix)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(refsWithHashes))
	for _, ref := range refsWithHashes {
		refs = append(refs, ref.Name)
	}
	return refs, nil
}

// ListPushRemoteRefsWithHashes is ListPushRemoteRefs with commit hashes.
func (g *Git) ListPushRemoteRefsWithHashes(remote, prefix string) ([]RemoteRef, error) {
	return g.ListRemoteRefsWithHashes(g.pushTarget(remote), prefix)
}

// Rebase rebases the current branch onto the given ref.
func (g *Git) Rebase(onto string) error {
	_, err := g.run("rebase", onto)
	return err
}

// AbortMerge aborts a merge in progress.
func (g *Git) AbortMerge() error {
	_, err := g.run("merge", "--abort")
	return err
}

// CheckConflicts performs a test merge to check if source can be merged into target
// without conflicts. Returns a list of conflicting files, or empty slice if clean.
// The merge is always aborted after checking - no actual changes are made.
//
// The caller must ensure the working directory is clean before calling this.
// After return, the working directory is restored to the target branch.
// A caller whose HEAD already sits on target uses CheckConflictsAtHead instead.
func (g *Git) CheckConflicts(source, target string) ([]string, error) {
	// Checkout the target branch
	if err := g.Checkout(target); err != nil {
		return nil, fmt.Errorf("checkout target %s: %w", target, err)
	}

	return g.CheckConflictsAtHead(source)
}

// CheckConflictsAtHead performs the test merge of source into the commit HEAD
// already names, so a caller that staged HEAD on the target baseline itself is
// not made to check the branch out again — the checkout it would repeat is one
// git refuses while any other worktree holds the branch (gt-032w).
//
// The caller must ensure the working directory is clean before calling this.
// After return, HEAD and the working directory are restored as they were.
func (g *Git) CheckConflictsAtHead(source string) ([]string, error) {
	// Attempt test merge with --no-commit --no-ff
	// We need to capture both stdout and stderr to detect conflicts
	_, mergeErr := g.runMergeCheck("merge", "--no-commit", "--no-ff", source)

	if mergeErr != nil {
		// ZFC: Use git's porcelain output to detect conflicts instead of parsing stderr.
		// GetConflictingFiles() uses `git diff --diff-filter=U` which is the proper way.
		conflicts, err := g.GetConflictingFiles()
		if err == nil && len(conflicts) > 0 {
			// Abort the test merge (best-effort cleanup)
			_ = g.AbortMerge()
			return conflicts, nil
		}

		// No unmerged files detected - this is some other merge error
		_ = g.AbortMerge()
		return nil, mergeErr
	}

	// Merge succeeded (no conflicts) - abort the test merge
	// Use reset since --abort won't work on successful merge (best-effort cleanup)
	_, _ = g.run("reset", "--hard", "HEAD")
	return nil, nil
}

// runMergeCheck runs a git merge command and returns error info from both stdout and stderr.
// ZFC: Returns GitError with raw output for agent observation.
func (g *Git) runMergeCheck(args ...string) (string, error) {
	if err := g.guardUnsafeTownRootMutation(args); err != nil {
		return "", err
	}

	cmd := exec.Command("git", args...)
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = g.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// ZFC: Return raw output for observation, don't interpret CONFLICT
		return "", g.wrapError(err, stdout.String(), stderr.String(), args)
	}

	return strings.TrimSpace(stdout.String()), nil
}

// GetConflictingFiles returns the list of files with merge conflicts.
// ZFC: Uses git's porcelain output (diff --diff-filter=U) instead of parsing stderr.
// This is the proper way to detect conflicts without violating ZFC.
func (g *Git) GetConflictingFiles() ([]string, error) {
	// git diff --name-only --diff-filter=U shows unmerged files
	out, err := g.run("diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}

	if out == "" {
		return nil, nil
	}

	files := strings.Split(out, "\n")
	// Filter out empty strings
	var result []string
	for _, f := range files {
		if f != "" {
			result = append(result, f)
		}
	}
	return result, nil
}

// AbortRebase aborts a rebase in progress.
func (g *Git) AbortRebase() error {
	_, err := g.run("rebase", "--abort")
	return err
}

// CreateBranch creates a new branch.
func (g *Git) CreateBranch(name string) error {
	_, err := g.run("branch", name)
	return err
}

// CreateBranchFrom creates a new branch from a specific ref.
func (g *Git) CreateBranchFrom(name, ref string) error {
	_, err := g.run("branch", name, ref)
	return err
}

// BranchExists checks if a branch exists locally.
func (g *Git) BranchExists(name string) (bool, error) {
	_, err := g.run("show-ref", "--verify", "--quiet", "refs/heads/"+name)
	if err != nil {
		// Exit code 1 means branch doesn't exist
		if strings.Contains(err.Error(), "exit status 1") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// RefExists checks if a ref exists (works for any ref including origin/<branch>).
// Uses show-ref for fully-qualified refs, falls back to rev-parse for short refs.
func (g *Git) RefExists(ref string) (bool, error) {
	// Fully-qualified refs (refs/...) use show-ref which has a stable exit code contract:
	// exit 0 = exists, exit 1 = missing, exit >1 = error.
	if strings.HasPrefix(ref, "refs/") {
		_, err := g.run("show-ref", "--verify", "--quiet", ref)
		if err != nil {
			if strings.Contains(err.Error(), "exit status 1") {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}

	// Short refs (e.g., origin/main) need rev-parse --verify.
	_, err := g.run("rev-parse", "--verify", ref)
	if err != nil {
		// Only treat "ref missing" as false — propagate other failures
		// (e.g. corrupted repo, permissions, disk I/O).
		var gitErr *GitError
		if errors.As(err, &gitErr) &&
			strings.Contains(gitErr.Stderr, "Needed a single revision") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// IsEmpty returns true if the repository has no refs (an empty/unborn repo).
// This is the case for newly-created repos with no commits.
func (g *Git) IsEmpty() (bool, error) {
	out, err := g.run("show-ref")
	if err != nil {
		// git show-ref exits 1 when there are no refs — that means empty
		if strings.Contains(err.Error(), "exit status 1") {
			return true, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// RemoteBranchExists checks if a branch exists on the remote.
// NOTE: For named remotes with a separate pushurl, this checks the fetch URL.
// Use PushRemoteBranchExists to verify branches that were pushed.
func (g *Git) RemoteBranchExists(remote, branch string) (bool, error) {
	out, err := g.runWithTimeout(remoteQueryTimeout, "ls-remote", "--heads", remote, branch)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// RemoteBranchTip returns the SHA at refs/heads/<branch> on the remote.
// An empty SHA with nil error means the branch is missing.
func (g *Git) RemoteBranchTip(remote, branch string) (string, error) {
	out, err := g.runWithTimeout(remoteQueryTimeout, "ls-remote", "--heads", remote, branch)
	if err != nil {
		return "", err
	}
	return parseLSRemoteTip(out, branch), nil
}

// RemoteRefsContaining returns the remote-tracking branches whose history
// contains sha (git branch -r --contains <sha>), e.g. "origin/main".
//
// Reachability — not tip membership — is what proves a commit survives a
// delete. Work that was already merged into main is an ancestor of origin/main,
// so `ls-remote --heads <branch>` finds nothing for the original branch name
// while the commit is in fact preserved. Callers that need certainty against
// the remote's current state must Fetch first: these are the local
// remote-tracking refs.
func (g *Git) RemoteRefsContaining(sha string) ([]string, error) {
	if strings.TrimSpace(sha) == "" {
		return nil, fmt.Errorf("RemoteRefsContaining: empty sha")
	}
	out, err := g.run("branch", "-r", "--contains", sha)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// `git branch -r` also prints the symbolic "origin/HEAD -> origin/main"
		// entry, which names no branch of its own.
		if line == "" || strings.Contains(line, "->") {
			continue
		}
		refs = append(refs, line)
	}
	return refs, nil
}

// PushRemoteBranchExists checks if a branch exists on the push target of a remote.
// With a fork-based or local-bare-repo workflow (pushurl configured), pushes go to
// the push URL but ls-remote resolves the fetch URL. This method queries the push
// URL directly so verification matches where the branch was actually pushed.
// Falls back to RemoteBranchExists when no custom push URL is configured.
func (g *Git) PushRemoteBranchExists(remote, branch string) (bool, error) {
	pushTarget := g.pushTarget(remote)
	if pushTarget == remote {
		return g.RemoteBranchExists(remote, branch)
	}
	out, err := g.runWithTimeout(remoteQueryTimeout, "ls-remote", "--heads", pushTarget, branch)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// PushRemoteBranchTip returns the SHA at refs/heads/<branch> on the push target.
// This mirrors PushRemoteBranchExists: when remote.<name>.pushurl differs from
// the fetch URL, verification must query the push URL because that is where the
// preceding git push wrote.
func (g *Git) PushRemoteBranchTip(remote, branch string) (string, error) {
	pushTarget := g.pushTarget(remote)
	if pushTarget == remote {
		return g.RemoteBranchTip(remote, branch)
	}
	return g.RemoteBranchTip(pushTarget, branch)
}

func (g *Git) pushTarget(remote string) string {
	fetchURL, fetchErr := g.RemoteURL(remote)
	pushURL, pushErr := g.GetPushURL(remote)
	if fetchErr != nil || pushErr != nil || pushURL == fetchURL {
		return remote
	}
	return pushURL
}

// VerifyPushedCommit verifies that the push target branch tip is exactly commit.
// gt/refinery callers invoke this immediately after a push, before closing beads
// or creating downstream merge artifacts. Exact-tip verification catches the
// dangerous case where git push exits 0 but leaves the remote branch stale.
func (g *Git) VerifyPushedCommit(remote, branch, commit string) error {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return fmt.Errorf("verified_push_failed: empty commit for %s/%s", remote, branch)
	}
	tip, err := g.PushRemoteBranchTip(remote, branch)
	if err != nil {
		return fmt.Errorf("verified_push_failed: unable to read %s/%s: %w", remote, branch, err)
	}
	if tip == "" {
		return fmt.Errorf("verified_push_failed: branch %s/%s missing after push (expected %s)", remote, branch, shortSHA(commit))
	}
	if tip != commit {
		return fmt.Errorf("verified_push_failed: commit %s not on %s/%s (remote tip %s)", shortSHA(commit), remote, branch, shortSHA(tip))
	}
	return nil
}

// VerifyPushedCommitReachableFromPushTarget verifies that commit landed on the
// push target branch, either literally (exact tip or ancestor) or by content
// (same patch replayed on a moved base). Use this only for shared target
// branches where a later fast-forward push by another actor may legitimately
// advance the tip.
//
// A sequential merge queue rebases each MR onto the moved target before
// merging, which rewrites the submitted commit's SHA even though its content
// landed unchanged — a strict ancestor check alone would fail for every MR
// that required a rebase. Falling back to content-preservation (same
// technique as branch-preservation checks elsewhere, see aa-apw) recognizes
// that case instead of misreporting it as an unverified push.
func (g *Git) VerifyPushedCommitReachableFromPushTarget(remote, branch, commit string) error {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return fmt.Errorf("verified_push_failed: empty commit for %s/%s", remote, branch)
	}
	tip, err := g.PushRemoteBranchTip(remote, branch)
	if err != nil {
		return fmt.Errorf("verified_push_failed: unable to read %s/%s: %w", remote, branch, err)
	}
	if tip == "" {
		return fmt.Errorf("verified_push_failed: branch %s/%s missing after push (expected %s)", remote, branch, shortSHA(commit))
	}
	if tip == commit {
		return nil
	}

	fetchTarget := g.pushTarget(remote)
	if _, err := g.run("fetch", "--no-tags", fetchTarget, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("verified_push_failed: unable to fetch %s/%s for ancestry check: %w", remote, branch, err)
	}
	reachable, err := g.IsAncestor(commit, "FETCH_HEAD")
	if err != nil {
		return fmt.Errorf("verified_push_failed: unable to verify commit %s on %s/%s: %w", shortSHA(commit), remote, branch, err)
	}
	if reachable {
		return nil
	}
	if status, statusErr := g.preservationOfRefAgainstRef(commit, "FETCH_HEAD"); statusErr == nil && status.Preserved {
		return nil
	}
	return fmt.Errorf("verified_push_failed: commit %s not on %s/%s (remote tip %s)", shortSHA(commit), remote, branch, shortSHA(tip))
}

func parseLSRemoteTip(out, branch string) string {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if parts[1] == "refs/heads/"+branch {
			return parts[0]
		}
	}
	return ""
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// RemoteTrackingBranchExists checks if a remote-tracking branch ref exists locally
// (e.g. refs/remotes/origin/main), without hitting the network.
func (g *Git) RemoteTrackingBranchExists(remote, branch string) (bool, error) {
	ref := fmt.Sprintf("refs/remotes/%s/%s", remote, branch)
	_, err := g.run("show-ref", "--verify", "--quiet", ref)
	if err != nil {
		if strings.Contains(err.Error(), "exit status 1") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// DeleteBranch deletes a local branch.
func (g *Git) DeleteBranch(name string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := g.run("branch", flag, name)
	return err
}

// ListBranches returns all local branches matching a pattern.
// Pattern uses git's pattern matching (e.g., "polecat/*" matches all polecat branches).
// Returns branch names without the refs/heads/ prefix.
func (g *Git) ListBranches(pattern string) ([]string, error) {
	args := []string{"branch", "--list", "--format=%(refname:short)"}
	if pattern != "" {
		args = append(args, pattern)
	}
	out, err := g.run(args...)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// ResetBranch force-updates a branch to point to a ref.
// This is useful for resetting stale polecat branches to main.
// NOTE: This uses `git branch -f` which fails on the currently checked-out branch.
// Use ResetHard instead when the target branch is checked out.
func (g *Git) ResetBranch(name, ref string) error {
	_, err := g.run("branch", "-f", name, ref)
	return err
}

// ResetHard resets the current working tree and index to the given ref.
// Unlike ResetBranch, this works on the currently checked-out branch.
func (g *Git) ResetHard(ref string) error {
	_, err := g.run("reset", "--hard", ref)
	return err
}

// CleanForce removes untracked files and directories from the working tree.
// Excludes .runtime/ to preserve agent lock files and session state.
func (g *Git) CleanForce() error {
	_, err := g.run("clean", "-fd", "--exclude=.runtime")
	return err
}

// Rev returns the commit hash for the given ref.
func (g *Git) Rev(ref string) (string, error) {
	return g.run("rev-parse", ref)
}

// Parents returns the parent shas of commit, in order (git rev-list
// --parents -n 1). A merge commit's second parent is parents[1], which is the
// polecat head a merge queue merge brought in.
func (g *Git) Parents(commit string) ([]string, error) {
	out, err := g.run("rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return nil, fmt.Errorf("resolve commit %s: no such object", commit)
	}
	return fields[1:], nil
}

// FirstParentContains reports whether commit is on the first-parent chain of
// descendant — whether walking descendant's first parents reaches it.
//
// It is the membership test the editorial-coverage check's walk makes: that
// walk follows first parents only, so a commit inside a merged branch is an
// ancestor of the target without being a commit the check ever reads. A note
// stamped on such a commit is proof of nothing (gt-ljn8).
func (g *Git) FirstParentContains(commit, descendant string) (bool, error) {
	out, err := g.run("rev-list", "--first-parent", descendant)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == commit {
			return true, nil
		}
	}
	return false, nil
}

// IsAncestor checks if ancestor is an ancestor of descendant.
func (g *Git) IsAncestor(ancestor, descendant string) (bool, error) {
	_, err := g.run("merge-base", "--is-ancestor", ancestor, descendant)
	if err != nil {
		// Exit code 1 means not an ancestor, not an error
		if strings.Contains(err.Error(), "exit status 1") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CommitLandedOnTarget reports whether commit was already pushed to
// remote/target by an earlier merge: its exact tip, an ancestor of a later
// commit, or (a rebase-based queue can rewrite a commit's SHA while
// preserving content) a commit every one of whose own patches is already
// applied upstream by patch-id (git cherry).
//
// This deliberately stops short of counting a "merge-tree no-op" as landed,
// unlike VerifyPushedCommitReachableFromPushTarget: that signal is equally
// true of a commit that never contributed anything to target to begin with,
// which is a different question (an empty or superseded submission, not a
// landed one — see the refinery's own empty-merge checks, gt-j5cc). Reusing
// the broader check here would misreport that case as already landed
// instead of merge-ineligible, so this checks strictly: literal
// reachability, or every one of commit's own commits individually already
// present upstream by patch-id — never merely that merging it now would add
// nothing.
//
// A queue that squashes a multi-commit branch into one upstream commit
// defeats even the cherry check (no single upstream patch-id equals any one
// of the branch's own commits); that case has no fully-automatic answer here
// (see gt mq post-merge --landed-commit, which takes an explicit human
// attestation instead) and this reports it as not landed.
func (g *Git) CommitLandedOnTarget(remote, target, commit string) bool {
	commit = strings.TrimSpace(commit)
	target = strings.TrimSpace(target)
	if commit == "" || target == "" {
		return false
	}
	targetRef := remote + "/" + target

	if tip, err := g.Rev(targetRef); err == nil && strings.TrimSpace(tip) == commit {
		return true
	}
	if reachable, err := g.IsAncestor(commit, targetRef); err == nil && reachable {
		return true
	}

	base, err := g.MergeBase(targetRef, commit)
	if err != nil || base == commit {
		// commit is itself the merge-base (or the base couldn't be
		// resolved): there is no range of commit's own patches for cherry to
		// check, so there is no landed-content signal here.
		return false
	}
	out, err := g.Cherry(targetRef, commit)
	if err != nil {
		return false
	}
	return out != "" && CountCherryUnmergedCommits(out) == 0
}

// LogGrep reports whether any commit reachable from ref has a message
// containing pattern as a literal substring (git log --grep -F). Unlike
// IsAncestor, this survives a squash merge: a squash rewrites the branch
// tip into a new commit on the target, so ancestry can never confirm the
// work landed, but the target's squash commit message still carries the
// original text (e.g. an issue id) by convention.
func (g *Git) LogGrep(ref, pattern string) (bool, error) {
	out, err := g.run("log", ref, "--grep="+pattern, "-F", "-1", "--oneline")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Cherry runs `git cherry <upstream> <head>` to list commits on head that are
// not yet on upstream, comparing by patch-id. Each output line is prefixed with
// "+ " (patch not on upstream) or "- " (patch already applied upstream, e.g.
// via squash merge). Used to detect already-merged work that plain ancestor
// checks miss. See aa-apw.
func (g *Git) Cherry(upstream, head string) (string, error) {
	return g.run("cherry", upstream, head)
}

// WorktreeAdd creates a new worktree at the given path with a new branch.
// The new branch is created from the current HEAD.
// Skips LFS smudge filter during checkout (see WorktreeAddFromRef).
func (g *Git) WorktreeAdd(path, branch string) error {
	if _, err := g.runWithEnv(
		[]string{"worktree", "add", "-b", branch, path},
		[]string{"GIT_LFS_SKIP_SMUDGE=1"},
	); err != nil {
		return err
	}
	return InitSubmodules(path, g.submoduleReferencePath())
}

// WorktreeAddFromRef creates a new worktree at the given path with a new branch
// starting from the specified ref (e.g., "origin/main").
// Skips LFS smudge filter during checkout to avoid downloading large LFS objects
// over NFS (~72s for 473MB). LFS files appear as pointer files initially;
// callers can run "git lfs pull" later when LFS content is actually needed.
func (g *Git) WorktreeAddFromRef(path, branch, startPoint string) error {
	if _, err := g.runWithEnv(
		[]string{"worktree", "add", "-b", branch, path, startPoint},
		[]string{"GIT_LFS_SKIP_SMUDGE=1"},
	); err != nil {
		return err
	}
	return InitSubmodules(path, g.submoduleReferencePath())
}

// WorktreeAddDetached creates a new worktree at the given path with a detached HEAD.
// Skips LFS smudge filter during checkout (see WorktreeAddFromRef).
func (g *Git) WorktreeAddDetached(path, ref string) error {
	if _, err := g.runWithEnv(
		[]string{"worktree", "add", "--detach", path, ref},
		[]string{"GIT_LFS_SKIP_SMUDGE=1"},
	); err != nil {
		return err
	}
	return InitSubmodules(path, g.submoduleReferencePath())
}

// WorktreeAddExisting creates a new worktree at the given path for an existing branch.
// Skips LFS smudge filter during checkout (see WorktreeAddFromRef).
func (g *Git) WorktreeAddExisting(path, branch string) error {
	if _, err := g.runWithEnv(
		[]string{"worktree", "add", path, branch},
		[]string{"GIT_LFS_SKIP_SMUDGE=1"},
	); err != nil {
		return err
	}
	return InitSubmodules(path, g.submoduleReferencePath())
}

// WorktreeAddExistingForce creates a new worktree even if the branch is already checked out elsewhere.
// This is useful for cross-rig worktrees where multiple clones need to be on main.
func (g *Git) WorktreeAddExistingForce(path, branch string) error {
	if _, err := g.run("worktree", "add", "--force", path, branch); err != nil {
		return err
	}
	return InitSubmodules(path, g.submoduleReferencePath())
}

// submoduleReferencePath returns the mayor/rig path to use as --reference
// for submodule init. For bare repos (.repo.git), this resolves to the
// sibling mayor/rig directory which contains the initialized submodules.
// Returns empty string if no suitable reference path exists or if the
// reference repo is a shallow clone (git rejects shallow references).
func (g *Git) submoduleReferencePath() string {
	// For bare repos, the gitDir is <rig>/.repo.git
	// The reference clone is at <rig>/mayor/rig/
	if g.gitDir != "" {
		rigDir := filepath.Dir(g.gitDir)
		mayorRig := filepath.Join(rigDir, "mayor", "rig")
		if isValidSubmoduleReference(mayorRig) {
			return mayorRig
		}
	}

	// For regular clones (workDir-based), the workDir itself could be mayor/rig
	// but we don't want to reference ourselves. Check for a sibling .repo.git
	// to find the rig root, then use mayor/rig.
	if g.workDir != "" {
		dir := g.workDir
		for i := 0; i < 4; i++ {
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			if _, err := os.Stat(filepath.Join(parent, ".repo.git")); err == nil {
				mayorRig := filepath.Join(parent, "mayor", "rig")
				if mayorRig != g.workDir && isValidSubmoduleReference(mayorRig) {
					return mayorRig
				}
				break
			}
			dir = parent
		}
	}

	return ""
}

// isValidSubmoduleReference checks if a path is suitable as a --reference
// for git submodule update. It must have a tracked .gitmodules and not be a
// shallow clone (git rejects shallow repos as references).
func isValidSubmoduleReference(repoPath string) bool {
	if !hasTrackedGitmodules(repoPath) {
		return false
	}
	// Check if shallow — git rev-parse --is-shallow-repository
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "--is-shallow-repository")
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != "true"
}

// IsSparseCheckoutConfigured checks if sparse checkout is enabled for a given repo/worktree.
// This is used by doctor to detect legacy sparse checkout configurations that should be removed.
func IsSparseCheckoutConfigured(repoPath string) bool {
	cmd := exec.Command("git", "-C", repoPath, "config", "core.sparseCheckout")
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(output)) == "true"
}

// RemoveSparseCheckout disables sparse checkout for a repo/worktree and restores all files.
// This is used by doctor to clean up legacy sparse checkout configurations.
func RemoveSparseCheckout(repoPath string) error {
	if err := EnsureSafeMutationWorkDir(repoPath); err != nil {
		return err
	}

	// Use git sparse-checkout disable which properly restores hidden files
	cmd := exec.Command("git", "-C", repoPath, "sparse-checkout", "disable")
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("disabling sparse checkout: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// WorktreeRemove removes a worktree.
func (g *Git) WorktreeRemove(path string, force bool) error {
	args := []string{"worktree", "remove", path}
	if force {
		args = append(args, "--force")
	}
	_, err := g.run(args...)
	return err
}

// WorktreeMove moves a worktree to a new path, updating all git references.
// This is the correct way to relocate a worktree — using os.Rename breaks
// the .git file and worktree registry references. (GH#2056)
func (g *Git) WorktreeMove(oldPath, newPath string) error {
	_, err := g.run("worktree", "move", oldPath, newPath)
	return err
}

// WorktreePrune removes worktree entries for deleted paths.
func (g *Git) WorktreePrune() error {
	_, err := g.run("worktree", "prune")
	return err
}

// Worktree represents a git worktree.
type Worktree struct {
	Path   string
	Branch string
	Commit string
}

// WorktreeList returns all worktrees for this repository.
func (g *Git) WorktreeList() ([]Worktree, error) {
	out, err := g.run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}

	var worktrees []Worktree
	var current Worktree

	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			if current.Path != "" {
				worktrees = append(worktrees, current)
				current = Worktree{}
			}
			continue
		}

		switch {
		case strings.HasPrefix(line, "worktree "):
			current.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "HEAD "):
			current.Commit = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}

	// Don't forget the last one
	if current.Path != "" {
		worktrees = append(worktrees, current)
	}

	return worktrees, nil
}

// BranchCreatedDate returns the date when a branch was created.
// This uses the committer date of the first commit on the branch.
// Returns date in YYYY-MM-DD format.
func (g *Git) BranchCreatedDate(branch string) (string, error) {
	// Get the date of the first commit on the branch that's not on the default branch
	// Use merge-base to find where the branch diverged
	defaultBranch := g.RemoteDefaultBranch()
	mergeBase, err := g.run("merge-base", defaultBranch, branch)
	if err != nil {
		// If merge-base fails, fall back to the branch tip's date
		out, err := g.run("log", "-1", "--format=%cs", branch)
		if err != nil {
			return "", err
		}
		return out, nil
	}

	// Get the first commit after the merge base on this branch
	out, err := g.run("log", "--format=%cs", "--reverse", mergeBase+".."+branch)
	if err != nil {
		return "", err
	}

	// Get the first line (first commit's date)
	lines := strings.Split(out, "\n")
	if len(lines) > 0 && lines[0] != "" {
		return lines[0], nil
	}

	// If no commits after merge-base, the branch points to merge-base
	// Return the merge-base commit date
	out, err = g.run("log", "-1", "--format=%cs", mergeBase)
	if err != nil {
		return "", err
	}
	return out, nil
}

// CommitsAhead returns the number of commits that branch has ahead of base.
// DiffStat returns the --stat output for a diff range (e.g., "main...feature").
func (g *Git) DiffStat(rangeSpec string) (string, error) {
	return g.run("diff", "--stat", rangeSpec)
}

// For example, CommitsAhead("main", "feature") returns how many commits
// are on feature that are not on main.
func (g *Git) CommitsAhead(base, branch string) (int, error) {
	out, err := g.run("rev-list", "--count", base+".."+branch)
	if err != nil {
		return 0, err
	}

	var count int
	_, err = fmt.Sscanf(out, "%d", &count)
	if err != nil {
		return 0, fmt.Errorf("parsing commit count: %w", err)
	}

	return count, nil
}

// CountCommitsBehind returns the number of commits that HEAD is behind the given ref.
// For example, CountCommitsBehind("origin/main") returns how many commits
// are on origin/main that are not on the current HEAD.
func (g *Git) CountCommitsBehind(ref string) (int, error) {
	out, err := g.run("rev-list", "--count", "HEAD.."+ref)
	if err != nil {
		return 0, err
	}

	var count int
	_, err = fmt.Sscanf(out, "%d", &count)
	if err != nil {
		return 0, fmt.Errorf("parsing commit count: %w", err)
	}

	return count, nil
}

// BranchContamination holds the result of a branch contamination check.
type BranchContamination struct {
	Behind int // commits HEAD is behind base (e.g., origin/main)
	Ahead  int // commits HEAD is ahead of base
}

// CheckBranchContamination checks whether the current branch has diverged
// significantly from a base ref (typically origin/main). Returns the number
// of commits behind and ahead, letting callers decide severity thresholds.
// (GH#2220)
func (g *Git) CheckBranchContamination(baseRef string) (BranchContamination, error) {
	var result BranchContamination

	behind, err := g.CountCommitsBehind(baseRef)
	if err != nil {
		return result, fmt.Errorf("counting commits behind %s: %w", baseRef, err)
	}
	result.Behind = behind

	ahead, err := g.CommitsAhead(baseRef, "HEAD")
	if err != nil {
		return result, fmt.Errorf("counting commits ahead of %s: %w", baseRef, err)
	}
	result.Ahead = ahead

	return result, nil
}

// StashCount returns the number of stashes belonging to the current branch.
// Git stashes are stored in the main repo (.git/refs/stash) and shared across
// all worktrees. Counting all stashes is incorrect for worktree-based polecats:
// a fresh polecat worktree would inherit stash count from siblings, blocking
// Remove(force=true) on work it never created. Filter by current branch name
// to only count stashes that actually belong to this worktree.
func (g *Git) StashCount() (int, error) {
	out, err := g.run("stash", "list")
	if err != nil {
		return 0, err
	}

	if out == "" {
		return 0, nil
	}

	// Get current branch to filter stashes.
	// If we can't determine the branch (detached HEAD, error), count all
	// stashes as a safe fallback — better to over-count than silently lose work.
	branch, branchErr := g.CurrentBranch()
	filterByBranch := branchErr == nil && branch != "" && branch != "HEAD"

	// Stash reflog lines have the format:
	//   stash@{N}: WIP on <branch>: <hash> <message>
	//   stash@{N}: On <branch>: <message>
	// We anchor the match to ": WIP on <branch>:" or ": On <branch>:" to avoid
	// false positives from commit messages that happen to contain "on <branch>:".
	wipPrefix := ": WIP on " + branch + ":"
	onPrefix := ": On " + branch + ":"

	lines := strings.Split(out, "\n")
	count := 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		if filterByBranch {
			if !strings.Contains(line, wipPrefix) && !strings.Contains(line, onPrefix) {
				continue
			}
		}
		count++
	}
	return count, nil
}

// StashCountAll returns the total number of repo-wide stashes visible from the
// worktree. Git stores stashes in the shared repository, so callers must not use
// this as per-worktree risk; use StashCount for current-branch risk instead.
func (g *Git) StashCountAll() (int, error) {
	out, err := g.run("stash", "list")
	if err != nil {
		return 0, err
	}
	if out == "" {
		return 0, nil
	}

	count := 0
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			count++
		}
	}
	return count, nil
}

// StashEntry represents one entry from `git stash list`, scoped to the current branch.
type StashEntry struct {
	Ref     string // e.g. "stash@{2}"
	Message string // e.g. "WIP on main: <hash> <subject>"
}

// StashListForBranch returns all stash entries belonging to the current branch,
// ordered as `git stash list` returns them (newest first, i.e. stash@{0} first).
// Filtering matches StashCount: only entries with ": WIP on <branch>:" or
// ": On <branch>:" prefixes are returned, since stashes are global to the repo
// but conceptually belong to the worktree where they were created.
func (g *Git) StashListForBranch() ([]StashEntry, error) {
	out, err := g.run("stash", "list")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}

	branch, branchErr := g.CurrentBranch()
	filterByBranch := branchErr == nil && branch != "" && branch != "HEAD"
	wipPrefix := ": WIP on " + branch + ":"
	onPrefix := ": On " + branch + ":"

	var entries []StashEntry
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if filterByBranch {
			if !strings.Contains(line, wipPrefix) && !strings.Contains(line, onPrefix) {
				continue
			}
		}
		// Lines have the form "stash@{N}: <message>"
		colonIdx := strings.Index(line, ":")
		if colonIdx <= 0 {
			continue
		}
		entries = append(entries, StashEntry{
			Ref:     line[:colonIdx],
			Message: strings.TrimSpace(line[colonIdx+1:]),
		})
	}
	return entries, nil
}

// StashPop applies the given stash ref to the working tree and drops it on success.
// Returns an error if the pop has conflicts (working tree is left as-is for manual
// resolution). Callers should treat conflict errors as "stop, escalate to user".
func (g *Git) StashPop(ref string) error {
	if ref == "" {
		return fmt.Errorf("stash ref required")
	}
	if _, err := g.run("stash", "pop", ref); err != nil {
		return fmt.Errorf("git stash pop %s: %w", ref, err)
	}
	return nil
}

// UnpushedCommits returns the number of commits that are not pushed to the remote.
// It prefers the exact remote branch when one exists, because polecat branches may
// track origin/main while pushing work to origin/<current-branch>.
// Returns 0 if there is no upstream or exact remote branch configured.
//
// The exact-branch evidence is read from the remote itself (ls-remote). Use
// UnpushedCommitsLocal when measuring many worktrees in one run — see
// BranchPreservationStatusLocal.
func (g *Git) UnpushedCommits() (int, error) {
	return g.unpushedCommits(g.BranchPreservationStatus)
}

// UnpushedCommitsLocal is UnpushedCommits without the network round trip: the
// exact-branch evidence comes from this clone's remote-tracking refs. It is
// the counterpart a caller needs when it measures one worktree per seat
// (gt-8q0s). It can answer above UnpushedCommits' count or below it, depending
// on which way this clone's view of the branch has drifted — see
// BranchPreservationStatusLocal for both cases.
func (g *Git) UnpushedCommitsLocal() (int, error) {
	return g.unpushedCommits(g.BranchPreservationStatusLocal)
}

// UnpushedCommitsLocalFailClosed is UnpushedCommitsLocal with the opposite
// posture on an unresolvable comparison. UnpushedCommitsLocal treats
// errNoComparisonRefs as "nothing to preserve" (0, nil), which is correct for
// a caller that only reports a count. A caller about to discard a worktree
// because it looks clean cannot take that shortcut: a branch that was never
// fetched into this clone (no exact-branch, upstream, or default-branch ref
// to compare against) is exactly the case with no evidence either way, and
// reading "no evidence" as "nothing to preserve" is the silent-discard bug
// this exists to close (gt-utt4). This reports at least 1 whenever the
// comparison itself could not be made, so the caller treats the branch as
// having unpreserved work rather than as clean.
func (g *Git) UnpushedCommitsLocalFailClosed() (int, error) {
	branch, branchErr := g.CurrentBranch()
	if branchErr != nil || branch == "" || branch == "HEAD" {
		branch = ""
	}

	status, err := g.BranchPreservationStatusLocal(branch, "origin", nil)
	if err != nil {
		if errors.Is(err, errNoComparisonRefs) {
			return 1, nil
		}
		return 0, err
	}
	return status.UnpreservedPatchCount, nil
}

// unpushedCommits is the shared body: read the current branch, then count the
// patches branchPreservation would not find preserved. Taking the status
// lookup as a parameter is what keeps the local and live variants from
// drifting — they differ only in where the branch tip is read from.
func (g *Git) unpushedCommits(branchPreservation func(localBranch, remote string, targets []string) (BranchPreservationStatus, error)) (int, error) {
	branch, branchErr := g.CurrentBranch()
	if branchErr != nil || branch == "" || branch == "HEAD" {
		branch = ""
	}

	status, err := branchPreservation(branch, "origin", nil)
	if err != nil {
		if errors.Is(err, errNoComparisonRefs) {
			return 0, nil
		}
		return 0, err
	}
	return status.UnpreservedPatchCount, nil
}

func (g *Git) countCommitsAhead(base string) (int, error) {
	out, err := g.run("rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0, err
	}

	var count int
	_, err = fmt.Sscanf(out, "%d", &count)
	if err != nil {
		return 0, fmt.Errorf("parsing unpushed count: %w", err)
	}

	return count, nil
}

func (g *Git) unpushedFromExactRemoteBranch(localBranch, remote string) (int, bool, error) {
	remoteSHA, err := g.PushRemoteBranchTip(remote, localBranch)
	if err != nil || remoteSHA == "" {
		return 0, false, err
	}

	count, err := g.countCommitsAhead(remoteSHA)
	return count, true, err
}

// BranchPreservationStatus describes whether HEAD is already preserved on a
// durable branch, and how many patch-unique commits remain if it is not.
type BranchPreservationStatus struct {
	Preserved             bool
	ComparisonBase        string
	UnpreservedPatchCount int
	Evidence              string
}

// BranchPreservationStatus checks whether HEAD is safe relative to the actual
// custody target for the branch. It prefers proof from the exact pushed source
// branch, then explicit target branches, then upstream. It only falls back to the
// remote default branch when no target/custody/upstream evidence exists — a
// worktree with no branch at all (detached HEAD) is first judged against every
// remote-tracking branch, since a default-branch comparison would report its
// pushed work as unpreserved.
func (g *Git) BranchPreservationStatus(localBranch, remote string, targets []string) (BranchPreservationStatus, error) {
	return g.branchPreservationStatus(localBranch, remote, targets, true)
}

// BranchTargetStatus checks whether HEAD is already represented on the branch's
// target/custody refs. Unlike BranchPreservationStatus, the exact pushed source
// branch is not enough evidence because pushed-but-unsubmitted work still needs
// merge-queue recovery.
func (g *Git) BranchTargetStatus(localBranch, remote string, targets []string) (BranchPreservationStatus, error) {
	return g.branchPreservationStatus(localBranch, remote, targets, false)
}

// BranchPreservationStatusLocal is BranchPreservationStatus with both custody
// lookups — the branch's own tip and a branch-less HEAD's — answered from this
// clone's remote-tracking refs instead of the remote. Everything after those
// lookups — the integration-branch candidates, the ancestry / merge-tree /
// cherry judging — is the same code, so a local verdict and a live verdict
// agree about what "preserved" means and differ only in the branch tip they
// judged.
//
// They differ in both directions, whichever way this clone's refs have drifted
// from the remote (gt-dt0k). A ref this clone never had — never fetched, or
// already pruned — resolves to no evidence, drops the comparison to the
// integration branch, and reports work that did land as unpreserved: stricter
// than the live probe, which is the direction that flags a seat for recovery
// rather than clearing it. A ref that outlives its remote branch — the branch
// deleted or rewritten on the remote by something other than a delete push
// from this clone, with no fetch --prune since — still holds HEAD, and reads
// as preserved on a branch the remote no longer has: looser than the live
// probe. Nothing local separates the two cases — this clone's refs, config,
// and worktree are byte-identical to a clone whose branch is still on the
// remote — so no network-free probe can tell them apart, and the miss costs a
// skipped recovery rather than a lost branch, since the commits stay reachable
// from the ref that was just trusted. That bounded, one-seat error is the
// price of N local ref reads instead of N network round trips.
//
// Use it for bulk enumeration (gt polecat list, the dashboard's inventory
// poll). Use BranchPreservationStatus where the answer gates a single
// irreversible action and a network round trip is affordable.
func (g *Git) BranchPreservationStatusLocal(localBranch, remote string, targets []string) (BranchPreservationStatus, error) {
	return g.branchPreservationStatusWith(localBranch, remote, targets, true, g.localRemoteBranchTip, g.detachedHeadCustodyLocal)
}

// localRemoteBranchTip resolves a branch's tip from refs this clone already
// holds, spawning no network round trip. A missing tracking ref yields "" with
// no error, which the caller reads exactly as the live probe reads a remote
// that has no such branch.
//
// The local branch ref (refs/heads/<branch>) is deliberately NOT consulted as
// a fallback. git push creates the tracking ref, so the two agree whenever the
// branch was ever pushed; when it was not, refs/heads/<branch> would resolve
// to HEAD itself, report every seat's work as preserved on a branch that only
// exists locally, and clear the unpushed-work blocker this check exists to
// raise.
func (g *Git) localRemoteBranchTip(remote, branch string) (string, error) {
	sha, err := g.Rev("refs/remotes/" + remote + "/" + branch)
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(sha), nil
}

// RefPreservedByRef reports whether the work on head is already contained in
// the given ref, judged the way BranchPreservationStatus judges HEAD against a
// custody ref: ancestry, then a merge-tree no-op, then per-patch (cherry)
// preservation. Both refs are taken as given, so this answers questions that
// are not about a branch's own custody — "is this work already in the
// integration branch on origin" (polecat.ProbeWorkLandedOnRef).
//
// The merge-tree arm is what makes a squash-merged branch count as preserved:
// its commits are never ancestors of the integration branch, but merging it in
// changes nothing.
func (g *Git) RefPreservedByRef(head, ref string) (BranchPreservationStatus, error) {
	return g.preservationOfRefAgainstRef(head, ref)
}

// remoteBranchTipFunc reads the tip of a branch on a remote. The live
// implementation is PushRemoteBranchTip (an ls-remote: one network round trip
// per call); the local one reads the remote-tracking ref this clone already
// has. Which one a caller passes is a cost decision, made once, at the top:
// a caller that measures every seat in the town cannot afford a round trip
// per seat (gt-8q0s), while a caller that gates a single spawn wants the
// remote's actual current tip.
type remoteBranchTipFunc func(remote, branch string) (string, error)

// detachedHeadCustodyFunc answers "is this worktree's branch-less HEAD already
// on the remote, and on which ref" — the question a preservation verdict must
// ask when there is no branch name to ask it about. It reads no branch name,
// so it cannot reuse remoteBranchTipFunc.
type detachedHeadCustodyFunc func(remote, head string) (string, bool)

func (g *Git) branchPreservationStatus(localBranch, remote string, targets []string, includeExactBranch bool) (BranchPreservationStatus, error) {
	return g.branchPreservationStatusWith(localBranch, remote, targets, includeExactBranch, g.PushRemoteBranchTip, g.detachedHeadCustodyRemote)
}

// branchPreservationStatusWith is the shared implementation behind every
// preservation verdict — live and local alike. Only the two custody lookups
// (the exact branch's tip, and a branch-less HEAD's) are parameterized; the
// candidate set, the ancestry/merge-tree/cherry judging, and the fail-closed
// ordering are identical, so the two fidelity levels cannot disagree about
// what "preserved" means.
func (g *Git) branchPreservationStatusWith(localBranch, remote string, targets []string, includeExactBranch bool, branchTip remoteBranchTipFunc, detachedCustody detachedHeadCustodyFunc) (BranchPreservationStatus, error) {
	if remote == "" {
		remote = "origin"
	}
	var result BranchPreservationStatus
	var candidates []string
	hasEvidence := len(nonEmptyUnique(targets)) > 0

	if includeExactBranch && localBranch != "" && localBranch != "HEAD" {
		if remoteSHA, err := branchTip(remote, localBranch); err == nil && remoteSHA != "" {
			hasEvidence = true
			result.ComparisonBase = remote + "/" + localBranch
			if contains, containsErr := g.refContainsHead(remoteSHA); containsErr == nil && contains {
				result.Preserved = true
				result.UnpreservedPatchCount = 0
				result.Evidence = "exact_remote_branch"
				return result, nil
			}
			candidates = append(candidates, remoteSHA)
		}
	}

	// A detached worktree names no branch, so the exact-branch arm above has
	// nothing to ask the remote about and the fallback below answers "has this
	// landed on main" instead of "does this survive anywhere". A finished
	// polecat is left detached at its branch tip, so that fallback reported
	// has_unpushed, as a seat's only blocker, for work already sitting on an
	// origin branch tip with nothing at risk (gt-1bpgm). Custody of a pushed
	// branch is the evidence the exact-branch arm already accepts for a named
	// branch, so this matches the standard rather than lowering it.
	//
	// Target status (includeExactBranch false) excludes exactly this evidence
	// by design — pushed-but-unsubmitted work still needs merge-queue recovery
	// (see BranchTargetStatus).
	if includeExactBranch && (localBranch == "" || localBranch == "HEAD") {
		if head, err := g.Rev("HEAD"); err == nil {
			if ref, ok := detachedCustody(remote, strings.TrimSpace(head)); ok {
				result.Preserved = true
				result.ComparisonBase = ref
				result.UnpreservedPatchCount = 0
				result.Evidence = "detached_head_on_remote_branch"
				return result, nil
			}
		}
	}

	for _, target := range nonEmptyUnique(targets) {
		if ref, ok := g.resolveComparisonRef(target, remote); ok {
			candidates = append(candidates, ref)
		}
	}

	if upstream, err := g.run("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil && strings.TrimSpace(upstream) != "" {
		upstream = strings.TrimSpace(upstream)
		if includeExactBranch || !isPolecatSelfUpstream(localBranch, remote, upstream) {
			hasEvidence = true
			candidates = append(candidates, upstream)
		}
	}

	if !hasEvidence {
		for _, ref := range []string{remote + "/" + g.RemoteDefaultBranch(), remote + "/main", remote + "/master"} {
			if resolved, ok := g.resolveComparisonRef(ref, remote); ok {
				candidates = append(candidates, resolved)
			}
		}
	}

	candidates = nonEmptyUnique(candidates)
	if len(candidates) == 0 {
		if hasEvidence {
			return result, fmt.Errorf("no target/custody refs resolved")
		}
		return result, errNoComparisonRefs
	}

	var lastErr error
	for _, ref := range candidates {
		candidate, err := g.preservationAgainstRef(ref)
		if err != nil {
			lastErr = err
			continue
		}
		if candidate.Evidence == "" {
			candidate.Evidence = "comparison_ref"
		}
		if candidate.Preserved {
			return candidate, nil
		}
		if result.ComparisonBase == "" {
			result = candidate
		}
	}
	if result.ComparisonBase != "" {
		return result, nil
	}
	if lastErr != nil {
		return result, lastErr
	}
	return result, fmt.Errorf("no usable comparison refs")
}

func isPolecatSelfUpstream(localBranch, remote, upstream string) bool {
	return strings.HasPrefix(localBranch, "polecat/") && upstream == remote+"/"+localBranch
}

func (g *Git) refContainsHead(ref string) (bool, error) {
	head, err := g.Rev("HEAD")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(ref) == strings.TrimSpace(head) {
		return true, nil
	}
	return g.IsAncestor("HEAD", ref)
}

// detachedHeadCustodyLocal names a remote-tracking branch that holds head:
// the polecat branch it was pushed to, or any branch the work was merged into.
// Free, and blind to branches this clone never fetched — the same one-sided
// error the rest of the local level has.
func (g *Git) detachedHeadCustodyLocal(remote, head string) (string, bool) {
	refs, err := g.RemoteRefsContaining(head)
	if err != nil || len(refs) == 0 {
		return "", false
	}
	return refs[0], true
}

// detachedHeadCustodyRemote is detachedHeadCustodyLocal plus a match against
// the remote's own branch tips, which is the case a detached seat actually
// lands in: by the time the worktree is left branch-less, the local branch and
// its tracking ref are gone too, so only the remote can still name the work.
//
// Tip membership, not reachability: reachability against the remote would cost
// a round trip per branch. A match proves the commit is on the remote now; a
// miss proves nothing, which is the direction a preservation claim needs.
func (g *Git) detachedHeadCustodyRemote(remote, head string) (string, bool) {
	if ref, ok := g.detachedHeadCustodyLocal(remote, head); ok {
		return ref, true
	}
	refs, err := g.ListRemoteRefsWithHashes(g.pushTarget(remote), "refs/heads/")
	if err != nil {
		return "", false
	}
	for _, ref := range refs {
		if strings.TrimSpace(ref.Hash) == head {
			return remote + "/" + strings.TrimPrefix(ref.Name, "refs/heads/"), true
		}
	}
	return "", false
}

func (g *Git) resolveComparisonRef(ref, remote string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	for _, candidate := range comparisonRefCandidates(ref, remote) {
		if ok, err := g.RefExists(candidate); err == nil && ok {
			return candidate, true
		}
	}
	return "", false
}

func comparisonRefCandidates(ref, remote string) []string {
	if strings.HasPrefix(ref, "refs/") || strings.HasPrefix(ref, remote+"/") {
		return []string{ref}
	}
	if strings.HasPrefix(ref, "upstream/") {
		return []string{ref}
	}
	if !strings.Contains(ref, "/") && remote != "upstream" {
		return []string{"upstream/" + ref, remote + "/" + ref, ref}
	}
	return []string{remote + "/" + ref, ref}
}

func (g *Git) preservationAgainstRef(ref string) (BranchPreservationStatus, error) {
	return g.preservationOfRefAgainstRef("HEAD", ref)
}

func (g *Git) preservationOfRefAgainstRef(head, ref string) (BranchPreservationStatus, error) {
	status := BranchPreservationStatus{ComparisonBase: ref}
	if contains, err := g.IsAncestor(head, ref); err == nil && contains {
		status.Preserved = true
		status.Evidence = "ancestor"
		return status, nil
	}
	if preserved, err := g.mergeTreeNoopBetweenRefs(head, ref); err == nil && preserved {
		status.Preserved = true
		status.Evidence = "merge_tree_noop"
		return status, nil
	}
	out, err := g.Cherry(ref, head)
	if err != nil {
		return status, err
	}
	status.UnpreservedPatchCount = CountCherryUnmergedCommits(out)
	status.Preserved = status.UnpreservedPatchCount == 0
	if status.Preserved {
		status.Evidence = "cherry"
	}
	return status, nil
}

// MergedTree returns the tree a conflict-free merge of revA and revB would
// produce at their merge base. It is what "is this merge commit the merge of
// its parents?" is measured against: a merge commit whose own tree equals
// this changed nothing the two parents did not, while one that differs
// carries a conflict resolution or an edit no parent made.
//
// An error means the two revs have no conflict-free merge tree at all
// (git merge-tree exits non-zero and lists the conflicts), which is a
// different answer from "here is the tree".
func (g *Git) MergedTree(revA, revB string) (string, error) {
	out, err := g.run("merge-tree", "--write-tree", revA, revB)
	if err != nil {
		return "", fmt.Errorf("merge-tree --write-tree %s %s: %w", shortSHA(revA), shortSHA(revB), err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("git merge-tree produced no tree for %s %s", shortSHA(revA), shortSHA(revB))
	}
	return fields[0], nil
}

func (g *Git) mergeTreeNoopAgainstRef(ref string) (bool, error) {
	return g.mergeTreeNoopBetweenRefs("HEAD", ref)
}

func (g *Git) mergeTreeNoopBetweenRefs(head, ref string) (bool, error) {
	refTree, err := g.run("rev-parse", ref+"^{tree}")
	if err != nil {
		return false, err
	}
	mergedTree, err := g.run("merge-tree", "--write-tree", ref, head)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(mergedTree) == strings.TrimSpace(refTree), nil
}

// PushRemoteRefTargetStatus checks whether a push-remote ref is preserved on
// target. It fetches the exact candidate ref first so remote-only tips and split
// fetch/push remotes are classified against the listed hash, not stale tracking
// refs.
func (g *Git) PushRemoteRefTargetStatus(remote string, ref RemoteRef, target string) (BranchPreservationStatus, error) {
	var status BranchPreservationStatus
	refName := strings.TrimSpace(ref.Name)
	expectedHash := strings.TrimSpace(ref.Hash)
	if refName == "" || expectedHash == "" {
		return status, fmt.Errorf("remote ref is missing name or hash")
	}

	if _, err := g.run("fetch", "--no-tags", g.pushTarget(remote), refName); err != nil {
		return status, fmt.Errorf("fetching candidate %s: %w", refName, err)
	}
	fetchedHash, err := g.Rev("FETCH_HEAD")
	if err != nil {
		return status, fmt.Errorf("resolving fetched candidate %s: %w", refName, err)
	}
	fetchedHash = strings.TrimSpace(fetchedHash)
	if fetchedHash != expectedHash {
		return status, fmt.Errorf("candidate %s changed while pruning: expected %s, fetched %s", refName, shortSHA(expectedHash), shortSHA(fetchedHash))
	}

	return g.preservationOfRefAgainstRef("FETCH_HEAD", target)
}

// CountCherryUnmergedCommits counts `git cherry` lines whose patches are not
// present on the comparison base.
func CountCherryUnmergedCommits(out string) int {
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "+") {
			count++
		}
	}
	return count
}

func nonEmptyUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// UncommittedWorkStatus contains information about uncommitted work in a repo.
type UncommittedWorkStatus struct {
	HasUncommittedChanges bool
	StashCount            int
	UnpushedCommits       int
	// Details for error messages
	ModifiedFiles  []string
	UntrackedFiles []string
	UnmergedFiles  []string
	// StagedOnly lists paths whose index differs from HEAD but whose working
	// tree matches the index (GitStatus.StagedOnly) — candidates for the
	// checkout-skew classification IndexSkewFiles performs. Kept as raw paths
	// here because classifying them costs several extra git subprocesses
	// (classifyIndexSkew); every CheckUncommittedWork caller gets this slice
	// for free, but only a caller that actually asks via IndexSkewFiles pays
	// for the classification (gt-8q0s: gt done and the other non-reuse
	// consumers never do).
	StagedOnly []string

	indexSkewComputed bool
	indexSkewFiles    []string
}

// IndexSkewFiles is the subset of StagedOnly whose content already matches a
// comparison ref this checkout is at or behind. This is the shared-.repo.git
// checkout-skew pattern from gt-ui2x: a dormant checkout's index describes a
// commit that moved out from under it, not real unsaved work. Only
// CleanExcludingRuntimeAndIndexSkew treats these as non-blocking — every other
// consumer (HasUncommittedChanges, Clean, gt done) is unaffected, so real
// staged-but-uncommitted work still blocks everywhere it always has.
//
// The classification runs several git subprocesses (classifyIndexSkew), so it
// is computed lazily on first call and memoized — a caller that never asks
// (gt done and the other CheckUncommittedWork consumers) never pays for it,
// and a reuse-gate caller that asks more than once only pays once (gt-8q0s).
func (s *UncommittedWorkStatus) IndexSkewFiles(g *Git) []string {
	if !s.indexSkewComputed {
		s.indexSkewFiles = g.classifyIndexSkew(s.StagedOnly)
		s.indexSkewComputed = true
	}
	return s.indexSkewFiles
}

// Clean returns true if there is no uncommitted work.
func (s *UncommittedWorkStatus) Clean() bool {
	return !s.HasUncommittedChanges && s.StashCount == 0 && s.UnpushedCommits == 0 && len(s.UnmergedFiles) == 0
}

// CleanExcludingBeads returns true if the only uncommitted changes are .beads/ files.
// This is useful for polecat stale detection where beads database files are synced
// across worktrees and shouldn't block cleanup.
func (s *UncommittedWorkStatus) CleanExcludingBeads() bool {
	// Stashes and unpushed commits always count as uncommitted work
	if s.StashCount > 0 || s.UnpushedCommits > 0 || len(s.UnmergedFiles) > 0 {
		return false
	}

	// Check if all modified files are beads files
	for _, f := range s.ModifiedFiles {
		if !isBeadsPath(f) {
			return false
		}
	}

	// Check if all untracked files are beads files
	for _, f := range s.UntrackedFiles {
		if !isBeadsPath(f) {
			return false
		}
	}

	return true
}

// isBeadsPath returns true if the path is a .beads/ file.
func isBeadsPath(path string) bool {
	return strings.Contains(path, ".beads/") || strings.Contains(path, ".beads\\")
}

// runtimeArtifactRoot returns the path that should be reset when a runtime artifact
// is staged. Directory artifacts return the directory root so large trees like
// nested node_modules are unstaged with one pathspec instead of thousands.
func runtimeArtifactRoot(path string) (string, bool) {
	path = strings.TrimPrefix(filepath.ToSlash(strings.ReplaceAll(path, "\\", "/")), "./")
	bare := strings.TrimSuffix(path, "/")
	if bare == "" {
		return "", false
	}

	parts := strings.Split(bare, "/")
	for i, part := range parts {
		switch part {
		case ".beads", ".claude", ".opencode", ".runtime", ".logs", "__pycache__", "node_modules", ".vite", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".cache", "coverage", "htmlcov":
			return strings.Join(parts[:i+1], "/") + "/", true
		}
	}

	base := filepath.Base(bare)
	lower := strings.ToLower(base)
	if base == "CLAUDE.local.md" || base == ".DS_Store" || strings.HasSuffix(lower, ".db") || strings.HasSuffix(lower, ".pyc") || strings.HasSuffix(lower, ".pyo") {
		return bare, true
	}

	return "", false
}

// isGasTownRuntimePath returns true if the path is a runtime artifact that should
// not block gt done. These paths are managed by tooling or test/build commands,
// not by the developer, and must not be auto-saved into polecat MRs.
func isGasTownRuntimePath(path string) bool {
	_, ok := runtimeArtifactRoot(path)
	return ok
}

// RuntimeArtifactPathspecs returns deduplicated git pathspecs for runtime
// artifacts in paths. Callers can pass the result to git reset after git add
// to keep generated state out of safety-net commits.
func RuntimeArtifactPathspecs(paths []string) []string {
	seen := make(map[string]bool)
	var pathspecs []string
	for _, f := range paths {
		root, ok := runtimeArtifactRoot(f)
		if !ok || seen[root] {
			continue
		}
		seen[root] = true
		pathspecs = append(pathspecs, root)
	}
	return pathspecs
}

// RuntimeArtifactPaths returns deduplicated pathspecs for runtime artifacts in the
// current uncommitted work. Callers can pass the result to git reset after git add
// to keep generated state out of safety-net commits.
func (s *UncommittedWorkStatus) RuntimeArtifactPaths() []string {
	paths := append(append([]string{}, s.ModifiedFiles...), s.UntrackedFiles...)
	return RuntimeArtifactPathspecs(paths)
}

// NonRuntimePaths returns uncommitted paths that are not covered by the runtime
// artifact policy. Recovery checks use this to ignore generated tool state while
// still blocking on real source changes.
func (s *UncommittedWorkStatus) NonRuntimePaths() []string {
	var paths []string
	paths = append(paths, s.UnmergedFiles...)
	for _, f := range append(append([]string{}, s.ModifiedFiles...), s.UntrackedFiles...) {
		if !isGasTownRuntimePath(f) {
			paths = append(paths, f)
		}
	}
	return paths
}

// NonRuntimeNonSkewPaths is NonRuntimePaths with index-skew files (see
// IndexSkewFiles) also removed. Used to size the diagnostic message when
// CleanExcludingRuntimeAndIndexSkew reports dirt, so the reported count
// reflects only the files actually blocking reuse.
func (s *UncommittedWorkStatus) NonRuntimeNonSkewPaths(g *Git) []string {
	skewFiles := s.IndexSkewFiles(g)
	skew := make(map[string]bool, len(skewFiles))
	for _, f := range skewFiles {
		skew[f] = true
	}
	var paths []string
	for _, f := range s.NonRuntimePaths() {
		if !skew[f] {
			paths = append(paths, f)
		}
	}
	return paths
}

// CleanExcludingRuntime returns true if the only uncommitted changes are
// runtime artifacts covered by the centralized exclusion policy.
// Used by gt done to avoid blocking completion on toolchain-managed files.
//
// Note: UnpushedCommits and StashCount are intentionally NOT checked here. This
// function only evaluates whether uncommitted *file* changes are runtime artifacts.
// Unpushed commits represent committed (but not yet pushed) work, and stashes
// survive worktree deletion — both are handled separately and shouldn't block
// completion on runtime-only dirt (gas-7vg).
func (s *UncommittedWorkStatus) CleanExcludingRuntime() bool {
	if len(s.UnmergedFiles) > 0 {
		return false
	}

	for _, f := range s.ModifiedFiles {
		if !isGasTownRuntimePath(f) {
			return false
		}
	}

	for _, f := range s.UntrackedFiles {
		if !isGasTownRuntimePath(f) {
			return false
		}
	}

	return true
}

// CleanExcludingRuntimeAndIndexSkew is CleanExcludingRuntime plus one more
// exclusion: staged-only files whose content already matches a comparison ref
// this checkout is at or behind (IndexSkewFiles). Used by polecat seat-reuse
// dirt checks, where a shared-.repo.git checkout's index can describe a commit
// that moved out from under it — content already known elsewhere, not real
// unsaved work (gt-ui2x). gt done and every other uncommitted-work consumer
// keep using CleanExcludingRuntime unchanged, so this relaxation is scoped to
// reuse eligibility only.
func (s *UncommittedWorkStatus) CleanExcludingRuntimeAndIndexSkew(g *Git) bool {
	if len(s.UnmergedFiles) > 0 {
		return false
	}

	skewFiles := s.IndexSkewFiles(g)
	skew := make(map[string]bool, len(skewFiles))
	for _, f := range skewFiles {
		skew[f] = true
	}

	for _, f := range s.ModifiedFiles {
		if !isGasTownRuntimePath(f) && !skew[f] {
			return false
		}
	}

	for _, f := range s.UntrackedFiles {
		if !isGasTownRuntimePath(f) {
			return false
		}
	}

	return true
}

// String returns a human-readable summary of uncommitted work.
func (s *UncommittedWorkStatus) String() string {
	var issues []string
	if s.HasUncommittedChanges {
		issues = append(issues, fmt.Sprintf("%d uncommitted change(s)", len(s.ModifiedFiles)+len(s.UntrackedFiles)+len(s.UnmergedFiles)))
	}
	if len(s.UnmergedFiles) > 0 {
		issues = append(issues, fmt.Sprintf("unmerged: %s", strings.Join(s.UnmergedFiles, ", ")))
	}
	if s.StashCount > 0 {
		issues = append(issues, fmt.Sprintf("%d stash(es)", s.StashCount))
	}
	if s.UnpushedCommits > 0 {
		issues = append(issues, fmt.Sprintf("%d unpushed commit(s)", s.UnpushedCommits))
	}
	if len(issues) == 0 {
		return "clean"
	}
	return strings.Join(issues, ", ")
}

// indexSkewComparisonRefs returns the locally-resolvable refs to compare
// staged-only paths against when classifying index skew: origin's default
// branch tracking ref, then the local branch of the same name. It makes no
// network calls (no fetch) — only refs already known to this clone count, so
// a polecat that has never fetched fails closed with no candidates rather
// than reaching out over the network on every dirt check.
//
// A candidate ref counts only while HEAD is at or behind it. That is the
// checkout-skew shape the exemption exists for: a ref that moved out from
// under a dormant checkout, so the index describes where the ref went. A
// checkout HEAD is *ahead* of is a different shape entirely — content
// matching that ref is then a deliberate edit back to it, most often a staged
// revert of work this checkout carries (gt-ycvx), and the ref is dropped so
// those paths keep blocking.
func (g *Git) indexSkewComparisonRefs() []string {
	branch := g.RemoteDefaultBranch()
	if branch == "" {
		return nil
	}
	var refs []string
	for _, ref := range []string{"origin/" + branch, branch} {
		if _, err := g.run("rev-parse", "--verify", "--quiet", ref); err != nil {
			continue
		}
		if atOrBehind, err := g.IsAncestor("HEAD", ref); err != nil || !atOrBehind {
			continue
		}
		refs = append(refs, ref)
	}
	return refs
}

// classifyIndexSkew narrows paths down to the ones whose staged index blob
// is affirmatively confirmed identical to the same path's blob on a
// qualifying comparison ref (see indexSkewComparisonRefs for which refs
// qualify and why HEAD's position relative to them decides it).
//
// This compares blob shas directly (git ls-files -s for the index side, git
// ls-tree <ref> for the ref side) instead of inferring identity from a
// path's *absence* in `git diff --name-only` output. That absence-based
// check failed open: a pathspec that matched nothing at all — because the
// filename carries glob/magic characters, or because Status() had left it
// still C-quoted — produced empty diff output too, so a real staged edit
// with such a name was silently classified as skew (gt-ui2x). Every
// pathspec here is prefixed with the `:(literal)` magic so glob characters
// in a real filename are never reinterpreted.
//
// It never errors: a ref or path that fails to resolve is simply skipped,
// and a path whose match cannot be affirmatively confirmed against any ref —
// including a staged deletion, which has no index blob to confirm at all —
// is left out of the result entirely. Fail closed: the same file stays a
// blocker.
func (g *Git) classifyIndexSkew(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	refs := g.indexSkewComparisonRefs()
	if len(refs) == 0 {
		return nil
	}

	indexBlobs := g.lsFilesStagedBlobs(paths)

	unresolved := make([]string, 0, len(paths))
	for _, p := range paths {
		if indexBlobs[p] != "" {
			unresolved = append(unresolved, p)
		}
	}

	confirmed := make(map[string]bool, len(unresolved))
	for _, ref := range refs {
		if len(unresolved) == 0 {
			break
		}
		refBlobs := g.lsTreeBlobs(ref, unresolved)
		var stillUnresolved []string
		for _, p := range unresolved {
			if sha := refBlobs[p]; sha != "" && sha == indexBlobs[p] {
				confirmed[p] = true
			} else {
				stillUnresolved = append(stillUnresolved, p)
			}
		}
		unresolved = stillUnresolved
	}

	var skew []string
	for _, p := range paths {
		if confirmed[p] {
			skew = append(skew, p)
		}
	}
	return skew
}

// literalPathspecs wraps each path in the `:(literal)` pathspec magic so
// glob/magic characters in a real filename (`?`, `*`, `[`, a leading `:`)
// are matched as literal bytes rather than reinterpreted by git.
func literalPathspecs(paths []string) []string {
	specs := make([]string, len(paths))
	for i, p := range paths {
		specs[i] = ":(literal)" + p
	}
	return specs
}

// lsFilesStagedBlobs returns each path's staged (index) blob sha, keyed by
// path (git ls-files -s -z). A path this cannot resolve — including a
// staged deletion, which is in the index status but not the index tree — is
// simply absent from the result.
func (g *Git) lsFilesStagedBlobs(paths []string) map[string]string {
	args := append([]string{"ls-files", "-s", "-z", "--"}, literalPathspecs(paths)...)
	out, err := g.runOutput(args...)
	if err != nil {
		return nil
	}
	blobs := make(map[string]string, len(paths))
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		meta, path, ok := strings.Cut(record, "\t")
		if !ok {
			continue
		}
		// Format: "<mode> <sha> <stage>".
		fields := strings.Fields(meta)
		if len(fields) < 2 {
			continue
		}
		blobs[path] = fields[1]
	}
	return blobs
}

// lsTreeBlobs returns each path's blob sha on ref, keyed by path (git
// ls-tree -z <ref>). A path absent from ref is simply absent from the
// result.
func (g *Git) lsTreeBlobs(ref string, paths []string) map[string]string {
	args := append([]string{"ls-tree", "-z", ref, "--"}, literalPathspecs(paths)...)
	out, err := g.runOutput(args...)
	if err != nil {
		return nil
	}
	blobs := make(map[string]string, len(paths))
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		meta, path, ok := strings.Cut(record, "\t")
		if !ok {
			continue
		}
		// Format: "<mode> <type> <sha>".
		fields := strings.Fields(meta)
		if len(fields) < 3 {
			continue
		}
		blobs[path] = fields[2]
	}
	return blobs
}

// CheckUncommittedWork performs a comprehensive check for uncommitted work.
func (g *Git) CheckUncommittedWork() (*UncommittedWorkStatus, error) {
	return g.checkUncommittedWork(g.UnpushedCommits)
}

// CheckUncommittedWorkLocal is CheckUncommittedWork without the network: the
// status and stash facts are identical, and the unpushed-commit count is
// derived from local remote-tracking refs rather than an ls-remote against the
// remote (see BranchPreservationStatusLocal). It is the probe a caller uses
// when it does this once per seat in the town.
func (g *Git) CheckUncommittedWorkLocal() (*UncommittedWorkStatus, error) {
	return g.checkUncommittedWork(g.UnpushedCommitsLocal)
}

// CheckUncommittedWorkLocalFailClosed is CheckUncommittedWorkLocal built on
// UnpushedCommitsLocalFailClosed instead of UnpushedCommitsLocal: an
// unresolvable comparison reports the branch as having unpreserved work
// rather than as clean. Use this where the caller is about to discard the
// worktree (feed a fresh polecat, reuse the seat) if it looks clean;
// UnpushedCommitsLocal's 0-on-no-evidence reading stays correct for callers
// that only report a count.
func (g *Git) CheckUncommittedWorkLocalFailClosed() (*UncommittedWorkStatus, error) {
	return g.checkUncommittedWork(g.UnpushedCommitsLocalFailClosed)
}

// checkUncommittedWork is the shared body. The unpushed-commit count is the
// only fact whose source differs between the live and local probes, so it is
// the only thing passed in — the other three checks cannot drift apart.
func (g *Git) checkUncommittedWork(unpushedCommits func() (int, error)) (*UncommittedWorkStatus, error) {
	status := &UncommittedWorkStatus{}

	// Check git status
	gitStatus, err := g.Status()
	if err != nil {
		return nil, fmt.Errorf("checking git status: %w", err)
	}
	status.HasUncommittedChanges = !gitStatus.Clean
	status.ModifiedFiles = append(gitStatus.Modified, gitStatus.Added...)
	status.ModifiedFiles = append(status.ModifiedFiles, gitStatus.Deleted...)
	status.UntrackedFiles = gitStatus.Untracked
	status.UnmergedFiles = gitStatus.Unmerged
	status.StagedOnly = gitStatus.StagedOnly

	// Check stashes
	stashCount, err := g.StashCount()
	if err != nil {
		return nil, fmt.Errorf("checking stashes: %w", err)
	}
	status.StashCount = stashCount

	// Check unpushed commits
	unpushed, err := unpushedCommits()
	if err != nil {
		return nil, fmt.Errorf("checking unpushed commits: %w", err)
	}
	status.UnpushedCommits = unpushed

	return status, nil
}

// BranchPushedToRemote checks if a branch has been pushed to the remote.
// Returns (pushed bool, unpushedCount int, err).
// This handles polecat branches that don't have upstream tracking configured.
func (g *Git) BranchPushedToRemote(localBranch, remote string) (bool, int, error) {
	status, err := g.BranchPreservationStatus(localBranch, remote, nil)
	if err != nil {
		return false, 0, err
	}
	return status.Preserved, status.UnpreservedPatchCount, nil
}

// PrunedBranch represents a local branch that was pruned (or would be pruned in dry-run).
type PrunedBranch struct {
	Name   string // Branch name (e.g., "polecat/rictus-mkb0vq9f")
	Reason string // Why it was pruned: "merged", "no-remote", "no-remote-merged"
}

// PruneStaleBranches finds and deletes local branches matching a pattern that are
// stale — either fully merged to the default branch or whose remote tracking branch
// no longer exists (indicating the remote branch was deleted after merge).
//
// This addresses cross-clone branch accumulation: when polecats push branches to
// origin, other clones create local tracking branches via git fetch. After the
// remote branch is deleted (post-merge), git fetch --prune removes the remote
// tracking ref but the local branch persists indefinitely.
//
// Safety: never deletes the current branch or the default branch (main/master).
// Uses git branch -d (not -D), so only fully-merged branches are deleted.
func (g *Git) PruneStaleBranches(pattern string, dryRun bool) ([]PrunedBranch, error) {
	if pattern == "" {
		pattern = "polecat/*"
	}

	// Get current branch to avoid deleting it
	currentBranch, _ := g.CurrentBranch()
	defaultBranch := g.RemoteDefaultBranch()

	// List all local branches matching the pattern
	branches, err := g.ListBranches(pattern)
	if err != nil {
		return nil, fmt.Errorf("listing branches: %w", err)
	}

	var pruned []PrunedBranch
	for _, branch := range branches {
		branch = strings.TrimSpace(branch)
		if branch == "" || branch == currentBranch || branch == defaultBranch {
			continue
		}

		// Check if the remote tracking branch still exists
		hasRemote, err := g.RemoteTrackingBranchExists("origin", branch)
		if err != nil {
			continue // Skip on error, don't fail the whole operation
		}

		// Check if the branch is merged to the default branch
		merged, err := g.IsAncestor(branch, "origin/"+defaultBranch)
		if err != nil {
			// If we can't determine merge status, only prune if remote is gone
			if hasRemote {
				continue
			}
			// Remote gone and can't check merge status — skip to be safe
			continue
		}

		var reason string
		if merged && !hasRemote {
			reason = "no-remote-merged"
		} else if merged {
			reason = "merged"
		} else if !hasRemote {
			reason = "no-remote"
		} else {
			continue // Branch has remote and is not merged — keep it
		}

		if !dryRun {
			// Use -d (not -D) for safety — only deletes fully merged branches.
			// For "no-remote" branches that aren't merged, -d will fail safely.
			if err := g.DeleteBranch(branch, false); err != nil {
				// If -d fails (not merged), skip this branch
				continue
			}
		}

		pruned = append(pruned, PrunedBranch{
			Name:   branch,
			Reason: reason,
		})
	}

	return pruned, nil
}

// SubmoduleChange represents a changed submodule pointer between two refs.
type SubmoduleChange struct {
	Path   string // Submodule path relative to repo root
	OldSHA string // Previous commit SHA (or empty for new submodule)
	NewSHA string // New commit SHA (or empty for removed submodule)
	URL    string // Submodule remote URL from .gitmodules
}

// InitSubmodules initializes and updates submodules if .gitmodules exists.
// This is a no-op for repos without submodules.
//
// If referencePath is non-empty and contains submodules, --reference is used
// to share git objects from a local clone instead of fetching from remote.
// This makes submodule init near-instant for large submodules (e.g. 655MB gitlabhq).
func InitSubmodules(repoPath string, referencePath ...string) error {
	if !hasTrackedGitmodules(repoPath) {
		return nil
	}
	if err := EnsureSafeMutationWorkDir(repoPath); err != nil {
		return err
	}

	args := []string{"-C", repoPath, "submodule", "update", "--init", "--recursive"}

	// Use --reference to share objects from a local clone (avoids remote fetch)
	if len(referencePath) > 0 && referencePath[0] != "" {
		refPath := referencePath[0]
		if hasTrackedGitmodules(refPath) {
			args = append(args, "--reference", refPath)
		}
	}

	cmd := exec.Command("git", args...)
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("initializing submodules: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// hasTrackedGitmodules checks whether .gitmodules exists on disk AND is tracked
// by git. After a submodule-to-monorepo migration, .gitmodules may linger as an
// untracked file (e.g., in a stale mayor/rig clone or bare repo worktree) even
// though it has been removed from the repository. Checking only os.Stat would
// incorrectly trigger submodule init on these stale artifacts.
func hasTrackedGitmodules(repoPath string) bool {
	gitmodules := filepath.Join(repoPath, ".gitmodules")
	if _, err := os.Stat(gitmodules); os.IsNotExist(err) {
		return false
	}
	// Verify .gitmodules is actually tracked in the index.
	cmd := exec.Command("git", "-C", repoPath, "ls-files", "--error-unmatch", ".gitmodules")
	return cmd.Run() == nil
}

// InitSparseCheckout initializes sparse checkout with cone mode and configures
// the given paths. If paths is empty, initializes with cone mode only (checkout root files).
func InitSparseCheckout(repoPath string, paths []string) error {
	if err := EnsureSafeMutationWorkDir(repoPath); err != nil {
		return err
	}

	// Initialize sparse checkout in cone mode
	cmd := exec.Command("git", "-C", repoPath, "sparse-checkout", "init", "--cone")
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("initializing sparse checkout: %s", strings.TrimSpace(stderr.String()))
	}
	if len(paths) > 0 {
		args := append([]string{"-C", repoPath, "sparse-checkout", "set"}, paths...)
		cmd = exec.Command("git", args...)
		util.SetDetachedProcessGroup(cmd)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("setting sparse checkout paths: %s", strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}

// SubmoduleChanges detects submodule pointer changes between two refs.
// Returns nil if no submodules changed or if the repo has no submodules.
func (g *Git) SubmoduleChanges(base, head string) ([]SubmoduleChange, error) {
	// git diff --raw shows mode 160000 for gitlink (submodule) entries
	out, err := g.run("diff", "--raw", base, head)
	if err != nil {
		return nil, fmt.Errorf("diffing for submodule changes: %w", err)
	}
	if out == "" {
		return nil, nil
	}

	var changes []SubmoduleChange
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: :oldmode newmode oldsha newsha status\tpath
		// Submodule entries have mode 160000
		if !strings.Contains(line, "160000") {
			continue
		}
		// Parse the raw diff line
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		path := strings.TrimSpace(parts[1])
		// Skip .claude/ paths — Claude Code creates worktrees under
		// .claude/worktrees/ with .git files (worktree pointers) that git
		// reports as gitlinks. These are not real submodules. (gt-dg7)
		if strings.HasPrefix(path, ".claude/") {
			continue
		}
		fields := strings.Fields(parts[0])
		if len(fields) < 5 {
			continue
		}
		oldSHA := fields[2]
		newSHA := fields[3]
		// Null SHAs (all zeros) indicate added/removed submodules
		if strings.Repeat("0", len(oldSHA)) == oldSHA {
			oldSHA = ""
		}
		if strings.Repeat("0", len(newSHA)) == newSHA {
			newSHA = ""
		}

		change := SubmoduleChange{
			Path:   path,
			OldSHA: oldSHA,
			NewSHA: newSHA,
		}

		// Try to get the submodule URL from .gitmodules on the head ref
		url, urlErr := g.submoduleURL(head, path)
		if urlErr == nil {
			change.URL = url
		}

		changes = append(changes, change)
	}
	return changes, nil
}

// submoduleURL reads the URL for a submodule from .gitmodules at a given ref.
// Uses git config -f to parse the file correctly regardless of field ordering.
func (g *Git) submoduleURL(ref, submodulePath string) (string, error) {
	// Write .gitmodules from the ref to a temp file so we can use git config -f
	content, err := g.run("show", ref+":.gitmodules")
	if err != nil {
		return "", err
	}
	tmpFile, err := os.CreateTemp("", "gitmodules-*")
	if err != nil {
		return "", fmt.Errorf("creating temp file for .gitmodules: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("writing temp .gitmodules: %w", err)
	}
	tmpFile.Close()

	// List all submodule.<name>.path entries to find the section matching our path
	cmd := exec.Command("git", "config", "-f", tmpFile.Name(), "--get-regexp", `^submodule\..*\.path$`)
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("reading submodule paths from .gitmodules: %w", err)
	}

	var sectionName string
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: submodule.<name>.path <value>
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[1]) == submodulePath {
			key := parts[0]
			key = strings.TrimPrefix(key, "submodule.")
			key = strings.TrimSuffix(key, ".path")
			sectionName = key
			break
		}
	}
	if sectionName == "" {
		return "", fmt.Errorf("submodule URL not found for path %s", submodulePath)
	}

	// Get the URL for this section
	urlCmd := exec.Command("git", "config", "-f", tmpFile.Name(), "--get", "submodule."+sectionName+".url")
	util.SetDetachedProcessGroup(urlCmd)
	var urlOut bytes.Buffer
	urlCmd.Stdout = &urlOut
	if err := urlCmd.Run(); err != nil {
		return "", fmt.Errorf("reading URL for submodule %s: %w", sectionName, err)
	}
	url := strings.TrimSpace(urlOut.String())
	if url == "" {
		return "", fmt.Errorf("submodule URL not found for path %s", submodulePath)
	}
	return url, nil
}

// PushSubmoduleCommit pushes a specific commit SHA from a submodule to its remote.
// The submodulePath is relative to the repo working directory.
// The commit must exist in the submodule's object store (shared via .repo.git/modules/).
func (g *Git) PushSubmoduleCommit(submodulePath, sha, remote string) error {
	absPath := filepath.Join(g.workDir, submodulePath)
	// Detect the remote's default branch (don't assume main)
	defaultBranch, err := submoduleDefaultBranch(absPath, remote)
	if err != nil {
		return fmt.Errorf("detecting default branch for submodule %s: %w", submodulePath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", absPath, "push", remote, sha+":refs/heads/"+defaultBranch)
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("pushing submodule %s timed out after %v (remote may be unreachable)", submodulePath, pushTimeout)
		}
		abbrev := sha
		if len(abbrev) > 8 {
			abbrev = abbrev[:8]
		}
		return fmt.Errorf("pushing submodule %s commit %s: %s", submodulePath, abbrev, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// submoduleDefaultBranch detects the default branch of a submodule's remote.
// Tries local refs first to avoid network round-trips, falling back to remote queries.
func submoduleDefaultBranch(submodulePath, remote string) (string, error) {
	// Try local symbolic-ref first (no network, fastest)
	symCmd := exec.Command("git", "-C", submodulePath, "symbolic-ref", "refs/remotes/"+remote+"/HEAD")
	util.SetDetachedProcessGroup(symCmd)
	if symOut, err := symCmd.Output(); err == nil {
		ref := strings.TrimSpace(string(symOut))
		// refs/remotes/origin/HEAD -> refs/remotes/origin/main -> main
		if parts := strings.Split(ref, "/"); len(parts) > 0 {
			branch := parts[len(parts)-1]
			if branch != "" {
				return branch, nil
			}
		}
	}

	// Try local tracking refs (no network)
	for _, candidate := range []string{"main", "master"} {
		check := exec.Command("git", "-C", submodulePath, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remote+"/"+candidate)
		util.SetDetachedProcessGroup(check)
		if check.Run() == nil {
			return candidate, nil
		}
	}

	// Fallback: network query via ls-remote
	for _, candidate := range []string{"main", "master"} {
		check := exec.Command("git", "-C", submodulePath, "ls-remote", "--exit-code", remote, "refs/heads/"+candidate)
		util.SetDetachedProcessGroup(check)
		if check.Run() == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not determine default branch for remote %s", remote)
}
