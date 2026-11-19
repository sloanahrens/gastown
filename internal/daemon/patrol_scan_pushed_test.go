package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/session"
)

const (
	pushedRig     = "gastown"
	pushedPolecat = "capable"
	pushedBead    = "gt-hhh11"
	pushedBranch  = "polecat/capable/gt-hhh11"
)

// pushedTown is a temp town with one seat holding one work bead: the intent
// record names the bead, the work-bead read answers it, the seat's worktree
// resolves, and every git call is the returned stub.
type pushedTown struct {
	d    *Daemon
	h    *patrolScanHost
	git  *pushStubGit
	work *pushedBeads
}

func newPushedTown(t *testing.T) *pushedTown {
	t.Helper()
	d, _ := newOverrunDaemon(t, false)
	// An empty registry: no sandbox record names the seat, so the read takes the
	// host worktree, which the stub stands in for.
	d.prefixRegistryFn = func() *session.PrefixRegistry { return session.NewPrefixRegistry() }
	if _, err := intent.Update(d.config.TownRoot, pushedSeat(), func(r *intent.Record) error {
		r.WorkBead = pushedBead
		return nil
	}); err != nil {
		t.Fatalf("seed the seat's intent record: %v", err)
	}
	worktree := filepath.Join(d.config.TownRoot, pushedRig, "polecats", pushedPolecat, pushedRig)
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatalf("seed the seat's worktree: %v", err)
	}
	g := &pushStubGit{branch: pushedBranch, sha: "abc1234"}
	d.openGitFn = func(string) daemonGit { return g }
	work := &pushedBeads{work: &beads.Issue{ID: pushedBead, Status: "in_progress"}}
	d.openWorkBeads = func([]string, time.Duration) workBeadReader { return work }
	return &pushedTown{d: d, h: &patrolScanHost{d: d}, git: g, work: work}
}

func pushedSeat() intent.Seat {
	return intent.Seat{Rig: pushedRig, Role: constants.RolePolecat, Name: pushedPolecat}
}

// slingAt appends one dispatcher sling of the seat's bead to the town's event
// log at at, the clock PushedSinceSling dates a head from.
func (p *pushedTown) slingAt(t *testing.T, at time.Time) {
	t.Helper()
	line := fmt.Sprintf(`{"ts":%q,"source":"gt","type":%q,"actor":%q,"payload":{"bead":%q,"target":%q},"visibility":"audit"}`,
		at.UTC().Format(time.RFC3339), events.TypeSling, events.SpecDispatchActor,
		pushedBead, pushedRig+"/polecats/"+pushedPolecat)
	f, err := os.OpenFile(filepath.Join(p.h.town(), events.EventsFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open the event log: %v", err)
	}
	defer f.Close() //nolint:errcheck // test fixture
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("append the sling event: %v", err)
	}
}

// originHolds makes origin list the seat's branch at sha.
func (p *pushedTown) originHolds(sha string) {
	p.git.originRefs = []git.RemoteRef{{Hash: sha, Name: "refs/heads/" + p.git.branch}}
}

// pushedBeads is the work-bead reader PushedSinceSling reads: Show answers its
// own bead and an error for any other ID, so the sling read's agent-bead
// fallback fails unless a test stands in for it.
type pushedBeads struct {
	work *beads.Issue
	err  error
}

func (b *pushedBeads) Show(id string) (*beads.Issue, error) {
	if b.err != nil {
		return nil, b.err
	}
	if b.work != nil && id == b.work.ID {
		return b.work, nil
	}
	return nil, fmt.Errorf("bead not found: %s", id)
}

func (b *pushedBeads) List(beads.ListOptions) ([]*beads.Issue, error) {
	if b.err != nil {
		return nil, b.err
	}
	return nil, nil
}

// branchReadFails is a git handle whose branch read fails, the stand-in for a
// checkout git cannot name a branch in.
type branchReadFails struct{ daemonGit }

func (branchReadFails) CurrentBranch() (string, error) { return "", errors.New("not a worktree") }

// A branch origin holds whose head was committed after the sling is this run's
// work: the seat pushed and the head is the answer.
func TestPushedSinceSling_HeadFromThisRunIsTheAnswer(t *testing.T) {
	t.Parallel()
	p := newPushedTown(t)
	slung := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	p.slingAt(t, slung)
	p.git.commitTime = slung.Add(5 * time.Minute)
	p.originHolds("abc1234")

	got, err := p.h.PushedSinceSling(pushedRig, pushedPolecat)
	if err != nil {
		t.Fatalf("PushedSinceSling: %v", err)
	}
	if got != "abc1234" {
		t.Errorf("PushedSinceSling = %q, want abc1234", got)
	}
}

// A head committed before the sling is the branch the seat resumed, not work
// this run produced: no answer.
func TestPushedSinceSling_HeadBeforeTheSlingIsNoWork(t *testing.T) {
	t.Parallel()
	p := newPushedTown(t)
	slung := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	p.slingAt(t, slung)
	p.git.commitTime = slung.Add(-time.Hour)
	p.originHolds("abc1234")

	got, err := p.h.PushedSinceSling(pushedRig, pushedPolecat)
	if err != nil {
		t.Fatalf("PushedSinceSling: %v", err)
	}
	if got != "" {
		t.Errorf("PushedSinceSling = %q, want \"\"", got)
	}
}

// An origin head that does not descend from the local tip is a history this
// run did not produce: no answer, and not an error.
func TestPushedSinceSling_DivergedOriginHeadIsNoWork(t *testing.T) {
	t.Parallel()
	p := newPushedTown(t)
	slung := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	p.slingAt(t, slung)
	p.git.commitTime = slung.Add(5 * time.Minute)
	p.originHolds("def5678")

	got, err := p.h.PushedSinceSling(pushedRig, pushedPolecat)
	if err != nil {
		t.Fatalf("PushedSinceSling: %v", err)
	}
	if got != "" {
		t.Errorf("PushedSinceSling = %q, want \"\"", got)
	}
}

// A branch origin does not hold is a seat that has not pushed it: no answer,
// and not an error.
func TestPushedSinceSling_BranchMissingOnOriginIsNoWork(t *testing.T) {
	t.Parallel()
	p := newPushedTown(t)
	slung := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	p.slingAt(t, slung)
	p.git.commitTime = slung.Add(5 * time.Minute)

	got, err := p.h.PushedSinceSling(pushedRig, pushedPolecat)
	if err != nil {
		t.Fatalf("PushedSinceSling: %v", err)
	}
	if got != "" {
		t.Errorf("PushedSinceSling = %q, want \"\"", got)
	}
}

// A sling the log and the agent bead cannot date leaves the head undated, which
// is an error: "" would read as "this run pushed nothing".
func TestPushedSinceSling_UnknownSlingTimeIsAnError(t *testing.T) {
	t.Parallel()
	p := newPushedTown(t)
	p.git.commitTime = time.Date(2026, 11, 18, 12, 5, 0, 0, time.UTC)
	p.originHolds("abc1234")

	got, err := p.h.PushedSinceSling(pushedRig, pushedPolecat)
	if err == nil || got != "" {
		t.Fatalf("PushedSinceSling = %q, %v; want \"\" and an error", got, err)
	}
	if !strings.Contains(err.Error(), "sling clock") {
		t.Errorf("error %q does not name the sling clock", err)
	}
}

// An origin that cannot be asked is an error: "" would read as an unpushed
// branch.
func TestPushedSinceSling_UnreadableOriginIsAnError(t *testing.T) {
	t.Parallel()
	p := newPushedTown(t)
	slung := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	p.slingAt(t, slung)
	p.git.commitTime = slung.Add(5 * time.Minute)
	p.git.originErr = errors.New("could not resolve host: origin")

	got, err := p.h.PushedSinceSling(pushedRig, pushedPolecat)
	if err == nil || got != "" {
		t.Fatalf("PushedSinceSling = %q, %v; want \"\" and an error", got, err)
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Errorf("error %q does not name origin", err)
	}
}

// Every input the read cannot get at is an error, never the "" that reads as an
// unpushed branch.
func TestPushedSinceSling_UnreadableInputsAreErrors(t *testing.T) {
	t.Parallel()
	slung := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		// broke breaks the fixture's input and returns what the error must name.
		broke func(t *testing.T, p *pushedTown) string
	}{
		{
			name: "intent record",
			broke: func(t *testing.T, p *pushedTown) string {
				path := pushedSeat().Path(p.h.town())
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
				return "intent record"
			},
		},
		{
			name: "no work bead",
			broke: func(t *testing.T, p *pushedTown) string {
				if _, err := intent.Update(p.h.town(), pushedSeat(), func(r *intent.Record) error {
					r.WorkBead = ""
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return "holds no work bead"
			},
		},
		{
			name: "assigned work",
			broke: func(t *testing.T, p *pushedTown) string {
				if _, err := intent.Update(p.h.town(), pushedSeat(), func(r *intent.Record) error {
					r.WorkBead = ""
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				p.work.err = errors.New("bd: timed out")
				return "work assigned to"
			},
		},
		{
			name: "work bead",
			broke: func(t *testing.T, p *pushedTown) string {
				p.work.err = errors.New("bd: timed out")
				return "work bead"
			},
		},
		{
			name: "worktree",
			broke: func(t *testing.T, p *pushedTown) string {
				p.d.overrunSeatGitFn = func(overrunSeat) (daemonGit, error) {
					return nil, errors.New("no sandbox runner")
				}
				return "worktree"
			},
		},
		{
			name: "branch read",
			broke: func(t *testing.T, p *pushedTown) string {
				p.d.openGitFn = func(string) daemonGit { return branchReadFails{p.git} }
				return "read the seat's branch"
			},
		},
		{
			name: "branch",
			broke: func(t *testing.T, p *pushedTown) string {
				p.git.branch = ""
				return "branch"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPushedTown(t)
			p.slingAt(t, slung)
			p.git.commitTime = slung.Add(5 * time.Minute)
			p.originHolds("abc1234")
			want := tc.broke(t, p)

			got, err := p.h.PushedSinceSling(pushedRig, pushedPolecat)
			if err == nil || got != "" {
				t.Fatalf("PushedSinceSling = %q, %v; want \"\" and an error", got, err)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name the %s", err, want)
			}
		})
	}
}

// failingGit is the git handle with the named reads failing: the stand-in for
// a read that fails after origin has already answered.
type failingGit struct {
	daemonGit
	rev, hasObject, isAncestor error
}

func (g failingGit) Rev(ref string) (string, error) {
	if g.rev != nil {
		return "", g.rev
	}
	return g.daemonGit.Rev(ref)
}

func (g failingGit) HasObject(sha string) (bool, error) {
	if g.hasObject != nil {
		return false, g.hasObject
	}
	return g.daemonGit.HasObject(sha)
}

func (g failingGit) IsAncestor(ancestor, descendant string) (bool, error) {
	if g.isAncestor != nil {
		return false, g.isAncestor
	}
	return g.daemonGit.IsAncestor(ancestor, descendant)
}

// A read that fails after origin has answered is an error, never the "" that
// reads as an unpushed branch. Origin's head differs from the local tip and
// descends from it, so every read below is reached.
func TestPushedSinceSling_FailedHeadReadsAreErrors(t *testing.T) {
	t.Parallel()
	slung := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	boom := errors.New("bd: git: transport failed")
	for _, tc := range []struct {
		name string
		// broke breaks one read of the fixture and returns what the error must name.
		broke func(p *pushedTown) string
	}{
		{
			name: "local head read",
			broke: func(p *pushedTown) string {
				p.d.openGitFn = func(string) daemonGit { return failingGit{daemonGit: p.git, rev: boom} }
				return "read the local head"
			},
		},
		{
			name: "local head empty",
			broke: func(p *pushedTown) string {
				p.git.sha = ""
				return "local head"
			},
		},
		{
			name: "origin head fetched check",
			broke: func(p *pushedTown) string {
				p.d.openGitFn = func(string) daemonGit { return failingGit{daemonGit: p.git, hasObject: boom} }
				return "is fetched"
			},
		},
		{
			name: "origin head fetch",
			broke: func(p *pushedTown) string {
				p.git.hasObject = nil
				p.git.fetchErr = boom
				return "fetch origin's"
			},
		},
		{
			name: "ancestry check",
			broke: func(p *pushedTown) string {
				p.d.openGitFn = func(string) daemonGit { return failingGit{daemonGit: p.git, isAncestor: boom} }
				return "descends"
			},
		},
		{
			name: "commit time",
			broke: func(p *pushedTown) string {
				p.git.commitTime = time.Time{}
				return "commit time"
			},
		},
		{
			name: "origin listing",
			broke: func(p *pushedTown) string {
				p.git.originErr = boom
				return "read origin's"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPushedTown(t)
			p.slingAt(t, slung)
			p.git.commitTime = slung.Add(5 * time.Minute)
			p.originHolds("def5678")
			p.git.ancestors = map[string]bool{"abc1234 def5678": true}
			p.git.hasObject = map[string]bool{"def5678": true}
			want := tc.broke(p)

			got, err := p.h.PushedSinceSling(pushedRig, pushedPolecat)
			if err == nil || got != "" {
				t.Fatalf("PushedSinceSling = %q, %v; want \"\" and an error", got, err)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		})
	}
}
