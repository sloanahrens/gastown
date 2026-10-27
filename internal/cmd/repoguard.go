package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/repoguard"
)

// repoguardAllowlist is the allowlist file gt repoguard reads at the repo root.
const repoguardAllowlist = ".repoguard-allowlist"

var repoguardCmd = newRepoGuardCommand(runRepoGuardCommand)

func init() {
	rootCmd.AddCommand(repoguardCmd)
}

// newRepoGuardCommand builds the command around its body, so a test can drive
// the flags and the RunE wiring without a repository.
func newRepoGuardCommand(run func(*cobra.Command, []string) error) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "repoguard",
		GroupID: GroupDiag,
		Short:   "Refuse new shell, Perl and Ruby scripts outside a shrinking allowlist",
		Long: `Hold the repository of the current directory to .repoguard-allowlist.

Every tracked .sh, .bash, .zsh, .pl or .rb file needs a line in
.repoguard-allowlist at the repository root: the repo-relative path, a tab,
and the reason the file cannot be Go. A line starting with # is a comment.
The list only shrinks: the guard fails when a script is unlisted, when a
listed path is no longer tracked, when a line has no reason, and when an entry
is not in the base allowlist.

--base names the revision to compare against (default origin/main). When that
revision has no allowlist yet, --init skips the growth check for this run and
says so on stderr, so a repo can commit its first list; a base allowlist that
exists is always compared.

The guard is the portable form of gastown's tree-wide shell-script test, for
repos with no Go module of their own to carry one (gt-7zfre).

Exit codes:
  0 - clean
  1 - one or more violations, each printed by name`,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE:         run,
	}
	cmd.Flags().String("base", "origin/main", "revision whose allowlist must not grow")
	cmd.Flags().Bool("init", false, "skip the growth check when the base revision has no allowlist, so a repo can create its first list")
	return cmd
}

// runRepoGuardCommand reads the flags and hands the run to the git the
// command really uses.
func runRepoGuardCommand(cmd *cobra.Command, _ []string) error {
	base, err := cmd.Flags().GetString("base")
	if err != nil {
		return err
	}
	initFlag, err := cmd.Flags().GetBool("init")
	if err != nil {
		return err
	}
	return repoGuardRun(cmd.OutOrStdout(), cmd.ErrOrStderr(), base, initFlag, repoGuardGit)
}

// repoGuardRun wires the inputs around run: git for the tree and the base
// revision, the working tree for the allowlist.
func repoGuardRun(out, warn io.Writer, base string, initFlag bool, run gitRunner) error {
	root, err := repoGuardRoot(run)
	if err != nil {
		return err
	}
	readBase, notice, err := repoGuardBaseReader(run, root, base, initFlag)
	if err != nil {
		return err
	}
	return runRepoGuard(out, warn, repoGuardInputs{
		listFiles:         repoGuardListFiles(run, root),
		readAllowlist:     repoGuardReadAllowlist(filepath.Join(root, repoguardAllowlist)),
		readBaseAllowlist: readBase,
		notice:            notice,
	})
}

// repoGuardInputs are the seams runRepoGuard reads. A nil readBaseAllowlist
// means there is no base allowlist to compare, the --init case.
type repoGuardInputs struct {
	listFiles         func() ([]string, error)
	readAllowlist     repoguard.Reader
	readBaseAllowlist repoguard.Reader
	// notice is printed to warn before any finding, so a run that skipped the
	// growth check does not look like one that passed it.
	notice string
}

// runRepoGuard prints one violation per line to out and fails when there is
// any, so a gate can decide on the exit status alone.
func runRepoGuard(out, warn io.Writer, in repoGuardInputs) error {
	if in.notice != "" {
		fmt.Fprintln(warn, in.notice)
	}
	findings, err := repoguard.CheckScripts(in.listFiles, in.readAllowlist, in.readBaseAllowlist)
	if err != nil {
		return err
	}
	for _, finding := range findings {
		fmt.Fprintln(out, finding)
	}
	switch len(findings) {
	case 0:
		return nil
	case 1:
		return errors.New("repoguard: 1 violation, listed above")
	default:
		return fmt.Errorf("repoguard: %d violations, listed above", len(findings))
	}
}

// gitRunner runs one git command in root and returns its stdout. A test
// injects a fake, so a git refusal is judged without a process.
type gitRunner func(root string, args ...string) ([]byte, error)

// repoGuardRoot is the repository the current directory belongs to.
func repoGuardRoot(run gitRunner) (string, error) {
	out, err := run("", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("repoguard: not inside a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// repoGuardGit runs git and returns stdout, with git's stderr folded into the
// error.
func repoGuardGit(root string, args ...string) ([]byte, error) {
	argv := args
	if root != "" {
		argv = append([]string{"-C", root}, args...)
	}
	cmd := exec.Command("git", argv...) //nolint:gosec // G204: fixed argv; the path is the caller's repository
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, repoGuardGitError(args, stderr.String(), err)
	}
	return out, nil
}

// repoGuardGitError names a failed git call and quotes what git said, since a
// bare exit status would hide why it refused.
func repoGuardGitError(args []string, stderr string, err error) error {
	if msg := strings.TrimSpace(stderr); msg != "" {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

// repoGuardListFiles lists every path git tracks, split on NUL so a path with
// a newline cannot become two paths.
func repoGuardListFiles(run gitRunner, root string) func() ([]string, error) {
	return func() ([]string, error) {
		out, err := run(root, "ls-files", "-z")
		if err != nil {
			return nil, err
		}
		var files []string
		for _, name := range bytes.Split(out, []byte{0}) {
			if len(name) > 0 {
				files = append(files, string(name))
			}
		}
		return files, nil
	}
}

// repoGuardReadAllowlist reads the allowlist at path. A missing file is no
// entries, so the scripts it would hold fail as unlisted, which names the
// file a repo adding the guard must write.
func repoGuardReadAllowlist(path string) repoguard.Reader {
	return func() ([]repoguard.Entry, error) {
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return repoguard.ParseRepoAllowlist(f)
	}
}

// repoGuardBaseReader resolves base now and returns a reader of its allowlist,
// nil when the base holds none. The notice says so, since a run that skips the
// growth check must not look like one that passed it.
func repoGuardBaseReader(run gitRunner, root, base string, init bool) (repoguard.Reader, string, error) {
	if err := repoGuardCheckBase(base); err != nil {
		return nil, "", err
	}
	if _, err := run(root, "rev-parse", "--verify", "--quiet", "--end-of-options", base+"^{commit}"); err != nil {
		return nil, "", fmt.Errorf("repoguard: base revision %s does not resolve: %w", base, err)
	}
	tree, err := run(root, "ls-tree", "-z", "--name-only", base, "--", repoguardAllowlist)
	if err != nil {
		return nil, "", err
	}
	compare, notice, err := repoGuardBasePlan(len(tree) > 0, init, base)
	if err != nil || !compare {
		return nil, notice, err
	}
	return func() ([]repoguard.Entry, error) {
		out, err := run(root, "show", base+":"+repoguardAllowlist)
		if err != nil {
			return nil, err
		}
		return repoguard.ParseRepoAllowlist(bytes.NewReader(out))
	}, "", nil
}

// repoGuardCheckBase refuses a --base git would parse as an option, since the
// value reaches rev-parse, ls-tree and show before any end-of-options marker.
func repoGuardCheckBase(base string) error {
	if base == "" {
		return errors.New("repoguard: --base is empty; name the revision whose allowlist must not grow")
	}
	if strings.HasPrefix(base, "-") {
		return fmt.Errorf("repoguard: --base %q starts with '-', which git would read as an option", base)
	}
	return nil
}

// repoGuardBasePlan decides what a run compares against: the base allowlist,
// no growth check with the notice that says why (--init only), or the error
// telling the author to pass --init.
func repoGuardBasePlan(baseHasAllowlist, init bool, base string) (compare bool, notice string, err error) {
	if baseHasAllowlist {
		return true, "", nil
	}
	if init {
		return false, fmt.Sprintf("repoguard: no %s at %s: --init skips the growth check for this run", repoguardAllowlist, base), nil
	}
	return false, "", fmt.Errorf("repoguard: no %s at %s; pass --init to create the first allowlist", repoguardAllowlist, base)
}
