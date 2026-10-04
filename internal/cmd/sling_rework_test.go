package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// reworkSlingFake is namedSlingFake plus what the rework path asks of the
// seat: the options each ReuseIdlePolecat call carried, and seats whose reuse
// the workstate gate refuses unless recovery was allowed — the verdict the
// real manager reaches for a seat with unmerged commits on its branch
// (gt-lid6d).
type reworkSlingFake struct {
	*namedSlingFake
	reuseOpts      map[string]polecat.AddOptions
	recoveryNeeded map[string]bool
}

func (f *reworkSlingFake) ReuseIdlePolecat(name string, opts polecat.AddOptions) (*polecat.Polecat, error) {
	if f.reuseOpts == nil {
		f.reuseOpts = map[string]polecat.AddOptions{}
	}
	f.reuseOpts[name] = opts
	f.reuseNames = append(f.reuseNames, name)
	if f.recoveryNeeded[name] && !opts.AllowRecovery {
		return nil, fmt.Errorf("%w: unmerged commits on %s", polecat.ErrPolecatNeedsRecovery, opts.ResumeBranch)
	}
	if err := f.reuseErr[name]; err != nil {
		return nil, err
	}
	return f.polecats[name], nil
}

func newReworkSlingFake() *reworkSlingFake {
	fake := newReworkSlingFakeBare()
	fake.polecats["garnet"].Branch = reworkBranch
	return fake
}

func newReworkSlingFakeBare() *reworkSlingFake {
	agate := &polecat.Polecat{Name: "agate", ClonePath: "/town/rig/polecats/agate/rig"}
	return &reworkSlingFake{
		namedSlingFake: &namedSlingFake{
			polecats: map[string]*polecat.Polecat{
				"garnet": {Name: "garnet", ClonePath: "/town/rig/polecats/garnet/rig", Branch: reworkBranch},
				"agate":  agate,
			},
			// The pool would offer agate: a rework that cannot have its author
			// must land there, exactly as it did before gt-lid6d.
			idle:     agate,
			reuseErr: map[string]error{},
		},
		reuseOpts:      map[string]polecat.AddOptions{},
		recoveryNeeded: map[string]bool{},
	}
}

const (
	reworkBead   = "gt-rw1"
	reworkBranch = "polecat/garnet/gt-rw1+muu1"
)

// reworkIdleReuseEnv is the idle-reuse path whose bead source answers with
// seats, and nothing for the beads it does not name.
func reworkIdleReuseEnv(seats map[string]*reworkSeat) idleReuseEnv {
	env := fakeIdleReuseEnv()
	env.reworkSeat = func(beadID string) (*reworkSeat, bool) {
		seat, ok := seats[beadID]
		return seat, ok
	}
	return env
}

// AuthorSeat labels garnet idle and free, or as the test needs it.
func authorSeat(mutate func(*polecat.Polecat)) *reworkSlingFake {
	fake := newReworkSlingFake()
	fake.polecats["garnet"].State = polecat.StateIdle
	if mutate != nil {
		mutate(fake.polecats["garnet"])
	}
	return fake
}

func reuseForRework(t *testing.T, fake idlePolecatReuse, opts SlingSpawnOptions, seats map[string]*reworkSeat) (*SpawnedPolecatInfo, error) {
	t.Helper()
	return reuseIdlePolecatForSlingWith(fake, reworkIdleReuseEnv(seats), "rig", opts, func() {})
}

// TestReworkSling_GoesBackToItsAuthorOnItsBranch: the polecat that built the
// rejected branch is reused, on that branch, ahead of the pool's offer.
func TestReworkSling_GoesBackToItsAuthorOnItsBranch(t *testing.T) {
	t.Parallel()
	fake := authorSeat(nil)

	info, err := reuseForRework(t, fake, SlingSpawnOptions{HookBead: reworkBead},
		map[string]*reworkSeat{reworkBead: {PolecatName: "garnet", Branch: reworkBranch}})
	if err != nil {
		t.Fatalf("rework reuse failed: %v", err)
	}
	if info == nil || info.PolecatName != "garnet" {
		t.Fatalf("rework got %+v; want polecat garnet", info)
	}
	if info.Branch != reworkBranch {
		t.Errorf("branch = %q; want the rejected attempt's branch %q", info.Branch, reworkBranch)
	}
	if fake.findCalls != 0 {
		t.Errorf("rework consulted the idle pool (%d calls); the author's seat was free", fake.findCalls)
	}
	got := fake.reuseOpts["garnet"]
	if got.ResumeBranch != reworkBranch {
		t.Errorf("ResumeBranch = %q; want %q", got.ResumeBranch, reworkBranch)
	}
	if !got.AllowRecovery {
		t.Error("reuse did not allow recovery; a rework's own unmerged commits would refuse it")
	}
}

// TestReworkSling_NeedsRecoveryAuthorIsResumed: a seat the workstate gate
// reads as NEEDS_RECOVERY for the attempt's unmerged commits is still reused.
func TestReworkSling_NeedsRecoveryAuthorIsResumed(t *testing.T) {
	t.Parallel()
	fake := authorSeat(func(p *polecat.Polecat) { p.Issue = reworkBead })
	fake.recoveryNeeded["garnet"] = true

	info, err := reuseForRework(t, fake, SlingSpawnOptions{HookBead: reworkBead},
		map[string]*reworkSeat{reworkBead: {PolecatName: "garnet", Branch: reworkBranch}})
	if err != nil {
		t.Fatalf("rework reuse failed: %v", err)
	}
	if info == nil || info.PolecatName != "garnet" {
		t.Fatalf("rework got %+v; want its own author garnet", info)
	}
	if fake.findCalls != 0 {
		t.Errorf("needs-recovery author was skipped for the pool (%d FindIdlePolecat calls)", fake.findCalls)
	}
}

// TestReworkSling_UnavailableAuthorFallsBack: an author the sling must not
// displace — busy, gone, or holding another bead — takes no part in the
// dispatch; the pool's seat is used instead, on a fresh branch, exactly as
// before gt-lid6d.
func TestReworkSling_UnavailableAuthorFallsBack(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		mutate func(*polecat.Polecat)
		seat   *reworkSeat
	}{
		{
			name:   "author busy",
			mutate: func(p *polecat.Polecat) { p.State = polecat.StateWorking },
			seat:   &reworkSeat{PolecatName: "garnet", Branch: reworkBranch},
		},
		{
			name:   "author gone",
			mutate: nil, // garnet is not on disk; the seat names flint
			seat:   &reworkSeat{PolecatName: "flint", Branch: "polecat/flint/gt-rw1+muu1"},
		},
		{
			name:   "author holds another bead",
			mutate: func(p *polecat.Polecat) { p.Issue = "gt-other" },
			seat:   &reworkSeat{PolecatName: "garnet", Branch: reworkBranch},
		},
		{
			name:   "author is on another branch",
			mutate: func(p *polecat.Polecat) { p.Branch = "polecat/garnet/gt-other+muu9" },
			seat:   &reworkSeat{PolecatName: "garnet", Branch: reworkBranch},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newReworkSlingFake()
			fake.polecats["garnet"].State = polecat.StateIdle
			if tt.mutate != nil {
				tt.mutate(fake.polecats["garnet"])
			}

			info, err := reuseForRework(t, fake, SlingSpawnOptions{HookBead: reworkBead},
				map[string]*reworkSeat{reworkBead: tt.seat})
			if err != nil {
				t.Fatalf("rework fell back with an error: %v", err)
			}
			if info == nil || info.PolecatName != "agate" {
				t.Fatalf("rework got %+v; want the pool's agate", info)
			}
			if got := fake.reuseOpts["agate"]; got.ResumeBranch != "" || got.AllowRecovery {
				t.Errorf("fallback reuse carried %+v; want a fresh branch with no recovery allowance", got)
			}
		})
	}
}

// TestReworkSling_RefusedReuseAllocatesFresh: when the author's seat itself
// refuses the resume — parked, or a branch that is no longer on origin — the
// sling returns no reuse at all, which is the fresh-polecat path a refused
// idle reuse has always taken. It never stops the sling.
func TestReworkSling_RefusedReuseAllocatesFresh(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		cause error
	}{
		{"author parked", fmt.Errorf("%w: %w: parked (operator parked)", polecat.ErrPolecatNeedsRecovery, polecat.ErrPolecatParked)},
		{"author's branch is gone", fmt.Errorf("start point origin/%s not found", reworkBranch)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := authorSeat(nil)
			fake.reuseErr["garnet"] = tt.cause

			info, err := reuseForRework(t, fake, SlingSpawnOptions{HookBead: reworkBead},
				map[string]*reworkSeat{reworkBead: {PolecatName: "garnet", Branch: reworkBranch}})
			if err != nil {
				t.Fatalf("refused rework reuse returned an error: %v", err)
			}
			if info != nil {
				t.Fatalf("refused rework reuse returned %+v; want nil so the caller allocates fresh", info)
			}
			if fake.findCalls != 0 {
				t.Errorf("refused reuse fell through to the idle pool after trying the author")
			}
		})
	}
}

// TestReworkSling_NoPreferenceDispatchesAsToday: a bead that is not rework,
// or one whose submission names no polecat, takes the plain idle pool.
func TestReworkSling_NoPreferenceDispatchesAsToday(t *testing.T) {
	t.Parallel()
	fake := newReworkSlingFake()

	info, err := reuseForRework(t, fake, SlingSpawnOptions{HookBead: reworkBead}, nil)
	if err != nil {
		t.Fatalf("plain sling failed: %v", err)
	}
	if info == nil || info.PolecatName != "agate" {
		t.Fatalf("got %+v; want the pool's agate", info)
	}
	if fake.findCalls != 1 {
		t.Errorf("FindIdlePolecat calls = %d; want 1", fake.findCalls)
	}
	if got := fake.reuseOpts["agate"]; got.ResumeBranch != "" || got.AllowRecovery {
		t.Errorf("plain sling reuse carried %+v; want a fresh branch", got)
	}
}

// TestReworkSeatFrom pins the read of the submission: the newest comment
// wins, a READY TO LAND block is the fallback, and anything else names no
// polecat.
func TestReworkSeatFrom(t *testing.T) {
	t.Parallel()
	ready := "READY TO LAND\nBranch: polecat/pearl/gt-rw1+muu2\nHead: 0123abcd\nTarget: main\nWorker: pearl"
	for _, tt := range []struct {
		name     string
		labels   []string
		notes    string
		comments []string
		want     *reworkSeat
	}{
		{
			name:     "latest submission wins",
			labels:   []string{"rework"},
			comments: []string{"Submitted for landing: polecat/agate/gt-rw1+muu1 @ 1111aaaa onto main (attempt 1)", "Submitted for landing: polecat/pearl/gt-rw1+muu2 @ 2222bbbb onto main (attempt 2)"},
			want:     &reworkSeat{PolecatName: "pearl", Branch: "polecat/pearl/gt-rw1+muu2"},
		},
		{
			name:   "ready-to-land block is the fallback",
			labels: []string{"rework"},
			notes:  ready,
			want:   &reworkSeat{PolecatName: "pearl", Branch: "polecat/pearl/gt-rw1+muu2"},
		},
		{
			name:   "not rework",
			labels: []string{"gt:ready-to-land"},
			notes:  ready,
		},
		{
			name:   "no submission",
			labels: []string{"rework"},
		},
		{
			name:     "submission names no polecat branch",
			labels:   []string{"rework"},
			comments: []string{"Submitted for landing: land/gt-rw1 @ 1111aaaa onto main (attempt 1)"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			comments := make([]beads.Comment, 0, len(tt.comments))
			for _, c := range tt.comments {
				comments = append(comments, beads.Comment{Text: c})
			}
			issue := &beads.Issue{ID: reworkBead, Labels: tt.labels, Notes: tt.notes}

			got, ok := reworkSeatFrom(issue, comments)
			if (tt.want == nil) != !ok {
				t.Fatalf("ok = %v; want %v (got %+v)", ok, tt.want != nil, got)
			}
			if tt.want != nil && (got.PolecatName != tt.want.PolecatName || got.Branch != tt.want.Branch) {
				t.Fatalf("got %+v; want %+v", got, tt.want)
			}
		})
	}
}

// TestPolecatFromBranch: only polecat/<name>/... names a polecat.
func TestPolecatFromBranch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		branch string
		name   string
		ok     bool
	}{
		{"polecat/garnet/gt-rw1+muu1", "garnet", true},
		{"polecat/garnet", "", false},
		{"polecats/garnet/x", "", false},
		{"land/gt-rw1", "", false},
		{"polecat//gt-rw1+muu1", "", false},
		{"", "", false},
	} {
		name, ok := polecatFromBranch(tt.branch)
		if ok != tt.ok || name != tt.name {
			t.Errorf("polecatFromBranch(%q) = (%q, %v); want (%q, %v)", tt.branch, name, ok, tt.name, tt.ok)
		}
	}
}

// TestReworkPreference_SkipsNamedSling: a named sling names its own seat; the
// rework preference must not redirect it.
func TestReworkPreference_SkipsNamedSling(t *testing.T) {
	t.Parallel()
	env := reworkIdleReuseEnv(map[string]*reworkSeat{reworkBead: {PolecatName: "garnet", Branch: reworkBranch}})
	if seat, ok := reworkPreference(env, SlingSpawnOptions{Name: "agate", HookBead: reworkBead}); ok {
		t.Fatalf("named sling took the rework preference %+v", seat)
	}
	if _, ok := reworkPreference(env, SlingSpawnOptions{HookBead: reworkBead}); !ok {
		t.Error("unnamed sling of a rework bead took no preference")
	}
	if seat, ok := reworkPreference(env, SlingSpawnOptions{HookBead: reworkBead, ResumeBranch: "topic/x"}); ok {
		t.Fatalf("sling with an explicit resume branch took the rework preference %+v", seat)
	}
}

// TestReworkSeatUnavailable covers the eligibility fence in one place.
func TestReworkSeatUnavailable(t *testing.T) {
	t.Parallel()
	seat := &reworkSeat{PolecatName: "garnet", Branch: reworkBranch}
	fake := newReworkSlingFake()
	fake.polecats["garnet"].State = polecat.StateIdle

	if why := reworkSeatUnavailable(fake, seat, reworkBead); why != "" {
		t.Errorf("idle author refused: %q", why)
	}
	fake.polecats["garnet"].Branch = "polecat/garnet/gt-other+muu9"
	if why := reworkSeatUnavailable(fake, seat, reworkBead); !strings.Contains(why, "is not on") {
		t.Errorf("author on another branch allowed: %q", why)
	}
	fake.polecats["garnet"].Branch = reworkBranch
	fake.polecats["garnet"].State = polecat.StateWorking
	if why := reworkSeatUnavailable(fake, seat, reworkBead); !strings.Contains(why, "working") {
		t.Errorf("busy author allowed: %q", why)
	}
	fake.polecats["garnet"].State = polecat.StateIdle
	fake.polecats["garnet"].Issue = "gt-other"
	if why := reworkSeatUnavailable(fake, seat, reworkBead); !strings.Contains(why, "gt-other") {
		t.Errorf("author holding another bead allowed: %q", why)
	}
}
