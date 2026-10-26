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

var (
	repoguardBase string
	repoguardInit bool
)

var repoguardCmd = &cobra.Command{
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
revision has no allowlist yet, --init skips the growth check for this run, so a
repo can commit its first list; a base allowlist that exists is always
compared.

The guard is the portable form of gastown's tree-wide shell-script test, for
repos with no Go module of their own to carry one (gt-7zfre).

Exit codes:
  0 - clean
  1 - one or more violations, each printed by name`,
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	RunE:         runRepoGuardCommand,
}

func init() {
	rootCmd.AddCommand(repoguardCmd)
	repoguardCmd.Flags().StringVar(&repoguardBase, "base", "origin/main", "revision whose allowlist must not grow")
	repoguardCmd.Flags().BoolVar(&repoguardInit, "init", false, "skip the growth check when the base revision has no allowlist, so a repo can create its first list")
}

// runRepoGuardCommand wires the real seams: git for the tree and the base
// revision, the working tree for the allowlist.
func runRepoGuardCommand(cmd *cobra.Command, _ []string) error {
	root, err := repoGuardRoot()
	if err != nil {
		return err
	}
	readBase, err := repoGuardBaseReader(root, repoguardBase, repoguardInit)
	if err != nil {
		return err
	}
	return runRepoGuard(cmd.OutOrStdout(), repoGuardInputs{
		listFiles:         repoGuardListFiles(root),
		readAllowlist:     repoGuardReadAllowlist(filepath.Join(root, repoguardAllowlist)),
		readBaseAllowlist: readBase,
	})
}

// repoGuardInputs are the seams runRepoGuard reads. A nil readBaseAllowlist
// means there is no base allowlist to compare, the --init case.
type repoGuardInputs struct {
	listFiles         func() ([]string, error)
	readAllowlist     repoguard.Reader
	readBaseAllowlist repoguard.Reader
}

// runRepoGuard prints one violation per line and fails when there is any, so
// a gate can decide on the exit status alone.
func runRepoGuard(out io.Writer, in repoGuardInputs) error {
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

// repoGuardRoot is the repository the current directory belongs to.
func repoGuardRoot() (string, error) {
	out, err := repoGuardGit("", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("repoguard: not inside a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// repoGuardGit runs git and returns stdout with stderr in the error, since
// git explains a refusal on stderr and a bare exit status would hide why.
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
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// repoGuardListFiles lists every path git tracks, split on NUL so a path with
// a newline cannot become two paths.
func repoGuardListFiles(root string) func() ([]string, error) {
	return func() ([]string, error) {
		out, err := repoGuardGit(root, "ls-files", "-z")
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
		return repoguard.ParseAllowlist(f)
	}
}

// repoGuardBaseReader resolves base now and returns a reader of its allowlist.
// A missing allowlist at base is an error unless init asks for this one run to
// read it as empty: then the reader is nil, and CheckScripts skips the growth
// check so a repo can commit its first list.
func repoGuardBaseReader(root, base string, init bool) (repoguard.Reader, error) {
	if _, err := repoGuardGit(root, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return nil, fmt.Errorf("repoguard: base revision %s does not resolve: %w", base, err)
	}
	tree, err := repoGuardGit(root, "ls-tree", "-z", "--name-only", base, "--", repoguardAllowlist)
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 {
		if init {
			return nil, nil
		}
		return nil, fmt.Errorf("repoguard: no %s at %s; pass --init to create the first allowlist", repoguardAllowlist, base)
	}
	return func() ([]repoguard.Entry, error) {
		out, err := repoGuardGit(root, "show", base+":"+repoguardAllowlist)
		if err != nil {
			return nil, err
		}
		return repoguard.ParseAllowlist(bytes.NewReader(out))
	}, nil
}
