package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/crewfresh"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/workspace"
)

// crewWorktreeTimeout bounds the fetch gt crew worktree makes. A remote that
// outlives it fails the command, so a session is never left waiting on one.
const crewWorktreeTimeout = 5 * time.Second

// crewWorktreeBase is the ref a new worktree's branch starts from: the tip the
// fetch just brought down.
const crewWorktreeBase = "origin/main"

// crewWorktreeTopicPattern is the shape a topic may take: lowercase letters,
// digits, dots and hyphens, opening with a letter or digit so a topic cannot
// start with a dot or a hyphen.
var crewWorktreeTopicPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

var crewWorktreeCmd = &cobra.Command{
	Use:   "worktree <topic>",
	Short: "Create a worktree beside your crew clone on a fresh branch",
	Long: `Fetch origin, then create <town>/<rig>/crew/<agent>-<topic> on the new
branch crew/<agent>/<topic>, cut from origin/main.

Run it from your own crew clone. The command refuses a path or a branch that
already exists, and prints the path it created:

  cd "$(gt crew worktree fix-flake)"`,
	Args: cobra.ExactArgs(1),
	RunE: runCrewWorktree,
}

func runCrewWorktree(cmd *cobra.Command, args []string) error {
	return crewWorktree(cmd.OutOrStdout(), args[0], crewWorktreeDeps{
		getwd:    os.Getwd,
		findTown: workspace.FindFromCwdOrError,
		role:     GetRoleWithContext,
		repo:     newCrewWorktreeRepo,
		eval:     filepath.EvalSymlinks,
		timeout:  crewWorktreeTimeout,
	})
}

// crewWorktreeGit is the git surface gt crew worktree runs through: the fetch,
// the two questions that decide whether the name is free, and the worktree it
// makes. newCrewWorktreeRepo builds the production value; a test passes a fake
// so no test runs git.
type crewWorktreeGit interface {
	Fetch(timeout time.Duration) error
	PathExists(path string) (bool, error)
	BranchExists(branch string) (bool, error)
	WorktreeAddFromRef(path, branch, startPoint string) error
}

// crewWorktreeDeps are the seams gt crew worktree runs through, so every
// refusal is covered without a live town, a crew clone or a remote.
type crewWorktreeDeps struct {
	getwd    func() (string, error)
	findTown func() (string, error)
	role     func(cwd, townRoot string) (RoleInfo, error)
	repo     func(root string) crewWorktreeGit
	// eval resolves a path through symlinks for the containment check on the
	// clone (crewCloneRoot). filepath.EvalSymlinks in production; a fake in a
	// test.
	eval    func(string) (string, error)
	timeout time.Duration
}

// crewWorktreeRepo is the production git surface. The fetch goes through
// crewfresh's git, whose deadline and prompt-free environment that package
// already pins; the questions and the creation use the clone's own *git.Git.
//
// lstat is the one filesystem read the name question makes. Nil is os.Lstat,
// which is what production runs; a test sets it so every answer — present,
// absent, and a stat that failed for another reason — is driven without the
// host filesystem.
type crewWorktreeRepo struct {
	clone *git.Git
	fresh crewfresh.Git
	lstat func(string) (os.FileInfo, error)
}

func newCrewWorktreeRepo(root string) crewWorktreeGit {
	return crewWorktreeRepo{clone: git.NewGit(root), fresh: crewfresh.NewGit(root)}
}

func (r crewWorktreeRepo) Fetch(timeout time.Duration) error { return r.fresh.Fetch(timeout) }

// PathExists reports whether path is already taken. A stat that fails for any
// reason other than absence is returned, so an unreadable parent refuses
// instead of passing as free.
func (r crewWorktreeRepo) PathExists(path string) (bool, error) {
	lstat := r.lstat
	if lstat == nil {
		lstat = os.Lstat
	}
	_, err := lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

func (r crewWorktreeRepo) BranchExists(branch string) (bool, error) {
	return r.clone.BranchExists(branch)
}

func (r crewWorktreeRepo) WorktreeAddFromRef(path, branch, startPoint string) error {
	return r.clone.WorktreeAddFromRef(path, branch, startPoint)
}

// crewWorktree fetches the crew clone that holds cwd and creates its sibling
// worktree for topic. Every lookup is a parameter so a test drives each refusal
// without a live town or a remote.
func crewWorktree(out io.Writer, topic string, deps crewWorktreeDeps) error {
	cwd, err := deps.getwd()
	if err != nil {
		return fmt.Errorf("read the current directory: %w", err)
	}
	townRoot, err := deps.findTown()
	if err != nil {
		return fmt.Errorf("find the town root: %w", err)
	}
	root, err := crewCloneRootResolved(cwd, townRoot, deps.eval)
	if err != nil {
		return err
	}
	info, err := deps.role(cwd, townRoot)
	if err != nil {
		return fmt.Errorf("read the crew role: %w", err)
	}
	if info.Role != RoleCrew {
		return fmt.Errorf("gt crew worktree runs from a crew clone, not as %s", info.Role)
	}
	path, branch, err := crewWorktreePlan(root, info.Polecat, topic)
	if err != nil {
		return err
	}

	g := deps.repo(root)
	// The two questions come before the fetch so a doomed name costs no
	// network call. A read that fails refuses: an unread answer is not a free
	// name.
	taken, err := g.PathExists(path)
	if err != nil {
		return fmt.Errorf("check whether %s exists: %w", path, err)
	}
	if taken {
		return fmt.Errorf("%s already exists", path)
	}
	taken, err = g.BranchExists(branch)
	if err != nil {
		return fmt.Errorf("check whether branch %s exists: %w", branch, err)
	}
	if taken {
		return fmt.Errorf("branch %s already exists", branch)
	}
	if err := g.Fetch(deps.timeout); err != nil {
		return fmt.Errorf("fetch origin in %s: %w", root, err)
	}
	if err := g.WorktreeAddFromRef(path, branch, crewWorktreeBase); err != nil {
		return fmt.Errorf("create the worktree at %s: %w", path, err)
	}
	fmt.Fprintln(out, path)
	return nil
}

// crewWorktreePlan returns the worktree path and branch for a topic. The path
// is the clone root's sibling named <agent>-<topic> and the branch is
// crew/<agent>/<topic>.
func crewWorktreePlan(root, agent, topic string) (path, branch string, err error) {
	if !crewWorktreeTopicPattern.MatchString(topic) {
		return "", "", fmt.Errorf("topic %q is not allowed: use lowercase letters, digits, dots and hyphens, starting with a letter or digit", topic)
	}
	if err := checkCrewWorktreeAgent(agent); err != nil {
		return "", "", err
	}
	return filepath.Join(filepath.Dir(root), agent+"-"+topic), "crew/" + agent + "/" + topic, nil
}

// checkCrewWorktreeAgent keeps the agent name from escaping the crew directory
// when it is joined into the worktree path.
func checkCrewWorktreeAgent(agent string) error {
	if agent == "" {
		return errors.New("the crew name is empty, so the worktree cannot be named")
	}
	if agent == "." || agent == ".." || strings.ContainsAny(agent, `/\`) {
		return fmt.Errorf("crew name %q cannot name a worktree", agent)
	}
	return nil
}
