// Package promote fast-forwards a rig's GitHub main to a commit the rig's main
// verdict has called good.
//
// It is the one thing that pushes a rig's main to GitHub: the red-main owner
// calls the step on a green post-land verdict, and the tier sweep calls the
// same step once a cycle covers every tier green (gt-fn9e6.38), so promotion
// rides the check that decides main is green instead of a Forgejo push mirror
// that pushes every commit before any check has run (gt-fn9e6.37).
//
// A promotion never blocks the landing or post-land run it rides in: every
// failure — a target that moved, an unreachable GitHub, a rejected push — is
// recorded in the returned State and retried at the next green verdict.
package promote

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/lock"
)

// MainRef is the one ref promotion writes: the target's main branch. Every
// push is exactly <green commit>:refs/heads/main, never a force, never
// another branch or a tag.
const MainRef = "refs/heads/main"

// KeyPlaceholder replaces the key file's path in every message this package
// logs or records, so a git push error that quotes ssh's own "Load key" line
// cannot spill the deploy key's location into a log or the state file.
const KeyPlaceholder = "<promote-key>"

// State is what a rig's promotion remembers between verdicts. It lives inside
// the red-main state JSON (landworker.MainState) so the main verdict's owner
// and every reader of a rig's promotion share one record and one file
// (gt-fn9e6.38).
type State struct {
	// LastPromoted is the commit the target's main was last advanced to.
	LastPromoted string `json:"last_promoted,omitempty"`
	// LastPromotedAt is when that push succeeded.
	LastPromotedAt time.Time `json:"last_promoted_at,omitzero"`
	// LastError is the newest failure to promote: an unreachable target, an
	// unreadable tip, a rejected push. A success clears it; a divergence
	// replaces it (a diverged target is its own, louder condition).
	LastError string `json:"last_promote_error,omitempty"`
	// GitHubDiverged records a target main that is not an ancestor of the
	// commit a green verdict wanted to promote. Promotion stops until an
	// operator reconciles the two; a push could only rewrite history.
	GitHubDiverged *Divergence `json:"github_diverged,omitempty"`
}

// Divergence is the two commits of a target main that cannot be
// fast-forwarded: where the target is, and where the green verdict wanted it.
type Divergence struct {
	// RemoteMain is the target main's tip, which is not an ancestor of Commit.
	RemoteMain string `json:"remote_main"`
	// Commit is the green commit that could not be promoted.
	Commit string `json:"commit"`
}

// Repo is the git surface a promotion uses: the target's main tip, ancestry
// between that tip and the green commit, and the push. *git.Git satisfies it;
// a test answers it with gitfake or a stub.
type Repo interface {
	ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error)
	IsAncestor(ancestor, descendant string) (bool, error)
	PushWithEnv(remote, refspec string, force bool, env []string) error
}

// Promoter fast-forwards one rig's GitHub main, one commit at a time.
type Promoter struct {
	// Rig names the rig in logs and in the escalation text.
	Rig string
	// Target is the target repository's URL (merge_queue.forgejo.promote_target),
	// any git URL from tests to the production ssh form.
	Target string
	// KeyFile is the private deploy key ssh authenticates with
	// (merge_queue.forgejo.promote_key_file). Its path reaches ssh as the one
	// -i argument and nowhere else: never logged, never stored.
	KeyFile string
	// Repo reads the target and pushes to it from the rig's repository.
	Repo Repo
	// LockPath is the file a promotion holds while it runs, so the red-main
	// owner and the tier sweep serialize instead of pushing at once. Empty
	// runs without the lock.
	LockPath string
	// Escalate raises one alert when the target's main has diverged. Empty
	// logs instead.
	Escalate func(message string)
	// Logf is the log sink. Empty discards.
	Logf func(format string, args ...any)
	// Now supplies the clock, so a test can pin LastPromotedAt. Empty means
	// time.Now.
	Now func() time.Time
}

// LockPath is the file a rig's promotion holds: the red-main runtime
// directory's, beside the state it updates, so one rig has one promotion lock.
func LockPath(townRoot, rig string) string {
	return filepath.Join(townRoot, ".runtime", "red-main", rig+".promote.flock")
}

// Promote advances the target's main to commit when that is a fast-forward,
// and returns the updated state. It reports no error: a failure is recorded in
// the returned state and retried at the next green verdict, so a GitHub
// problem never fails a verdict that is about the tree.
func (p *Promoter) Promote(st State, commit string) State {
	if p == nil || p.Target == "" || commit == "" {
		return st
	}
	unlock, held, err := p.lock()
	if err != nil {
		p.logf("taking the promotion lock: %v", err)
		return p.recordError(st, fmt.Sprintf("taking the promotion lock: %v", err))
	}
	if !held {
		p.logf("another promotion holds %s; retrying at the next green verdict", p.LockPath)
		return st
	}
	defer unlock()

	tip, err := p.remoteMain()
	if err != nil {
		p.logf("reading %s main: %v", p.safeTarget(), err)
		return p.recordError(st, fmt.Sprintf("reading the target's main: %v", err))
	}
	if tip == commit {
		// The target is already at the green commit: nothing to push, and no
		// divergence or error can still hold, so an operator's hand
		// reconciliation stops alarming on the next green verdict.
		st.LastError = ""
		st.GitHubDiverged = nil
		return st
	}
	if tip != "" {
		ancestor, err := p.Repo.IsAncestor(tip, commit)
		if err != nil {
			p.logf("checking %s against %s: %v", short(tip), short(commit), err)
			return p.recordError(st, fmt.Sprintf("checking whether the target's main %s is an ancestor of %s: %v", short(tip), short(commit), err))
		}
		if !ancestor {
			return p.diverged(st, tip, commit)
		}
	}
	if err := p.Repo.PushWithEnv(p.Target, commit+":"+MainRef, false, p.keyEnv()); err != nil {
		p.logf("pushing %s to %s: %v", short(commit), p.safeTarget(), err)
		return p.recordError(st, fmt.Sprintf("pushing %s to %s: %v", short(commit), p.safeTarget(), err))
	}
	p.logf("promoted %s to %s main", short(commit), p.safeTarget())
	st.LastPromoted = commit
	st.LastPromotedAt = p.now()
	st.LastError = ""
	st.GitHubDiverged = nil
	return st
}

// recordError notes a failure and clears nothing else, so LastPromoted keeps
// naming what the target actually holds.
func (p *Promoter) recordError(st State, detail string) State {
	st.LastError = p.scrub(detail)
	return st
}

// diverged records a target main that is not an ancestor of commit and raises
// the divergence's one escalation. A divergence already on record does not
// escalate again, so a target left diverged for many green verdicts pages
// once.
func (p *Promoter) diverged(st State, tip, commit string) State {
	d := &Divergence{RemoteMain: tip, Commit: commit}
	st.LastError = ""
	isNew := st.GitHubDiverged == nil || *st.GitHubDiverged != *d
	st.GitHubDiverged = d
	if isNew {
		p.escalate(fmt.Sprintf("rig %s: GitHub main is at %s, which is not an ancestor of the green commit %s, so it was not promoted. GitHub main is ahead of or diverged from Forgejo main; reconcile the two by hand and the next green verdict promotes.", p.Rig, short(tip), short(commit)))
	}
	p.logf("target main %s is not an ancestor of the green commit %s: not pushing (github_diverged)", short(tip), short(commit))
	return st
}

// remoteMain is the target's main tip, or "" when the target has no main yet
// (a fresh repository, which the push below creates).
func (p *Promoter) remoteMain() (string, error) {
	refs, err := p.Repo.ListRemoteRefsWithHashes(p.Target, "refs/heads/")
	if err != nil {
		return "", err
	}
	for _, ref := range refs {
		if ref.Name == MainRef {
			return ref.Hash, nil
		}
	}
	return "", nil
}

// keyEnv is the environment the push runs with: the deploy key and no other
// identity ssh might offer (IdentitiesOnly=yes).
func (p *Promoter) keyEnv() []string {
	return []string{"GIT_SSH_COMMAND=ssh -i " + shellQuote(p.KeyFile) + " -o IdentitiesOnly=yes"}
}

// lock takes the promotion lock, reporting false when another holder has it.
// A lock path's directory is created here: it is the red-main runtime
// directory, which the caller's state write also needs.
func (p *Promoter) lock() (func(), bool, error) {
	if p.LockPath == "" {
		return func() {}, true, nil
	}
	if err := os.MkdirAll(filepath.Dir(p.LockPath), 0o755); err != nil { //nolint:gosec // G301: a runtime directory, as RedMainStatePath's writer makes it
		return nil, false, err
	}
	return lock.FlockTryAcquire(p.LockPath)
}

// scrub replaces the key file's path with KeyPlaceholder, so a message that
// quotes an ssh error cannot name where the deploy key lives.
func (p *Promoter) scrub(s string) string {
	if p.KeyFile == "" {
		return s
	}
	return strings.ReplaceAll(s, p.KeyFile, KeyPlaceholder)
}

// safeTarget is the target with any key path scrubbed; it names a repository,
// never a credential.
func (p *Promoter) safeTarget() string { return p.scrub(p.Target) }

func (p *Promoter) escalate(message string) {
	message = p.scrub(message)
	if p.Escalate == nil {
		return
	}
	p.Escalate(message)
}

func (p *Promoter) logf(format string, args ...any) {
	if p.Logf == nil {
		return
	}
	// "promote" is this package's own component tag, not a caller's: two
	// callers promote (gt-fn9e6.38), so a line naming either is wrong for the
	// other. The rig still leads the message, which is the position gt tail
	// attributes a daemon line by.
	p.Logf("promote: %s: %s", p.Rig, p.scrub(fmt.Sprintf(format, args...)))
}

func (p *Promoter) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// shellQuote wraps s in single quotes for the shell git runs
// GIT_SSH_COMMAND with, escaping any single quote, so a path with a space or
// a quote reaches ssh as one argument.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// short is the first seven characters of a commit, enough to name it in a log
// line or an alert.
func short(commit string) string {
	if len(commit) <= 7 {
		return commit
	}
	return commit[:7]
}
