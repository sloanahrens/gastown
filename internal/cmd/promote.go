package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/promote"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/workspace"
)

// gt promote's exit codes are its caller's contract (gt-5xrmp): 0 the target
// holds the commit, 1 the commit is not promotable, 2 the promotion failed.
const (
	promoteExitNotPromotable = 1
	promoteExitFailed        = 2
)

var (
	promoteRigFlag string
	promoteSHAFlag string
)

var promoteCmd = &cobra.Command{
	Use:     "promote",
	GroupID: GroupWork,
	Short:   "Push one commit to a rig's GitHub promote target",
	Long: `Push exactly <commit>:refs/heads/main to a rig's GitHub promote target:
the promotion a green main verdict and the tier sweep ride (internal/promote).

Reads the rig's merge_queue.forgejo.promote_target and promote_key_file, and
refuses a commit that is not on the rig's Forgejo main. A promotion records
last_promoted in the rig's red-main state, so the next caller reads what the
target holds.

For a rig the town's tier_sweep patrol covers, the sweep owns the promotion:
only the commit of its last fully green cycle is promotable, and any other is
refused. A rig the sweep does not cover is promoted as the caller asks.

Exit codes: 0 the target holds the commit (promoted now, or already); 1 the
commit is not promotable (not on the rig's Forgejo main, the rig names no
promote_target or key, another promotion holds the rig's lock, or a
sweep-covered rig's commit is not the sweep's last green); 2 the promotion
failed (a diverged target, a rejected push, or a rig main that could not be
read).`,
	RunE: runPromote,
}

func init() {
	promoteCmd.Flags().StringVar(&promoteRigFlag, "rig", "", "Rig whose GitHub main to advance")
	promoteCmd.Flags().StringVar(&promoteSHAFlag, "sha", "", "Commit on the rig's Forgejo main to promote")
	_ = promoteCmd.MarkFlagRequired("rig")
	_ = promoteCmd.MarkFlagRequired("sha")
	rootCmd.AddCommand(promoteCmd)
}

// promoteGit is the git surface gt promote reads and pushes through: the rig's
// own bare repository, opened once. *git.Git in production, gitfake in a test.
type promoteGit interface {
	promote.Repo
	FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error
	RefExists(ref string) (bool, error)
	Rev(ref string) (string, error)
}

// promoteDeps are the seams gt promote runs through, so its exit codes are
// covered without a network, a daemon or a live GitHub target.
type promoteDeps struct {
	// TownRoot holds the rigs, their bare repositories and their red-main state.
	TownRoot string
	// Repo opens the rig's bare repository.
	Repo func(path string) promoteGit
	// State loads and saves the rig's red-main record (daemon.RedMainStateStore
	// in production).
	State func(townRoot, rigName string) landworker.MainStateStore
	// Escalate raises the divergence alert for a rig.
	Escalate func(rigName, message string)
	// Sweep reads what the town's tier sweep owns for a rig: whether the
	// sweep covers it and its last green commit (daemon.TierSweepCoverageFor
	// in production).
	Sweep func(townRoot, rigName string) (daemon.TierSweepCoverage, error)
	// Out carries the one-line outcome, Err the promoter's own log lines.
	Out, Err io.Writer
}

// logf is the promotion's log sink: the promoter tags each line "promote:",
// and Err keeps it off the outcome a caller reads.
func (d promoteDeps) logf(format string, args ...any) {
	if d.Err == nil {
		return
	}
	fmt.Fprintf(d.Err, format+"\n", args...)
}

func runPromote(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}
	return promoteRigSHA(promoteDeps{
		TownRoot: townRoot,
		Repo:     func(path string) promoteGit { return git.NewGit(path) },
		State:    daemon.RedMainStateStore,
		Escalate: func(rigName, message string) { raisePromoteDiverged(townRoot, rigName, message) },
		Sweep:    daemon.TierSweepCoverageFor,
		Out:      cmd.OutOrStdout(),
		Err:      cmd.ErrOrStderr(),
	}, promoteRigFlag, promoteSHAFlag)
}

// promoteRigSHA advances rigName's GitHub main to sha through the promotion
// owner a green main verdict and the tier sweep call
// (daemon.NewRigPromoter), and reports the outcome in promoteDeps' exit codes
// (gt-wilev).
func promoteRigSHA(deps promoteDeps, rigName, sha string) error {
	rigName = strings.TrimSpace(rigName)
	sha = strings.TrimSpace(sha)
	if rigName == "" {
		return fmt.Errorf("--rig is required")
	}
	if sha == "" {
		return fmt.Errorf("--sha is required")
	}
	fj := rig.ResolveForgejoConfig(deps.TownRoot, rigName)
	if fj == nil || strings.TrimSpace(fj.RemoteURL) == "" {
		return promoteNotPromotable("rig %s is not configured for Forgejo-primary landing: merge_queue.forgejo.remote_url is unset, so no Forgejo main names what may be promoted", rigName)
	}
	if strings.TrimSpace(fj.PromoteTarget) == "" {
		return promoteNotPromotable("rig %s names no merge_queue.forgejo.promote_target, so it does not promote", rigName)
	}
	if strings.TrimSpace(fj.PromoteKeyFile) == "" {
		return promoteNotPromotable("rig %s names a promote_target but no promote_key_file, so nothing can push to it", rigName)
	}

	repoPath := filepath.Join(deps.TownRoot, rigName, ".repo.git")
	repo := deps.Repo(repoPath)
	commit, err := commitOnRigMain(repo, fj.RemoteURL, sha)
	if err != nil {
		return promoteFailed("reading rig %s's Forgejo main: %v", rigName, err)
	}
	if commit == "" {
		return promoteNotPromotable("%s is not on rig %s's Forgejo main, so it is not promotable", shortSHA(sha), rigName)
	}
	if err := refusePromotePastSweep(deps, rigName, commit); err != nil {
		return err
	}

	store := deps.State(deps.TownRoot, rigName)
	st, err := store.Load()
	if err != nil {
		return fmt.Errorf("reading rig %s's red-main state: %w", rigName, err)
	}
	escalate := func(message string) {
		if deps.Escalate != nil {
			deps.Escalate(rigName, message)
		}
	}
	p := daemon.NewRigPromoter(deps.TownRoot, rigName, fj, repo, escalate, deps.logf)
	if p == nil {
		// Unreachable while NewRigPromoter refuses exactly the rigs the checks
		// above refused; kept so a rule added there cannot panic here.
		return promoteNotPromotable("rig %s has no GitHub promotion owner", rigName)
	}

	// The record is written whatever the promotion did, exactly as the landing
	// worker's verdict and the tier sweep write it: a divergence is raised
	// again and only the record keeps it from paging a second time.
	st.State = p.Promote(st.State, commit)
	if err := store.Save(st); err != nil {
		return fmt.Errorf("recording rig %s's promotion: %w", rigName, err)
	}

	switch {
	case st.State.GitHubDiverged != nil:
		return promoteFailed("rig %s: the promotion target's main is at %s, which is not an ancestor of %s, so it was left alone and the divergence is escalated", rigName, shortSHA(st.State.GitHubDiverged.RemoteMain), shortSHA(commit))
	case st.State.LastError != "":
		return promoteFailed("rig %s: the promotion failed: %s", rigName, st.State.LastError)
	case st.State.LastPromoted == commit:
		fmt.Fprintf(deps.Out, "rig %s: GitHub main holds %s\n", rigName, shortSHA(commit))
		return nil
	default:
		// Every other outcome of promote.Promoter.Promote sets LastPromoted,
		// a divergence or an error, so this is the lock: another promotion has
		// the rig and this one changed nothing.
		return promoteNotPromotable("rig %s: not promoted: another promotion holds %s; retry later", rigName, promote.LockPath(deps.TownRoot, rigName))
	}
}

// rigMainRef is where commitOnRigMain parks the rig's Forgejo main: a local,
// read-only ref, never pushed. The promotion's own fetch uses
// promote.TargetMainRef; the two never share a ref.
const rigMainRef = "refs/promote/rig-main"

// commitOnRigMain returns the full commit id sha names when sha is on the
// rig's Forgejo main, and "" when it is not: the rig's own repository fetches
// that main into a throwaway ref and asks whether sha is an ancestor of it. A
// fetch failure is an error, never a "no" (the landing worker's
// gitRemote.Contains rule, gt-fn9e6.9).
//
// The full id is what the command promotes and records. A caller may pass an
// abbreviated sha — a commit line copied from git log — and an abbreviated
// last_promoted is what the sweep's own retry test compares against, so the
// short form would read as a promotion still owed.
func commitOnRigMain(g promoteGit, forgejoURL, sha string) (string, error) {
	if err := g.FetchRefspecWithTimeout(forgejoURL, "+"+promote.MainRef+":"+rigMainRef, git.RemoteQueryTimeout); err != nil {
		return "", err
	}
	// A commit the repository still lacks after the fetch is not on main; any
	// other failure to read it is an error.
	exists, err := g.RefExists(sha + "^{commit}")
	if err != nil || !exists {
		return "", err
	}
	commit, err := g.Rev(sha + "^{commit}")
	if err != nil {
		return "", err
	}
	onMain, err := g.IsAncestor(commit, rigMainRef)
	if err != nil || !onMain {
		return "", err
	}
	return commit, nil
}

// refusePromotePastSweep refuses commit when the tier sweep owns rigName's
// promotion and its last green cycle was a different commit: for a rig the
// sweep covers, the sweep decides which commit GitHub main may hold, so a
// manual promotion cannot publish one the sweep has not called green
// (gt-qk0pi). A rig outside the sweep is the caller's to promote.
func refusePromotePastSweep(deps promoteDeps, rigName, commit string) error {
	sweep := deps.Sweep
	if sweep == nil {
		sweep = daemon.TierSweepCoverageFor
	}
	cov, err := sweep(deps.TownRoot, rigName)
	if err != nil {
		return promoteFailed("reading the tier sweep's coverage of rig %s: %v", rigName, err)
	}
	if !cov.Covered {
		return nil
	}
	if cov.LastGreenSHA == "" {
		return promoteNotPromotable("rig %s's promotion is owned by the tier sweep, which has no fully green cycle on record yet, so no commit is promotable", rigName)
	}
	if cov.LastGreenSHA != commit {
		return promoteNotPromotable("rig %s's promotion is owned by the tier sweep: only its last green commit %s is promotable, not %s", rigName, shortSHA(cov.LastGreenSHA), shortSHA(commit))
	}
	return nil
}

// promoteNotPromotable is a refusal under promoteExitNotPromotable: nothing
// pushed, and pushing again would change nothing until the caller acts.
func promoteNotPromotable(format string, args ...any) error {
	return &ExitCodeError{Code: promoteExitNotPromotable, Err: fmt.Errorf(format, args...)}
}

// promoteFailed is a promotion failure under promoteExitFailed: the target was
// left alone, and the cause is in the message.
func promoteFailed(format string, args ...any) error {
	return &ExitCodeError{Code: promoteExitFailed, Err: fmt.Errorf(format, args...)}
}

// raisePromoteDiverged files a promotion divergence under the key the landing
// worker's own promotion raises it under, so the condition a human reconciles
// has one open escalation whoever found it. A raise that itself fails is
// printed and the promotion's exit code still carries the divergence.
func raisePromoteDiverged(townRoot, rigName, message string) {
	cfg, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gt promote: loading the escalation config: %v\n", err)
		return
	}
	if _, err := notify.Raise(notify.EscalationRequest{
		TownRoot:    townRoot,
		Severity:    "high",
		Description: promoteHeadline(message),
		Reason:      message,
		Source:      "gt promote",
		Fingerprint: daemon.PromoteDivergedAlertKey(rigName),
	}, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "gt promote: raising the divergence alert: %v\n", err)
	}
}

// promoteHeadline is message's first line: an escalation's description becomes
// the bead's title, which is one line.
func promoteHeadline(message string) string {
	if i := strings.IndexAny(message, "\r\n"); i >= 0 {
		message = message[:i]
	}
	return strings.TrimSpace(message)
}
