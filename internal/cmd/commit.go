package cmd

import (
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/workspace"
)

// DefaultAgentEmailDomain is the default domain for agent git emails.
const DefaultAgentEmailDomain = "gastown.local"

var commitCmd = &cobra.Command{
	Use:   "commit [flags] [-- git-commit-args...]",
	Short: "Git commit with automatic agent identity",
	Long: `Git commit wrapper that automatically sets git author identity for agents.

When run by an agent (GT_ROLE set), this command:
1. Detects the agent identity from environment variables
2. Authors as that seat's git identity: its bare agent name and the user.email
   its worktree resolves
3. Runs 'git commit' with the correct identity

A worktree that resolves no user.email falls back to the agent's address at
the town's agent_email_domain (default: gastown.local).

Examples:
  gt commit -m "Fix bug"              # Commit as current agent
  gt commit -am "Quick fix"           # Stage all and commit
  gt commit -- --amend                # Amend last commit

Identity mapping:
  Agent: gastown/crew/jack  →  Name: jack
                                Email: the worktree's user.email

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

	// The worktree's own user.email is what a plain `git commit` here authors
	// with, so it is the half of the seat identity gt done's lease compares
	// (gt-tip6u).
	worktreeEmail, err := git.NewGit(cwd).AuthorEmail()
	if err != nil {
		worktreeEmail = ""
	}
	seat := git.SeatIdentityFor(identity, worktreeEmail)
	if seat.Email == "" {
		// Nothing in the worktree names an address, so keep the agent's own
		// rather than author with none; gt done cannot read this worktree's
		// email either, which leaves its lease off rather than mismatched.
		seat.Email = identityToEmail(identity, agentEmailDomain())
	}
	return runGitCommit(args, seat)
}

// agentEmailDomain returns the town's agent_email_domain, or the default when
// no town settings are reachable.
func agentEmailDomain() string {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return DefaultAgentEmailDomain
	}
	settings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil || settings.AgentEmailDomain == "" {
		return DefaultAgentEmailDomain
	}
	return settings.AgentEmailDomain
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

// identityToEmail converts a Gas Town identity to a git email address.
// "gastown/crew/jack" → "gastown.crew.jack@domain"
// "deacon/" → "deacon@domain"
func identityToEmail(identity, domain string) string {
	// Remove trailing slash if present
	identity = strings.TrimSuffix(identity, "/")

	// Replace slashes with dots for email local part
	localPart := strings.ReplaceAll(identity, "/", ".")

	return localPart + "@" + domain
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

// withAuthorEnv returns env with seat's author variables set in place of any it
// already holds: git reads the first GIT_AUTHOR_NAME in the environment, so an
// appended second entry would leave the inherited one in force.
func withAuthorEnv(env []string, seat git.SeatIdentity) []string {
	set := make(map[string]string, 2)
	if seat.Name != "" {
		set["GIT_AUTHOR_NAME"] = seat.Name
	}
	if seat.Email != "" {
		set["GIT_AUTHOR_EMAIL"] = seat.Email
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
	for k, v := range set {
		out = append(out, k+"="+v)
	}
	return out
}
