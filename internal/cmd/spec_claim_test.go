package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The two beads gt-n6jbw is about, transcribed from the fractals rig. fr-zdzs
// was attached at 15:54 and fr-z64c at 15:55, and both rewrote the final
// `repeats < hops.length / 10` assertion of wanderHops.test.ts with opposite
// fixes. The texts are kept verbatim, including the attached_* header the
// dispatcher reads and the two spellings of the file (fr-zdzs names it bare,
// fr-z64c names its path): a check that compared only the spellings would pass
// this test while missing the pair it replays.
const (
	// fr-zdzs landed the provable bound; it spells the file bare.
	frZdssTitle = "Tour tests: share, reach and repeat-rate hold for any number of fractals (place-weighted bounds, no seed picked)"
	frZdssDesc  = `attached_molecule: fr-wisp-7ja
attached_formula: mol-polecat-work
attached_at: 2026-10-31T15:54:43.916826Z
dispatched_by: daemon/spec-dispatch

## Goal
Make three tour and wander tests hold for any number of fractals and any number of places per fractal, so adding the thirteenth, fourteenth or fifteenth fractal cannot turn them red. Today each passes only because of the seed it uses.

3. wanderHops.test.ts "never go to the place they are on, or back inside the recent window" ends with ` + "`repeats < hops.length / 10`" + ` (seed 23, 600 hops). On clean main it passes by three: 57 repeats against a limit of 60.

## Constraints
- Read ~/.claude/docs/standards/test-engineering.md first. Never raise a timeout, skip a test or loosen a limit to land.
- Share test, in src/lib/__tests__/tour.test.ts: one walk, shared by the share and the reach test.
- wanderHops.test.ts: keep the per-hop assertions. Replace the final aggregate ` + "`repeats < hops.length / 10`" + ` with the provable bound: a repeat needs the window to cover every other place of the chosen fractal, so it can only happen on a fractal that has at most TOUR_RECENT_LIMIT + 1 places.

## Out of scope
- tour.ts, wander.ts and any other source file, places, presets, previews and every other test file.

## Gate
make gate (run by the landing worker's CI).

## Size
One worker, one landing.`

	// fr-z64c would have pooled the repeat rate over seeds; it names the path.
	frZ64cTitle = "Found: wanderHops repeat-rate bound is a one-seed fit and fails for 95 of 200 seeds on main"
	frZ64cDesc  = `attached_molecule: fr-wisp-hn6
attached_formula: mol-polecat-work
attached_at: 2026-10-31T15:55:42.437139Z
dispatched_by: daemon/spec-dispatch

## Goal
` + "`wanderHops.test.ts`" + `'s repeat-rate assertion holds for the walk the catalogue actually produces, so that adding or moving a catalogue place does not flip it on one seed's luck, and its comment names the measured distribution.

## Constraints
- ` + "`src/lib/__tests__/wanderHops.test.ts`" + ` only. Keep the per-repeat causal assertion (a repeat may happen only when the fractal it chose has no place outside the recent window) as it stands.
- Do not raise the bound to pass a run. The finding is that the assertion is a one-seed fit, not that the walk got worse.
- Record the measured distribution in the comment, and the seeds the assertion covers.

## Out of scope
- The tour engine, the wander controller, the catalogue. This is the test's statistics, not the walk.

## Gate
` + "`make gate`" + ` (the landing gate runs it).

## Size
one worker, one landing.`
)

// frSpec is one of the pair as a ready candidate: type task, open, unlabelled.
func frSpec(id, title, description, created string) specdispatch.Spec {
	return specdispatch.Spec{
		ID: id, Title: title, Type: "task", Status: "open", Priority: 1,
		CreatedAt: created, Description: description,
	}
}

// peerClaims reads the pool's claims the way a tick does: through the duplicate
// guard's pool over a bd shaped like production, reduced by claimOf.
func peerClaims(t *testing.T, issues ...beads.Issue) []specdispatch.Claimant {
	t.Helper()
	db := beadsfake.New()
	db.Seed(issues...)
	pool, err := fetchDuplicatePoolFrom(db, func(string, error) {}, t.TempDir())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	peers := make([]specdispatch.Claimant, 0, len(pool))
	for _, row := range pool {
		peers = append(peers, claimOf(row))
	}
	if len(peers) != len(issues) {
		t.Fatalf("pool holds %d beads, want %d", len(peers), len(issues))
	}
	return peers
}

// frPeerClaims is the pair as one tick sees it: both open, so each is the
// other's peer.
func frPeerClaims(t *testing.T) []specdispatch.Claimant {
	t.Helper()
	return peerClaims(t,
		beads.Issue{ID: "fr-zdzs", Title: frZdssTitle, Status: "open", Priority: 1,
			CreatedAt: "2026-10-31T15:00:00Z", Description: frZdssDesc},
		beads.Issue{ID: "fr-z64c", Title: frZ64cTitle, Status: "open", Priority: 1,
			CreatedAt: "2026-10-31T09:00:00Z", Description: frZ64cDesc},
	)
}

// TestSpecDispatchHoldsTheSecondBeadOfATargetClaim is gt-n6jbw's acceptance
// bar: the fr-z64c/fr-zdzs pair, in dispatch order, spends one seat, and the
// second bead is held naming the first rather than slung onto the same
// assertion. A third bead that names no file still dispatches.
func TestSpecDispatchHoldsTheSecondBeadOfATargetClaim(t *testing.T) {
	t.Parallel()
	peers := frPeerClaims(t)
	noFile := cleanSpec("fr-nofile", 1, "2026-10-31T08:00:00Z")

	f := newFakeSpecTown(
		noFile,
		frSpec("fr-z64c", frZ64cTitle, frZ64cDesc, "2026-10-31T09:00:00Z"),
		frSpec("fr-zdzs", frZdssTitle, frZdssDesc, "2026-10-31T15:00:00Z"),
	)
	f.claims = map[string][]specdispatch.Claimant{
		"fr-nofile": peers,
		"fr-z64c":   peers,
		"fr-zdzs":   peers,
	}
	env := f.env()
	// The real pair's acceptance lists predate the lint's three-item minimum,
	// and this test is about the claim rule, not their shape.
	env.ShapeGate = specdispatch.ShapeGateOff
	env.PerTick = 3

	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "fr-nofile fr-z64c" {
		t.Fatalf("slung %q, want the file-less bead and the first of the pair", got)
	}
	if len(r.Dispatched) != 2 {
		t.Fatalf("dispatched = %+v, want two", r.Dispatched)
	}
	var held string
	for _, e := range r.Skipped {
		if e.Bead == "fr-zdzs" {
			held = e.Line
		}
	}
	if held == "" {
		t.Fatalf("fr-zdzs was not held: skipped %+v", r.Skipped)
	}
	for _, want := range []string{"held:", "fr-z64c", "wanderHops.test.ts"} {
		if !strings.Contains(held, want) {
			t.Errorf("hold line %q should name %q", held, want)
		}
	}
}

// TestSpecDispatchDoesNotHoldForAPeerTheTickNeverSlings bounds the open-peer
// half of the rule: an open bead the board carries is a claim only when the
// tick gets it onto a seat. A bead held for planning is never slung, so holding
// its same-file candidate would hold that file for as long as the plan waits
// (gt-n6jbw).
func TestSpecDispatchDoesNotHoldForAPeerTheTickNeverSlings(t *testing.T) {
	t.Parallel()
	const claimFile = "src/lib/__tests__/wanderHops.test.ts"
	body := "- `" + claimFile + "` only."
	desc := specConstraints(&body)

	planning := cleanSpec("fr-z64c", 1, "2026-10-31T09:00:00Z")
	planning.Description = desc
	planning.Labels = []string{specdispatch.NeedsPlanningLabel}
	candidate := cleanSpec("fr-zdzs", 1, "2026-10-31T15:00:00Z")
	candidate.Description = desc

	peers := []specdispatch.Claimant{{
		ID: "fr-z64c", Status: "open", Priority: 1, CreatedAt: "2026-10-31T09:00:00Z",
		Paths: []string{claimFile},
	}}
	f := newFakeSpecTown(planning, candidate)
	f.claims = map[string][]specdispatch.Claimant{
		"fr-z64c": peers,
		"fr-zdzs": peers,
	}
	// Both beads clear the lint's path check: the file they name is on the
	// branch their rig lands on.
	f.targets = map[string]*specdispatch.TargetTree{
		"gastown": {Branch: "main", Files: map[string]struct{}{claimFile: {}}},
	}
	env := f.env()
	env.ShapeGate = specdispatch.ShapeGateWarn
	env.PerTick = 2

	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "fr-zdzs" {
		t.Fatalf("slung %q, want the shaped candidate: a peer the tick never slates holds nothing (skipped %+v)", got, r.Skipped)
	}
	if len(r.Planning) != 1 || r.Planning[0].Bead != "fr-z64c" {
		t.Fatalf("planning = %+v, want fr-z64c held for its plan", r.Planning)
	}
}

// TestSpecDispatchHoldsAFiledBeforeLandingCandidate drives the landed half of
// the rule through the production pool: a bead written before a landing on its
// test file is held, and one written after the landing dispatches.
func TestSpecDispatchHoldsAFiledBeforeLandingCandidate(t *testing.T) {
	t.Parallel()
	landed := beads.Issue{
		ID: "fr-landed", Title: "Tour tests hold for any number of fractals", Status: "closed",
		Priority: 1, CreatedAt: "2026-10-31T09:00:00Z", ClosedAt: "2026-10-31T17:00:00Z",
		Description: frZdssDesc, CloseReason: "Landed on main\ntarget_branch: main\ncommit_sha: 5be95dd",
	}
	peers := peerClaims(t, landed)
	if !peers[0].Landed {
		t.Fatalf("the pool lost the landing: %+v", peers[0])
	}

	for _, tc := range []struct {
		name    string
		created string
		slung   bool
	}{
		{name: "a bead filed before the landing is held", created: "2026-10-31T12:00:00Z"},
		{name: "a bead filed after the landing dispatches", created: "2026-10-31T18:00:00Z", slung: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(frSpec("fr-new", frZ64cTitle, frZ64cDesc, tc.created))
			f.claims = map[string][]specdispatch.Claimant{"fr-new": peers}
			env := f.env()
			env.ShapeGate = specdispatch.ShapeGateOff

			r := runSpecDispatchCycle(env)

			if got := len(f.slung) == 1; got != tc.slung {
				t.Fatalf("slung %v, want slung=%v (%+v)", f.slung, tc.slung, r)
			}
			if !tc.slung {
				if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "landed after this bead was filed") {
					t.Fatalf("skipped = %+v, want the landing hold", r.Skipped)
				}
			}
		})
	}
}

// TestSpecTargetClaimsFailsOnAnUnreadableRig is the production half of the
// no-fail-open rule: the claim set the tick reads is an error when the rig's
// beads directory cannot be resolved or its pool will not load, which the tick
// turns into a hold.
func TestSpecTargetClaimsFailsOnAnUnreadableRig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		beadsDir string
		pool     func(string) ([]duplicateCandidate, error)
		reason   string
	}{
		{
			name:   "a rig whose beads directory does not resolve",
			reason: "cannot resolve the beads directory of fr-z64c",
		},
		{
			name:     "a pool that will not load",
			beadsDir: "/town/fractals/mayor/rig/.beads",
			pool: func(string) ([]duplicateCandidate, error) {
				return nil, errors.New("bd: no database")
			},
			reason: "cannot read the beads of /town/fractals/mayor/rig/.beads: bd: no database",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := tc.pool
			if pool == nil {
				pool = func(string) ([]duplicateCandidate, error) {
					t.Fatal("a rig with no beads directory must not reach the pool")
					return nil, nil
				}
			}
			_, err := specTargetClaimsFrom("fr-z64c", tc.beadsDir, pool)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error %q should contain %q", err, tc.reason)
			}
		})
	}
}

// droppedShow answers a batched show without the beads named in drop, and
// refuses the whole batch when err is set: the two ways one peer's design and
// notes go unread while the pool still lists it.
type droppedShow struct {
	beads.Client
	drop map[string]bool
	err  error
}

func (s droppedShow) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if s.err != nil {
		return nil, s.err
	}
	issues, err := s.Client.ShowMultiple(ids)
	if err != nil {
		return nil, err
	}
	for id := range s.drop {
		delete(issues, id)
	}
	return issues, nil
}

// TestSpecDispatchHoldsForAPeerWhoseClaimWasNotRead is the fail-closed half of
// the partial-read finding: a pool that lists a peer but never read its design
// and notes must hold the candidate that names a test file, not read the peer
// as claiming nothing. The same pool read to completion, where the peer names
// no file, dispatches (gt-n6jbw).
func TestSpecDispatchHoldsForAPeerWhoseClaimWasNotRead(t *testing.T) {
	t.Parallel()
	peer := beads.Issue{ID: "fr-z64c", Title: "Pool the repeat rate over many seeds", Status: "in_progress",
		Assignee: "fractals/polecats/cantor", Priority: 1, CreatedAt: "2026-10-31T09:00:00Z",
		Description: "Pool the repeat rate of the walk over many seeds."}
	candidate := beads.Issue{ID: "fr-zdzs", Title: frZ64cTitle, Status: "open", Priority: 1,
		CreatedAt: "2026-10-31T15:00:00Z", Description: frZ64cDesc}

	for _, tc := range []struct {
		name      string
		drop      map[string]bool
		showErr   bool
		wantSlung bool
	}{
		{name: "a peer whose design and notes were not read holds the candidate", drop: map[string]bool{"fr-z64c": true}},
		{name: "a pool whose batched show failed holds the candidate", showErr: true},
		{name: "a peer read to completion that names no file does not hold it", wantSlung: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New()
			db.Seed(peer, candidate)
			dir := t.TempDir()
			var showErr error
			if tc.showErr {
				showErr = errors.New("bd: show timed out")
			}
			claims, err := specTargetClaimsFrom("fr-zdzs", "/town/fractals/mayor/rig/.beads",
				func(string) ([]duplicateCandidate, error) {
					return fetchDuplicatePoolFrom(droppedShow{Client: db, drop: tc.drop, err: showErr}, func(string, error) {}, dir)
				})
			if err != nil {
				t.Fatalf("claims: %v", err)
			}
			var peerClaim *specdispatch.Claimant
			for i := range claims {
				if claims[i].ID == "fr-z64c" {
					peerClaim = &claims[i]
				}
			}
			if peerClaim == nil {
				t.Fatalf("the pool lost fr-z64c: %+v", claims)
			}
			if unread := len(tc.drop) > 0 || tc.showErr; peerClaim.ClaimUnread != unread {
				t.Fatalf("fr-z64c claim unread = %v, want %v", peerClaim.ClaimUnread, unread)
			}

			f := newFakeSpecTown(frSpec("fr-zdzs", frZ64cTitle, frZ64cDesc, "2026-10-31T15:00:00Z"))
			f.claims = map[string][]specdispatch.Claimant{"fr-zdzs": claims}
			env := f.env()
			env.ShapeGate = specdispatch.ShapeGateOff

			r := runSpecDispatchCycle(env)

			if got := len(f.slung) == 1; got != tc.wantSlung {
				t.Fatalf("slung %v, want slung=%v (%+v)", f.slung, tc.wantSlung, r)
			}
			if !tc.wantSlung {
				if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "fr-z64c claim unread") {
					t.Fatalf("skipped = %+v, want a hold naming the unread peer", r.Skipped)
				}
			}
		})
	}
}

// TestSpecDispatchHoldsWhenClaimsCannotBeRead is the no-fail-open half: a
// claim set that will not load, and a tick whose claim reader was never wired,
// both hold the candidate instead of dispatching on what the tick never
// checked.
func TestSpecDispatchHoldsWhenClaimsCannotBeRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		wireErr error
		unwired bool
		reason  string
	}{
		{name: "a claim set that will not load", wireErr: errors.New("dolt is unreachable"),
			reason: "dolt is unreachable"},
		{name: "a reader the tick was never given", unwired: true,
			reason: "no claim reader is configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(frSpec("fr-z64c", frZ64cTitle, frZ64cDesc, "2026-10-31T09:00:00Z"))
			f.claimErr = tc.wireErr
			env := f.env()
			if tc.unwired {
				env.Claims = nil
			}
			env.ShapeGate = specdispatch.ShapeGateOff

			r := runSpecDispatchCycle(env)

			if len(f.slung) != 0 {
				t.Fatalf("slung %v with an unreadable claim set", f.slung)
			}
			if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, "cannot read the other beads' targets") {
				t.Fatalf("skipped = %+v, want one hold naming the failed read", r.Skipped)
			}
			if !strings.Contains(r.Skipped[0].Line, tc.reason) {
				t.Errorf("hold line %q should name %q", r.Skipped[0].Line, tc.reason)
			}
		})
	}
}
