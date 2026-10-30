package cmd

import (
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// DefaultAgentEmailDomain is the domain `gt config agent-email-domain` reports
// when the town sets none. It is the seat-local domain the seats' author
// addresses use, defined once in config.
const DefaultAgentEmailDomain = config.SeatLocalDomain

var commitCmd = &cobra.Command{
	Use:   "commit [flags] [-- git-commit-args...]",
	Short: "Git commit with automatic agent identity",
	Long: `Git commit wrapper that automatically sets git author identity for agents.

When run by an agent (GT_ROLE set), this command:
1. Detects the agent identity from environment variables
2. Authors as that seat's git identity: its bare agent name and its own
   address, <name>@gastown.local
3. Runs 'git commit' with the correct identity

The identity comes from the seat alone, so it does not depend on the worktree's
git config: a worktree with no user.email commits as the seat like any other.

Examples:
  gt commit -m "Fix bug"              # Commit as current agent
  gt commit -am "Quick fix"           # Stage all and commit
  gt commit -- --amend                # Amend last commit

Identity mapping:
  Agent: gastown/crew/jack  →  Name: jack
                                Email: jack@gastown.local

When run without GT_ROLE (human), passes through to git commit with no changes.`,
	RunE:               runCommit,
	DisableFlagParsing: true, // We'll parse flags ourselves to pass them to git
}

func init() {
	commitCmd.GroupID = GroupWork
	rootCmd.AddCommand(commitCmd)
}

func runCommit(cmd *cobra.Command, args []string) error {
	// Handle --help since DisableFlagParsing bypasses Cobra's help handling
	if helped, err := checkHelpFlag(cmd, args); helped || err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}

	// No agent identity (a human at a terminal): pass through to git commit
	identity := commitIdentity(os.Getenv, cwd)
	if identity == "" {
		return runGitCommit(args, git.SeatIdentity{})
	}

	return runGitCommit(args, seatForCommit(identity))
}

// seatForCommit returns the identity an agent's commit is authored as: the
// seat's bare agent name and its own address, <name>@gastown.local. Both come
// from the seat's compound identity alone, the same pair the seat is spawned with
// (config.GitIdentityEnv) and gt done's rework lease accepts, so the worktree's
// git config, which a linked worktree shares with the rig repository, decides
// nothing.
func seatForCommit(identity string) git.SeatIdentity {
	name := git.SeatIdentityFor(identity, "").Name
	return git.SeatIdentity{Name: name, Email: config.SeatLocalEmail(name)}
}

// commitIdentity returns the agent identity to author a commit as, or "" when
// the commit should pass through to plain git unchanged. Only GT_ROLE marks an
// agent session: detectSenderWith also infers an identity from GT_POLECAT,
// BD_ACTOR and the cwd path, which would author a human working in an agent
// directory (say ~/gt/gastown/crew/max) as that agent. A GT_ROLE that resolves
// to no agent address passes through too.
func commitIdentity(getenv func(string) string, cwd string) string {
	if getenv("GT_ROLE") == "" {
		return ""
	}
	identity := detectSenderWith(getenv, cwd)
	if identity == "overseer" {
		return ""
	}
	return identity
}

// runGitCommit executes git commit authored as seat. A zero seat runs plain
// git commit. Preserves git's exit code for proper wrapper behavior.
func runGitCommit(args []string, seat git.SeatIdentity) error {
	gitCmd := exec.Command("git", commitGitArgs(args, seat)...)
	// An inherited GIT_AUTHOR_NAME or GIT_AUTHOR_EMAIL outranks -c user.*, so
	// the author is set in the child's environment too, or the wrapper would
	// write the identity an agent shell happened to export (gt-tip6u).
	gitCmd.Env = withAuthorEnv(os.Environ(), seat)
	gitCmd.Stdin = os.Stdin
	gitCmd.Stdout = os.Stdout
	gitCmd.Stderr = os.Stderr

	if err := gitCmd.Run(); err != nil {
		// Preserve git's exit code for proper wrapper behavior
		if exitErr, ok := err.(*exec.ExitError); ok {
			return NewSilentExit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

// commitGitArgs prepends seat's config flags to the git commit arguments, so
// the committer carries the same identity as the author. A zero seat leaves
// the arguments bare.
func commitGitArgs(args []string, seat git.SeatIdentity) []string {
	gitArgs := make([]string, 0, 4+len(args))
	if seat.Name != "" {
		gitArgs = append(gitArgs, "-c", "user.name="+seat.Name)
	}
	if seat.Email != "" {
		gitArgs = append(gitArgs, "-c", "user.email="+seat.Email)
	}
	gitArgs = append(gitArgs, "commit")
	return append(gitArgs, args...)
}

// withAuthorEnv returns env with seat's author and committer variables set in
// place of any it already holds: git reads the first GIT_AUTHOR_NAME in the
// environment, so an appended second entry would leave the inherited one in
// force. The committer pair is set too, because GIT_COMMITTER_* outranks the
// -c user.* that commitGitArgs passes, so an inherited one would otherwise
// commit under another identity than the author.
func withAuthorEnv(env []string, seat git.SeatIdentity) []string {
	set := make(map[string]string, 4)
	if seat.Name != "" {
		set["GIT_AUTHOR_NAME"] = seat.Name
		set["GIT_COMMITTER_NAME"] = seat.Name
	}
	if seat.Email != "" {
		set["GIT_AUTHOR_EMAIL"] = seat.Email
		set["GIT_COMMITTER_EMAIL"] = seat.Email
	}
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		if k, _, found := strings.Cut(kv, "="); found {
			if _, replaced := set[k]; replaced {
				continue
			}
		}
		out = append(out, kv)
	}
	// Append in a fixed order, so the same seat always yields the same env.
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		if v, ok := set[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}
