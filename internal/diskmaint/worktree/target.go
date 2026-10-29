package worktree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// refsHeads is the prefix a `git worktree list --porcelain` branch line
// carries, and the ref namespace a push is checked against.
const refsHeads = "refs/heads/"

var (
	// errNoRunner reports a target with no git runner: nothing can be read, so
	// a plan refuses rather than reporting a town with no worktrees.
	errNoRunner = errors.New("no git runner configured")
	// errNoRigs reports a target that names no rig to look under. A rig with no
	// crew directory is skipped, but no rigs at all is a configuration mistake,
	// not an empty town.
	errNoRigs = errors.New("no rigs configured")
	// errNotWired reports an apply to a target that only reads: it plans, and it
	// runs no command that changes a repository.
	errNotWired = errors.New("worktree apply: this target removes nothing; the plan is a dry run")
	// errRedirected reports a git repository location variable in the
	// environment. The runner passes the process environment to git, so git
	// would read another repository than the crew checkout it was asked about.
	errRedirected = errors.New("a git repository location variable is set")
	// errWorktreeList reports a failed worktree listing.
	errWorktreeList = errors.New("git worktree list failed")
	// errStatus reports an unreadable working tree state.
	errStatus = errors.New("git status failed")
	// errLastCommit reports an unreadable last commit time, from the command or
	// from output that is not a Unix timestamp.
	errLastCommit = errors.New("git log -1 did not give a commit time")
	// errNoHead reports a worktree list entry that named no HEAD, so a push
	// cannot be checked against anything.
	errNoHead = errors.New("the worktree list named no HEAD")
	// errPushed reports a failed remote branch lookup.
	errPushed = errors.New("git ls-remote failed")
	// errLanded reports a failed search for the landing commit.
	errLanded = errors.New("git log origin/main failed")
	// errCrewUnreadable reports a crew directory that could not be listed.
	errCrewUnreadable = errors.New("the crew directory is unreadable")
	// errStatCheckout reports a directory whose .git entry could not be
	// inspected, so whether it is a checkout is unknown rather than no.
	errStatCheckout = errors.New("cannot tell whether the directory holds a checkout")
)

// Compile-time check that the worktree target satisfies the diskmaint contract.
var _ diskmaint.Target = (*Target)(nil)

// redirectVars are the environment variables that make git read a repository,
// a remote, or a configuration other than the one it would read for the crew
// checkout. A url.<base>.insteadOf in a config file the process names can point
// origin elsewhere, so the config locations count as well as the repository
// ones. The runner appends to the process environment, so a plan refuses when
// any of them is set rather than read the wrong clone.
var redirectVars = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_COMMON_DIR":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_NAMESPACE":                    true,
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_CONFIG_GLOBAL":                true,
	"GIT_CONFIG_SYSTEM":                true,
	"GIT_CONFIG_NOSYSTEM":              true,
}

// Config is what the worktree target needs to discover worktrees and read their
// state.
type Config struct {
	// Town is the town root; each rig's worktrees sit under <Town>/<rig>/crew.
	Town string
	// Rigs are the rigs to look under.
	Rigs []string
	// Run runs git. A test passes a fake that records calls and returns canned
	// output, so no test runs real git.
	Run diskmaint.Runner
	// Environ returns the environment as NAME=value entries; nil reads the
	// process environment. A test passes a fixed list so the result does not
	// depend on the host.
	Environ func() []string
	// Ledger records an apply. The planner never writes to it.
	Ledger *diskmaint.Ledger
}

// Target discovers crew worktrees and plans the removal of the stale ones.
type Target struct {
	cfg Config
}

// NewTarget returns a Target that reads the crew directories of cfg.Rigs.
func NewTarget(cfg Config) *Target {
	return &Target{cfg: cfg}
}

// Name is the target's name in plans.
func (t *Target) Name() string { return planTarget }

// Measure reports how many worktrees the rigs' crew directories hold. A
// worktree's size is not what this target accounts for, so Used and Capacity
// stay 0 rather than claiming the worktrees hold nothing. A crew directory or
// listing that cannot be read is an error, never a count that leaves it out.
func (t *Target) Measure(ctx context.Context) (diskmaint.Measurement, error) {
	found, readErrs, err := t.discover(ctx)
	if err != nil {
		return diskmaint.Measurement{}, fmt.Errorf("measure: %w", err)
	}
	if len(readErrs) > 0 {
		return diskmaint.Measurement{}, fmt.Errorf("measure: %s", strings.Join(readErrs, "; "))
	}
	return diskmaint.Measurement{
		Detail: fmt.Sprintf("%d crew worktrees in the rigs' crew directories; bytes are not measured", len(found)),
	}, nil
}

// Plan discovers the worktrees under the rigs' crew directories, reads each
// one's state with git, and returns BuildPlan's decision for them. A failed
// listing or an unreadable crew directory refuses the whole plan, because it
// could hide a worktree that must stay; a worktree whose own reads fail is kept
// with the error that kept it. Plan changes nothing.
func (t *Target) Plan(ctx context.Context, p diskmaint.Policy) (diskmaint.Plan, error) {
	plan := diskmaint.Plan{Target: planTarget}
	found, readErrs, err := t.discover(ctx)
	if err != nil {
		plan.Refused = err.Error()
		return plan, nil
	}
	ws := make([]Worktree, 0, len(found))
	for _, c := range found {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		ws = append(ws, t.read(ctx, c))
	}
	return BuildPlan(ws, p.Now, readErrs), nil
}

// Apply refuses the plan and runs no command: this target reads worktree state,
// it does not change a worktree, so a caller that ignored a dry run cannot lose
// one.
func (t *Target) Apply(context.Context, diskmaint.Plan) (diskmaint.Result, error) {
	return diskmaint.Result{Target: planTarget}, errNotWired
}

// checkout is one worktree as `git worktree list --porcelain` reported it.
type checkout struct {
	Path, Rig, Head, Branch string // Branch is the short name, e.g. "polecat/x"
}

// discover lists every worktree under <Town>/<rig>/crew for each rig. Only a
// directory that holds a git checkout is asked; a rig with no crew directory is
// skipped, and a crew directory or listing that cannot be read becomes a read
// error, which refuses the plan. The results are merged by worktree path, since
// the worktrees of one clone appear in each other's listings, and are ordered
// by path so the same town gives the same plan.
func (t *Target) discover(ctx context.Context) ([]checkout, []string, error) {
	if t.cfg.Run == nil {
		return nil, nil, errNoRunner
	}
	if len(t.cfg.Rigs) == 0 {
		return nil, nil, errNoRigs
	}
	if names := redirectedBy(t.environ()); len(names) > 0 {
		return nil, nil, fmt.Errorf("%w: %s", errRedirected, strings.Join(names, ", "))
	}
	byPath := map[string]checkout{}
	var readErrs []string
	for _, rig := range t.cfg.Rigs {
		parent := filepath.Join(t.cfg.Town, rig, crewDir)
		dirs, err := crewCheckouts(parent)
		if err != nil {
			readErrs = append(readErrs, fmt.Sprintf("%s: %v", parent, err))
			continue
		}
		for _, dir := range dirs {
			out, err := t.git(ctx, dir, "worktree", "list", "--porcelain")
			if err != nil {
				readErrs = append(readErrs, fmt.Sprintf("%s: %v", dir, errWorktreeList))
				continue
			}
			for _, c := range parseWorktrees(out) {
				c.Rig = rig
				byPath[c.Path] = c
			}
		}
	}
	found := make([]checkout, 0, len(byPath))
	for _, c := range byPath {
		found = append(found, c)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	return found, readErrs, nil
}

// environ is the environment the target checks for redirecting variables.
func (t *Target) environ() []string {
	if t.cfg.Environ != nil {
		return t.cfg.Environ()
	}
	return os.Environ()
}

// redirectedBy returns the sorted names of the redirecting variables that env
// sets. Only names are returned: a value can be a path a reader should not
// echo, and the name is enough to act on.
func redirectedBy(env []string) []string {
	var names []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if redirectVars[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// crewCheckouts returns the directories directly under parent that hold a git
// checkout. A parent that does not exist holds none, which is not an error; any
// other failure to read it, or to look for a checkout inside it, is.
func crewCheckouts(parent string) ([]string, error) {
	entries, err := os.ReadDir(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errCrewUnreadable
	}
	var dirs []string
	for _, e := range entries {
		dir := filepath.Join(parent, e.Name())
		ok, err := isCheckout(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if ok {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

// isCheckout reports whether dir is a directory with a .git entry, which is a
// directory in a clone and a file in a linked worktree. A directory whose .git
// cannot be inspected is an error; reading it as "not a checkout" would drop a
// worktree from the plan.
func isCheckout(dir string) (bool, error) {
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errStatCheckout
	}
	if !info.IsDir() {
		return false, nil
	}
	_, err = os.Stat(filepath.Join(dir, ".git"))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, errStatCheckout
}

// parseWorktrees reads `git worktree list --porcelain` output, one block per
// worktree. A detached worktree has no branch line, so Branch stays empty and
// the plan keeps it as report-only.
func parseWorktrees(out []byte) []checkout {
	var worktrees []checkout
	var cur checkout
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			if cur.Path != "" {
				worktrees = append(worktrees, cur)
			}
			cur = checkout{Path: strings.TrimPrefix(line, "worktree ")}
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), refsHeads)
		}
	}
	if cur.Path != "" {
		worktrees = append(worktrees, cur)
	}
	return worktrees
}

// read gathers the state BuildPlan needs for one worktree. The first read that
// fails sets Err and stops, so a worktree with an unreadable state is kept
// rather than judged on the facts that happened to be readable.
func (t *Target) read(ctx context.Context, c checkout) Worktree {
	w := Worktree{Path: c.Path, Rig: c.Rig, Dir: filepath.Base(c.Path), Branch: c.Branch}

	out, err := t.git(ctx, c.Path, "status", "--porcelain")
	if err != nil {
		w.Err = errStatus.Error()
		return w
	}
	w.Dirty = countLines(out)

	out, err = t.git(ctx, c.Path, "log", "-1", "--format=%ct")
	if err != nil {
		w.Err = errLastCommit.Error()
		return w
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		w.Err = errLastCommit.Error()
		return w
	}
	w.LastCommit = time.Unix(seconds, 0).UTC()

	if c.Branch == "" {
		return w
	}
	if c.Head == "" {
		w.Err = errNoHead.Error()
		return w
	}
	out, err = t.git(ctx, c.Path, "ls-remote", "origin", refsHeads+c.Branch)
	if err != nil {
		w.Err = errPushed.Error()
		return w
	}
	w.Pushed = strings.EqualFold(remoteSHA(out), c.Head)

	// The landing subject is "land: <branch> (<sha>) onto main (...)". --grep only
	// narrows the walk: the subject check below decides, so a commit that quotes
	// a landing (a revert of one, say) never marks this branch landed, and the
	// branch check keeps a branch that is another's prefix from matching.
	out, err = t.git(ctx, c.Path, "log", "origin/main", "--fixed-strings", "--grep="+landingPrefix(c.Branch), "--format=%s")
	if err != nil {
		w.Err = errLanded.Error()
		return w
	}
	w.Landed = landsBranch(out, landingPrefix(c.Branch))
	return w
}

// landingPrefix is what a landing commit's subject begins with for branch:
// "land: <branch> (<sha>) onto <target> (...)". The branch is followed by " ("
// so a branch name that is a prefix of another cannot match.
func landingPrefix(branch string) string {
	return "land: " + branch + " ("
}

// landsBranch reports whether one of the subjects git log printed names
// branch's landing. Only the subject counts, because --grep matches the whole
// message: a revert carries the landing it reverts in its subject, quoted, and
// in its body, and either would read as landed work that is not on the target.
func landsBranch(out []byte, prefix string) bool {
	for _, subject := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(subject, prefix) {
			return true
		}
	}
	return false
}

// remoteSHA is the sha `git ls-remote` printed, or "" when the remote has no
// such ref.
func remoteSHA(out []byte) string {
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// countLines counts the non-empty lines of `git status --porcelain` output,
// which is one line per changed or untracked entry.
func countLines(out []byte) int {
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// git runs git with -C dir and args, and returns its stdout.
func (t *Target) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	argv := make([]string, 0, 2+len(args))
	argv = append(argv, "-C", dir)
	argv = append(argv, args...)
	return t.cfg.Run(ctx, nil, "git", argv...)
}
