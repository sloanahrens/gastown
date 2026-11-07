package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// The image build reads a fully green sweep's commit from a branch, never a
// tag: the build's version is `git describe --tags --always`, so a green-*
// tag would become the nearest tag of every later commit and change the
// version of every build, and a branch has neither problem (gt-gg9hv.61).
const (
	// greenRefKeep is how many green refs the remote keeps: a push retains the
	// newest ones and deletes the rest.
	greenRefKeep = 20
	// greenRefAttentionAfter is the consecutive failed pushes that raise the
	// green-ref-push item. The ref is retried every cycle until one succeeds.
	greenRefAttentionAfter = 3
	// greenRefPrefix is the branch namespace the image build reads.
	greenRefPrefix = "refs/heads/green/"
	// greenRefLogEvery is how often a persisting failure is logged. A cycle
	// that retries the same commit every heartbeat must not write its line
	// every time.
	greenRefLogEvery = time.Hour
	// greenRefDetailRunes bounds the remote's message in a log line and an
	// item summary.
	greenRefDetailRunes = 200
)

// greenRefName is the branch a green commit is published at.
func greenRefName(sha string) string {
	return "green/" + greenRefShortSHA(sha)
}

// greenRefShortSHA is the 12-character commit the ref is named after.
func greenRefShortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// greenRefGit is the git surface a green ref push drives: the rig's repository
// at the remote its landing worker pushes through. *git.Git and the daemon's
// own seam both answer it.
type greenRefGit interface {
	Push(remote, refspec string, force bool) error
	ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error)
	DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error
	CommitTime(rev string) (time.Time, error)
}

// greenRefPusher publishes one rig's green refs. remote is a remote name,
// never a URL: the credential stays in the repository's configuration and no
// argument or log line carries it.
type greenRefPusher struct {
	git    greenRefGit
	remote string
}

// ensure publishes sha as green/<sha12>. pushed reports the push this call
// made: a ref already at the commit is success with nothing written, and a
// caller retaining the refs has nothing to prune then. It reports a conflict
// when the remote holds that ref at another commit, which is a refusal: the ref
// is never force-updated. The push is a plain one otherwise, so an unreachable
// remote or a refused hook is an error for the caller to record.
func (p greenRefPusher) ensure(sha string) (pushed, conflict bool, err error) {
	ref := "refs/heads/" + greenRefName(sha)
	held, err := p.tip(ref)
	if err != nil {
		return false, false, fmt.Errorf("reading %s on %s: %w", ref, p.remote, err)
	}
	switch {
	case sha != "" && held == sha:
		// The remote already publishes this commit: no second push. An empty
		// sha never matches the remote's silence, so a caller that passes none
		// falls through to a push git refuses rather than reading as success.
		return false, false, nil
	case held != "":
		return false, true, nil
	default:
		if err := p.git.Push(p.remote, sha+":"+ref, false); err != nil {
			return false, false, fmt.Errorf("pushing %s to %s: %w", ref, p.remote, err)
		}
		return true, false, nil
	}
}

// tip is the commit the remote holds at the full ref name, "" when it holds
// none. ls-remote matches its pattern as a path suffix, so the listing is
// filtered to the exact name.
func (p greenRefPusher) tip(ref string) (string, error) {
	refs, err := p.git.ListRemoteRefsWithHashes(p.remote, ref)
	if err != nil {
		return "", err
	}
	for _, r := range refs {
		if r.Name == ref {
			return r.Hash, nil
		}
	}
	return "", nil
}

// retain deletes every green ref past the newest greenRefKeep, oldest first,
// and returns what it deleted and why it could not delete the rest. Each
// delete is leased on the hash just listed, so a ref another writer moved is
// left alone; it is not a forced push. A delete failure is counted and the
// next ref is tried; nothing outside refs/heads/green/ is touched. A ref whose
// commit cannot be dated is kept and reported: its age is unknown, so it is
// never deleted on a guess.
func (p greenRefPusher) retain() (deleted, failures []string) {
	refs, err := p.git.ListRemoteRefsWithHashes(p.remote, greenRefPrefix)
	if err != nil {
		return nil, []string{"reading the green refs: " + greenRefReason(err)}
	}
	type datedRef struct {
		ref    string
		name   string
		hash   string
		commit time.Time
	}
	dated := make([]datedRef, 0, len(refs))
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, greenRefPrefix) {
			// The listing is filtered to the prefix; a remote ref outside it
			// reaching here is another namespace's and is never touched.
			continue
		}
		when, err := p.git.CommitTime(r.Hash)
		if err != nil {
			failures = append(failures, fmt.Sprintf("keeping %s: its commit could not be dated: %s", r.Name, greenRefReason(err)))
			continue
		}
		dated = append(dated, datedRef{ref: r.Name, name: strings.TrimPrefix(r.Name, "refs/heads/"), hash: r.Hash, commit: when})
	}
	// Newest first: the first greenRefKeep stay, the rest go. Refs at the same
	// commit keep the listing's order, so the deletion set is stable.
	sort.SliceStable(dated, func(i, j int) bool { return dated[i].commit.After(dated[j].commit) })
	if len(dated) <= greenRefKeep {
		return deleted, failures
	}
	for _, r := range dated[greenRefKeep:] {
		if err := p.git.DeleteRemoteBranchIfAt(p.remote, r.name, r.hash); err != nil {
			failures = append(failures, fmt.Sprintf("deleting %s: %s", r.ref, greenRefReason(err)))
			continue
		}
		deleted = append(deleted, r.ref)
	}
	return deleted, failures
}

// greenRefReason is why a green ref write failed, in one line: the remote's
// own message when it spoke, else the failure's class. Captured git output is
// never printed whole and the error's text is not echoed, because a push's
// stderr names the remote URL, which can carry the credential (gt-gg9hv.61).
func greenRefReason(err error) string {
	var remote []string
	for _, line := range strings.Split(err.Error(), "\n") {
		line = strings.TrimSpace(line)
		if msg, ok := strings.CutPrefix(line, "remote:"); ok {
			remote = append(remote, strings.TrimSpace(msg))
		}
	}
	if len(remote) > 0 {
		return greenRefDetail(strings.Join(remote, "; "))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) {
		return fmt.Sprintf("exit status %d", ee.ExitCode())
	}
	return "the git command failed with no remote message"
}

// greenRefDetail bounds text to one log line: control characters become
// spaces and the message is cut to greenRefDetailRunes runes.
func greenRefDetail(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if rs := []rune(out); len(rs) > greenRefDetailRunes {
		out = string(rs[:greenRefDetailRunes])
	}
	return out
}

// greenRefPublishState is the green ref push's memory between cycles: per rig
// the consecutive failed pushes and the refs the remote holds at another
// commit. The sweep goroutine writes it; the attention tick reads it from the
// heartbeat goroutine.
type greenRefPublishState struct {
	mu   sync.Mutex
	rigs map[string]*greenRefRigState
}

// greenRefRigState is one rig's failed pushes and conflicts. logged dates each
// reason's last line, so a persisting failure is printed hourly rather than
// once per cycle.
type greenRefRigState struct {
	failures  int
	detail    string
	conflicts map[string]bool
	logged    map[string]time.Time
}

// greenRefItem is one rig's state as the attention collector reads it.
type greenRefItem struct {
	failures  int
	detail    string
	conflicts []string
}

// failed records a failed push and returns its consecutive count.
func (s *greenRefPublishState) failed(rigName, detail string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.rigState(rigName)
	st.failures++
	st.detail = detail
	return st.failures
}

// conflicted records that the remote holds the ref for sha12 at another
// commit. A refusal is not a failed push, so it ends the failure run: the
// push reached the remote and was answered.
func (s *greenRefPublishState) conflicted(rigName, sha12 string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.rigState(rigName)
	st.failures, st.detail = 0, ""
	if st.conflicts == nil {
		st.conflicts = map[string]bool{}
	}
	st.conflicts[sha12] = true
}

// succeeded clears the rig's failed pushes and every conflict. A push that
// landed proves the remote accepts this rig's green refs again, and a conflict
// recorded for an earlier tip cannot clear by itself: nothing ever
// force-updates that ref. Keeping it would leave the item standing after the
// rig published a different commit (gt-atcddq).
func (s *greenRefPublishState) succeeded(rigName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.rigState(rigName)
	st.failures, st.detail = 0, ""
	clear(st.conflicts)
}

// due reports whether reason may be logged now, and records the line so the
// next one waits greenRefLogEvery. One reason per key: a conflict and a failed
// push are logged on their own schedules.
func (s *greenRefPublishState) due(rigName, reason string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.rigState(rigName)
	if st.logged == nil {
		st.logged = map[string]time.Time{}
	}
	if last, ok := st.logged[reason]; ok && now.Sub(last) < greenRefLogEvery {
		return false
	}
	st.logged[reason] = now
	return true
}

// snapshot is the collector's read of every rig with a failure or a conflict.
func (s *greenRefPublishState) snapshot() map[string]greenRefItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]greenRefItem, len(s.rigs))
	for rigName, st := range s.rigs {
		if st.failures == 0 && len(st.conflicts) == 0 {
			continue
		}
		conflicts := make([]string, 0, len(st.conflicts))
		for sha12 := range st.conflicts {
			conflicts = append(conflicts, sha12)
		}
		sort.Strings(conflicts)
		out[rigName] = greenRefItem{failures: st.failures, detail: st.detail, conflicts: conflicts}
	}
	return out
}

// rigState is the given rig's record, created on first use. The caller holds
// the mutex.
func (s *greenRefPublishState) rigState(rigName string) *greenRefRigState {
	if s.rigs == nil {
		s.rigs = map[string]*greenRefRigState{}
	}
	st := s.rigs[rigName]
	if st == nil {
		st = &greenRefRigState{}
		s.rigs[rigName] = st
	}
	return st
}

// tierSweepPublishGreenRef publishes rig's fully green tip as green/<sha12>
// for the image build, when tier_sweep.image_rigs names the rig. It runs
// beside the promotion and never changes the sweep's verdict: a refusal or a
// failed push is logged once an hour, retried by the next cycle, and raised as
// an attention item on its own terms (gt-gg9hv.61).
func (d *Daemon) tierSweepPublishGreenRef(rigName, sha string) {
	if sha == "" {
		return
	}
	rigs, err := d.greenRefImageRigs()
	if err != nil {
		// An unknown rig is refused by name and nothing is pushed: the list
		// cannot be trusted to name the repository the ref belongs in.
		d.greenRefLine(rigName, "config", err.Error())
		return
	}
	if !slices.Contains(rigs, rigName) {
		return
	}
	g, remote, resolveErr := d.greenRefRemote(rigName)
	if resolveErr != nil {
		d.greenRefFailed(rigName, resolveErr.reason)
		return
	}
	p := greenRefPusher{git: g, remote: remote}
	pushed, conflict, err := p.ensure(sha)
	switch {
	case err != nil:
		d.greenRefFailed(rigName, greenRefReason(err))
		return
	case conflict:
		sha12 := greenRefShortSHA(sha)
		d.greenRefs.conflicted(rigName, sha12)
		d.greenRefLine(rigName, "conflict:"+sha12, fmt.Sprintf(
			"the remote holds green/%s at another commit, so the image build is not marked for %s and nothing was forced",
			sha12, shortSHA(sha)))
		return
	}
	d.greenRefs.succeeded(rigName)
	if !pushed {
		// The remote already held the ref at this commit, so no ref was
		// written and the retention listing has nothing to prune. Running it
		// every unchanged-main heartbeat dates every ref for no change
		// (gt-atcddq).
		return
	}
	// A push that reached the remote is where retention runs: the refs are
	// only pruned once the newest one is published.
	deleted, failures := p.retain()
	for _, failure := range failures {
		d.logger.Printf("tier_sweep: %s: green ref retention: %s", rigName, failure)
	}
	if len(deleted) > 0 {
		d.logger.Printf("tier_sweep: %s: green ref retention deleted %d: %s", rigName, len(deleted), strings.Join(deleted, ", "))
	}
}

// greenRefFailed records a failed publish and logs it hourly.
func (d *Daemon) greenRefFailed(rigName, reason string) {
	n := d.greenRefs.failed(rigName, reason)
	message := fmt.Sprintf("publishing the green ref failed (%d cycles in a row): %s", n, reason)
	d.greenRefLine(rigName, "push", message)
}

// greenRefLine prints one green-ref line per rig and reason per hour.
func (d *Daemon) greenRefLine(rigName, reason, message string) {
	if !d.greenRefs.due(rigName, reason, d.clk().Now()) {
		return
	}
	d.logger.Printf("tier_sweep: %s: green ref: %s", rigName, message)
}

// greenRefImageRigs is the validated tier_sweep.image_rigs list: the rigs a
// fully green cycle publishes a ref for, or the refusal of a name the town
// does not register.
func (d *Daemon) greenRefImageRigs() ([]string, error) {
	return tierSweepConfig(d.patrolConfig).TierSweepImageRigs(d.getKnownRigs())
}

// greenRefRemoteError is a landing-remote resolution failure. reason is what a
// log line and an item summary may carry; the wrapped error is kept as the
// cause but its text is never printed, because resolution names
// merge_queue.forgejo.remote_url and a remote URL can carry a credential in
// its user info (gt-atcddq).
type greenRefRemoteError struct {
	reason string
	err    error // nil when the daemon refused the rig itself, not a wrapped cause
}

func (e *greenRefRemoteError) Error() string {
	if e.err == nil {
		return e.reason
	}
	return e.reason + ": " + e.err.Error()
}

func (e *greenRefRemoteError) Unwrap() error { return e.err }

// greenRefRemote opens the rig's repository at the remote its landing worker
// pushes land/<bead> branches with, so the green ref uses the same seam and
// the landing bot's credential. A rig with no Forgejo block has no such
// remote, and the publish is refused rather than sent to origin. The failure
// carries a safe class for the caller to log, never the resolution error's own
// text (gt-atcddq).
func (d *Daemon) greenRefRemote(rigName string) (greenRefGit, string, *greenRefRemoteError) {
	repo := constants.RigBareRepoPath(filepath.Join(d.config.TownRoot, rigName))
	g := d.gitAt(repo)
	fc, err := rig.ResolveForgejoConfigWithError(d.repoFileOpener(), d.config.TownRoot, rigName)
	if err != nil {
		return nil, "", &greenRefRemoteError{reason: "the rig's Forgejo settings could not be read", err: err}
	}
	if fc == nil || strings.TrimSpace(fc.RemoteURL) == "" {
		return nil, "", &greenRefRemoteError{
			reason: fmt.Sprintf("rig %s names no merge_queue.forgejo remote_url, so it has no Forgejo remote to publish the green ref to", rigName),
		}
	}
	remote, err := rig.ResolveLandingRemoteWith(d.repoFileOpener(), g, d.config.TownRoot, rigName)
	if err != nil {
		return nil, "", &greenRefRemoteError{reason: "no git remote matches merge_queue.forgejo.remote_url", err: err}
	}
	return g, remote, nil
}

// collectGreenRef raises the low items the green ref owes: a push that has
// failed greenRefAttentionAfter cycles in a row, and a rig whose remote holds
// the ref at another commit. Either clears when the next publish succeeds.
func (s *attentionSources) collectGreenRef(_ context.Context) ([]attention.Item, error) {
	var out []attention.Item
	for rigName, st := range s.d.greenRefs.snapshot() {
		if st.failures >= greenRefAttentionAfter {
			out = append(out, attention.Item{
				Key:      "green-ref-push:" + rigName,
				Kind:     attention.KindGreenRef,
				Severity: attention.SeverityLow,
				Rig:      rigName,
				Summary: fmt.Sprintf("green ref push for %s failed %d cycles in a row: %s",
					rigName, st.failures, st.detail),
			})
		}
		for _, sha12 := range st.conflicts {
			out = append(out, attention.Item{
				Key:      "green-ref-conflict:" + rigName + ":" + sha12,
				Kind:     attention.KindGreenRef,
				Severity: attention.SeverityLow,
				Rig:      rigName,
				SHA:      sha12,
				Summary: fmt.Sprintf("the remote holds green/%s for %s at another commit, so the image build is not marked and nothing was forced",
					sha12, rigName),
			})
		}
	}
	return out, nil
}
