package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
)

// localRoundInterval is how often the local round patrol looks for requests.
const localRoundInterval = 60 * time.Second

// localRoundRequestRE is a request a polecat writes to its bead's notes: the
// head it asks the daemon to review on its own round (gt-ryp1ff).
var localRoundRequestRE = regexp.MustCompile(`^LOCAL ROUND REQUEST head=(\S*)$`)

// localRoundHeadRE is the shape a request's head must have before it reaches
// git: a hex object name, so no option or path can be spliced in.
var localRoundHeadRE = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// localRoundBlockPrefix opens every line the round writes: a block header and
// an unavailable line. A request line does not start with it.
const localRoundBlockPrefix = "LOCAL ROUND ("

// localRoundNumberRE reads the round number off a block or an unavailable line.
var localRoundNumberRE = regexp.MustCompile(`^LOCAL ROUND \((\d{1,9})\)`)

// localRoundUnavailableRE is the line a failed or refused round leaves. It has
// no findings, so no reader can take it for a clean review.
var localRoundUnavailableRE = regexp.MustCompile(`^LOCAL ROUND \((\d{1,9})\): unavailable - `)

// localRoundTree is the tree one round reviews: a detached worktree at the
// head, the merge base the round's diff starts from, the repository that holds
// both, and the call that removes the worktree.
type localRoundTree struct {
	Dir     string
	Base    string
	Repo    land.Repo
	Cleanup func()
}

// LocalRoundPatrol serves LOCAL ROUND REQUEST notes with the landing reviewer
// and writes no verdict (gt-ryp1ff). Each tick it reads the open and in_progress
// beads, and for each request that no block answers, oldest first and one at a
// time, it runs one round and appends either a LOCAL ROUND block or an
// unavailable line.
//
// Every effect is a function field, so a test drives the patrol with fakes and
// starts no om, git or daemon. The patrol holds no verdict store, review log or
// rounds ledger: a round's findings go only into the bead's notes.
type LocalRoundPatrol struct {
	// ListRequests returns the open and in_progress beads whose notes may carry a request.
	ListRequests func() ([]*beads.Issue, error)
	// Comments returns one bead's comments, the decisions the review reads.
	Comments func(bead string) ([]beads.Comment, error)
	// AppendNote appends one note to a bead.
	AppendNote func(bead, note string) error
	// Fetch makes head available under the bead's private ref and returns the
	// tree the round reviews. It never writes a landing ref.
	Fetch func(bead, head string) (localRoundTree, error)
	// Reviewer runs the om review; it is the landing's reviewer with no
	// verdict store attached.
	Reviewer land.BeadReviewer
	// Bound counts the bead's rounds per local day.
	Bound land.Bound
	// Config reads the daemon's patrol config each tick, so a reload takes
	// effect on the next request.
	Config func() *config.DaemonPatrolConfig
	// Operator reads the operator identities the decisions are entitled by.
	Operator func() (*config.OperatorConfig, error)
	// Paused returns why the landing worker's om is paused, or "" when it is
	// not.
	Paused func() string
	// Now is the clock the block's time is taken from.
	Now func() time.Time
	// Logf logs a line the operator reads in the daemon log.
	Logf func(format string, args ...any)
}

// Tick serves every pending request once. A request that fails is answered
// with an unavailable line, so the next tick finds it answered and moves on.
// The switch is read per request, after the listing, on purpose: a disabled
// switch still answers each request unavailable, and that needs the listing.
func (p *LocalRoundPatrol) Tick(ctx context.Context) {
	issues, err := p.ListRequests()
	if err != nil {
		p.Logf("local_round: listing the open beads failed; no request is served this tick")
		return
	}
	sortByCreated(issues)
	for _, issue := range issues {
		if ctx.Err() != nil {
			return
		}
		head, ok := pendingLocalRoundRequest(issue.Notes)
		if !ok {
			continue
		}
		p.serve(ctx, issue, head)
	}
}

// serve runs one round for issue's request and appends its answer. Every
// refusal and failure appends an unavailable line that names the reason.
func (p *LocalRoundPatrol) serve(ctx context.Context, issue *beads.Issue, head string) {
	n := localRoundNextNumber(issue.Notes)
	unavailable := func(reason string) {
		p.answer(issue.ID, localRoundUnavailableLine(n, reason))
	}
	cfg := p.Config()
	if !cfg.LocalRoundEnabled() {
		unavailable("daemon.local_round is off")
		return
	}
	timeout, err := cfg.LocalRoundTimeout()
	if err != nil {
		unavailable(err.Error())
		return
	}
	if !localRoundHeadRE.MatchString(head) {
		unavailable("the request names no valid head")
		return
	}
	if reason := p.Paused(); reason != "" {
		unavailable("the landing worker's om is paused: " + reason)
		return
	}
	if _, err := p.Bound.Allow(issue.ID); err != nil {
		unavailable(err.Error())
		return
	}
	// The fetch, comment and operator errors are not written: they can carry a
	// command's output, so the answer names the step that failed instead. A
	// bead id that cannot name a ref is the one fetch error named, since the id
	// is the request's own.
	tree, err := p.Fetch(issue.ID, head)
	if errors.Is(err, errLocalRoundBeadRef) {
		unavailable(err.Error())
		return
	}
	if err != nil {
		unavailable("could not fetch the head")
		return
	}
	defer tree.Cleanup()
	comments, err := p.Comments(issue.ID)
	if err != nil {
		unavailable("could not read the bead's comments")
		return
	}
	op, err := p.Operator()
	if err != nil {
		unavailable("could not read the operator identities")
		return
	}
	in := land.BuildReviewInput(issue, comments, op, land.AcceptedRisksText(comments, op), "", tree.Base, head, tree.Dir, tree.Repo)
	// BuildReviewInput already carries the MERGE REJECTION findings
	// (review_input.go), so the previous LOCAL ROUND block's findings follow
	// them and no second parser reads the rejection notes.
	in.PriorFindings = append(in.PriorFindings, localRoundPriorFindings(issue.Notes)...)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	findings, err := land.RunLocalRound(rctx, p.Reviewer, in)
	cancel()
	if err != nil {
		unavailable(localRoundReviewReason(err, timeout))
		return
	}
	p.answer(issue.ID, land.FormatLocalRound(n, head, findings, p.Now()))
}

// answer appends text to the bead's notes. A failed append is logged without
// its error text: the request then stays open and the next tick serves it
// again, which the round bound limits.
func (p *LocalRoundPatrol) answer(bead, text string) {
	if err := p.AppendNote(bead, text); err != nil {
		p.Logf("local_round: %s: could not append the answer to the bead's notes", bead)
	}
}

// pendingLocalRoundRequest returns the head of notes' latest request when no
// block or unavailable line follows it. ok is false when there is no request,
// or when the latest one is already answered.
func pendingLocalRoundRequest(notes string) (head string, ok bool) {
	lines := strings.Split(notes, "\n")
	req := -1
	for i, line := range lines {
		if m := localRoundRequestRE.FindStringSubmatch(line); m != nil {
			req, head = i, m[1]
		}
	}
	if req < 0 {
		return "", false
	}
	for _, line := range lines[req+1:] {
		if strings.HasPrefix(line, localRoundBlockPrefix) {
			return "", false
		}
	}
	return head, true
}

// localRoundNextNumber is the round number a new block or unavailable line
// takes: one past the highest number any LOCAL ROUND line carries.
func localRoundNextNumber(notes string) int {
	highest := 0
	for _, line := range strings.Split(notes, "\n") {
		m := localRoundNumberRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// The regexp allows at most nine digits, so this cannot overflow.
		n := 0
		for _, c := range m[1] {
			n = n*10 + int(c-'0')
		}
		if n > highest {
			highest = n
		}
	}
	return highest + 1
}

// localRoundPriorFindings are the findings of the last real LOCAL ROUND block.
// Unavailable lines are dropped first: land's parser reads any "LOCAL ROUND ("
// line as a header, so an unavailable line after a block would hide that block.
func localRoundPriorFindings(notes string) []land.Finding {
	var kept []string
	for _, line := range strings.Split(notes, "\n") {
		if localRoundUnavailableRE.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	r, ok := land.ParseLastLocalRound(strings.Join(kept, "\n"))
	if !ok {
		return nil
	}
	return r.Findings
}

// localRoundUnavailableLine is the answer to a request the round did not
// serve. The reason is one line, and the marker is defused so the reason
// cannot open a line of its own.
func localRoundUnavailableLine(n int, reason string) string {
	reason = strings.ReplaceAll(land.NoteField(reason), "LOCAL ROUND", "LOCAL-ROUND")
	return fmt.Sprintf("LOCAL ROUND (%d): unavailable - %s", n, reason)
}

// localRoundReviewReason names why a review returned no findings. The error
// text is not written: a reviewer error can carry om's output.
func localRoundReviewReason(err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("the om review did not finish within %v", timeout)
	}
	return "the om review failed"
}

// sortByCreated orders issues oldest first. A created time that does not parse
// sorts as the zero time, so such a bead is served first.
func sortByCreated(issues []*beads.Issue) {
	created := func(i *beads.Issue) time.Time {
		t, err := time.Parse(time.RFC3339Nano, i.CreatedAt)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	sort.SliceStable(issues, func(i, j int) bool {
		return created(issues[i]).Before(created(issues[j]))
	})
}

// startLocalRoundPatrols starts one local round patrol per rig, where the
// landing workers start (gt-ryp1ff). The switch is read each tick, so a town
// with the switch off runs the patrol and answers each request unavailable.
func (d *Daemon) startLocalRoundPatrols() {
	if d.config == nil || d.ctx == nil {
		return
	}
	for _, rigName := range d.getKnownRigs() {
		go d.runLocalRoundPatrol(rigName)
	}
}

// runLocalRoundPatrol ticks one rig's patrol until the daemon stops.
func (d *Daemon) runLocalRoundPatrol(rigName string) {
	p, err := d.newRigLocalRoundPatrol(rigName)
	if err != nil {
		d.logger.Printf("local_round: %s: not starting: %v", rigName, err)
		return
	}
	d.logger.Printf("local_round: %s: patrol started", rigName)
	ticker := time.NewTicker(localRoundInterval)
	defer ticker.Stop()
	for {
		p.Tick(d.ctx)
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// listLocalRoundRequests returns the beads a request is served from: one bd
// query for the open and in_progress beads, and never a closed one. A request
// is written by the seat or crew working the bead, so its bead is usually
// in_progress; "open" in the work item means the note is unserved, not the
// bead's status.
func listLocalRoundRequests(list func(statuses ...beads.IssueStatus) ([]*beads.Issue, error)) func() ([]*beads.Issue, error) {
	return func() ([]*beads.Issue, error) {
		return list(beads.StatusOpen, beads.StatusInProgress)
	}
}

// newRigLocalRoundPatrol wires the production seams for one rig. The reviewer
// is an OMReviewer with no verdict store, so a round writes no verdict.
func (d *Daemon) newRigLocalRoundPatrol(rigName string) (*LocalRoundPatrol, error) {
	townRoot := d.config.TownRoot
	rigPath := filepath.Join(townRoot, rigName)
	repoPath := constants.RigBareRepoPath(rigPath)
	if _, err := os.Stat(repoPath); err != nil {
		return nil, fmt.Errorf("rig repository %s: %w", repoPath, err)
	}
	remote, err := rig.ResolveLandingRemoteWith(d.repoFileOpener(), d.gitAt(repoPath), townRoot, rigName)
	if err != nil {
		return nil, err
	}
	target, err := rigDefaultBranch(rigPath)
	if err != nil {
		return nil, err
	}
	cfg := landingWorkerConfig(d.patrol())
	if cfg == nil {
		cfg = &LandingWorkerConfig{}
	}
	workRoot, err := landingWorkRoot(cfg.WorkRoot, townRoot, rigName)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(workRoot); err != nil {
		return nil, fmt.Errorf("local round work root: %w", err)
	}
	omDir := filepath.Join(townRoot, ".runtime", "local-round-om", rigName)
	if err := ensurePrivateDir(omDir); err != nil {
		return nil, fmt.Errorf("local round om dir: %w", err)
	}
	// An unknown home leaves om to the PATH lookup alone, which resolveOMPath
	// already does for an empty home: a missing om then makes each round
	// unavailable, never a clean review.
	home, err := os.UserHomeDir()
	if err != nil {
		d.logger.Printf("local_round: %s: home directory unknown; om is looked up on PATH only", rigName)
	}
	reviewer := land.OMReviewer{
		Path:    resolveOMPath(cfg.OMPath, exec.LookPath, home),
		OutDir:  omDir,
		Timeout: landingWorkerDuration(cfg.OMTimeoutStr, land.DefaultOMTimeout),
	}
	bd := beads.NewWithBeadsDir(rigPath, beads.ResolveBeadsDir(rigPath))
	g := git.NewGit(repoPath)
	return &LocalRoundPatrol{
		ListRequests: listLocalRoundRequests(bd.ListIssueStatuses),
		Comments:     bd.Comments,
		AppendNote:   bd.AppendNotes,
		Fetch:        localRoundFetcher(g, g, workRoot, remote, target, d.logger.Printf),
		Reviewer:     reviewer,
		Bound:        land.Bound{Dir: filepath.Join(townRoot, ".runtime", "local-round-bound", rigName)},
		Config:       d.patrol,
		Operator:     func() (*config.OperatorConfig, error) { return config.LoadOperatorConfig(townRoot) },
		Paused:       d.localRoundPaused,
		Now:          time.Now,
		Logf:         d.logger.Printf,
	}, nil
}

// localRoundPaused is why the landing worker's om is paused, or "" when no
// hold stands. A drain for a rebuild or an upgrade counts as a pause.
func (d *Daemon) localRoundPaused() string {
	if d.landingHolds.held() {
		return "a landing hold is on (an upgrade or a rebuild of om is pending)"
	}
	return d.landingPausedReason()
}

// localRoundGit is the slice of the rig repository a round's fetch uses. The
// production value is a *git.Git; a test passes a fake.
type localRoundGit interface {
	Rev(ref string) (string, error)
	UpdateRef(ref, commit string) error
	WorktreeAddDetached(path, ref string) error
	WorktreeRemove(path string, force bool) error
	MergeBase(a, b string) (string, error)
}

// localRoundBeadRefRE is the shape a bead id must have to name a ref: the id is
// written into refs/local-round/<bead>, so a slash, a space or ".." would name
// another ref.
var localRoundBeadRefRE = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)+(\.[0-9]+)*$`)

// localRoundBeadRefMax is the longest bead id the round writes into a ref name.
const localRoundBeadRefMax = 64

// errLocalRoundBeadRef refuses a bead id that cannot name a ref. The error is
// wrapped with the id, so the answer can name it.
var errLocalRoundBeadRef = errors.New("is not valid for a ref name")

// localRoundFetcher returns the production Fetch. The head must already be a
// commit in the rig repository, which a pushed polecat branch makes it; the
// round points refs/local-round/<bead> at it, never a landing ref, and checks
// it out in a detached worktree under workRoot. Its base is the merge base of
// the rig's default branch and the head. repo is the same repository as g,
// handed to the review as the repository that holds the base.
func localRoundFetcher(g localRoundGit, repo land.Repo, workRoot, remote, target string, logf func(string, ...any)) func(bead, head string) (localRoundTree, error) {
	return func(bead, head string) (localRoundTree, error) {
		if !localRoundBeadRefRE.MatchString(bead) || len(bead) > localRoundBeadRefMax {
			return localRoundTree{}, fmt.Errorf("bead id %q %w", bead, errLocalRoundBeadRef)
		}
		if _, err := g.Rev(head + "^{commit}"); err != nil {
			return localRoundTree{}, fmt.Errorf("head %s is not a commit in the rig repository; push the branch first", head)
		}
		if err := g.UpdateRef("refs/local-round/"+bead, head); err != nil {
			return localRoundTree{}, fmt.Errorf("update-ref of refs/local-round/%s failed", bead)
		}
		dir, err := os.MkdirTemp(workRoot, "local-round-")
		if err != nil {
			return localRoundTree{}, fmt.Errorf("creating the local round worktree dir under %s failed", workRoot)
		}
		// Cleanup removes only the worktree this round made. A repository-wide
		// prune is not this round's to run: it touches every worktree of the rig.
		// Cleanup logs the step that failed, not its error text, which can carry
		// a command's output.
		cleanup := func() {
			if err := g.WorktreeRemove(dir, true); err != nil {
				logf("local_round: %s: worktree remove of %s failed", bead, dir)
			}
			if err := os.RemoveAll(dir); err != nil {
				logf("local_round: %s: removing %s failed", bead, dir)
			}
		}
		if err := g.WorktreeAddDetached(dir, head); err != nil {
			cleanup()
			return localRoundTree{}, errors.New("worktree add of the head failed")
		}
		base, err := g.MergeBase(remote+"/"+target, head)
		if err != nil {
			cleanup()
			return localRoundTree{}, fmt.Errorf("merge-base of %s/%s and the head failed", remote, target)
		}
		return localRoundTree{Dir: dir, Base: base, Repo: repo, Cleanup: cleanup}, nil
	}
}
