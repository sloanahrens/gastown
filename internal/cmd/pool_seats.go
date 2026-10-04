package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
)

// The town's polecat-seat picture: which seats a spawn could take. The spec
// dispatcher's roster and the pool's own admission read the same helpers, so
// a seat one process calls taken is taken in the other. It lived in
// daemon_dispatch.go until the idle-seat dispatch check was deleted
// (gt-rwp7z.3); the picture itself outlives the patrol.

// poolSeatSessions lists the seats the pool counts as taken: the live polecat
// sessions, the seats whose session is gone while their bead is still hooked
// (gt-tldj4), the in-flight seat claims a sling writes before its session
// exists (gt-t8q5), and the seats mid-landing (gt-thy6r).
//
// All of them belong in the count. The pool's own admission path counts sessions
// and claims (choosePoolAgent), so a picture taken from live sessions alone can
// name a seat free that the very next sling refuses (gt-59o9). A crashed seat
// waiting for its supervised restart is the same lie one step earlier: its
// session is gone but the polecat is coming back within minutes, so dropping
// the seat lets the restart itself push the town past its cap (gt-tldj4). A
// seat mid-landing is the lie one stage later: the polecat that ran `gt done`
// has no session and has dropped its claim, so an accounting that read live
// sessions alone would sling a second bead into a pool class that was still
// occupied (gt-2z8k1). The seat is not free until the landing worker records
// the result.
func poolSeatSessions(t sessionLister, townRoot string, pool *config.PolecatPool) ([]poolSession, error) {
	return poolSeatSessionsWith(t, townRoot, poolDispositionFor(townRoot), poolSeatWorkFor(townRoot), pool)
}

// poolSeatSessionsWith is poolSeatSessions with the town's reads explicit: the
// per-seat polecat-state read (listPolecatSessions' disposition) and the
// batched work-bead read the landing seats are confirmed with. A caller with
// no town to read (a test) passes nil for both.
func poolSeatSessionsWith(t sessionLister, townRoot string, disposition polecatDispositionFunc, work poolSeatWorkFunc, pool *config.PolecatPool) ([]poolSession, error) {
	sessions, err := poolOccupiedSessions(t, townRoot, disposition, work, pool, time.Now)
	if err != nil {
		return nil, err
	}
	claims, err := poolSeatClaimSessions(townRoot, "")
	if err != nil {
		// A claim set that cannot be read is not an empty claim set: reading it
		// as one is what makes the picture lie (gt-t8q5).
		return nil, err
	}
	return append(sessions, claims...), nil
}

// poolOccupiedSessions lists the seats the pool already holds: the live polecat
// sessions, the seats whose session is gone while their bead is still hooked
// (gt-tldj4), and the seats mid-landing (gt-thy6r). It is the one occupancy
// source the pool's own admission decision (poolRouter.route) and the spec
// dispatcher's roster both read, so a seat one counts as taken is taken in the
// other — admission that counted live sessions alone admits past the cap while
// a seat is mid-landing (gt-3o7zk), and a crashed polecat waiting for its
// supervised restart would let a restart push the town over the cap (gt-tldj4).
//
// The seat claims are deliberately not folded in here: route decides from the
// claims it reads under the seat-decision lock (poolSeatLedger.begin), and
// poolSeatSessionsWith appends the reader's own view of them
// (poolSeatClaimSessions). Counting a claim here as well would fill two seats
// with one.
//
// A failure carries which read failed (poolOccupancyError), so a caller can
// keep its own words for a lister it cannot talk to without parsing the error.
func poolOccupiedSessions(t sessionLister, townRoot string, disposition polecatDispositionFunc, work poolSeatWorkFunc, pool *config.PolecatPool, now func() time.Time) ([]poolSession, error) {
	live, err := listPolecatSessionsWith(t, disposition, now())
	if err != nil {
		return nil, &poolOccupancyError{sessions: true, err: err}
	}
	dead, err := poolDeadHookedSessions(townRoot, pool, disposition, live)
	if err != nil {
		return nil, &poolOccupancyError{err: err}
	}
	landing, err := poolLandingSessions(townRoot, pool, work)
	if err != nil {
		return nil, &poolOccupancyError{err: err}
	}
	return append(append(live, dead...), landing...), nil
}

// poolDeadHookedSessions lists the seats whose tmux session is gone but whose
// agent bead still names hooked, non-terminal work: a polecat that crashed and
// whose supervised restart is due (gt-tldj4). Such a seat keeps holding the
// pool's seat while it waits — the supervisor brings the polecat back within
// minutes, so dropping the seat in the gap is what let a restart push the town
// past its cap (the 4/3 on 2026-10-04: sapphire's session died at 18:06 with
// gt-rwp7z.10 still hooked, the roster read 2/3 at 18:07 and slung a fourth
// polecat, and the restart at 18:08 made that four).
//
// Seats are enumerated the way `gt polecat list` and the capacity snapshot
// enumerate them — the polecat directories under each rig (listPolecatDirectoryNames)
// — less the seats a live session already accounts for. The decision is the
// shared seat-state read (polecatDispositionFunc), so a seat whose bead is
// terminal, or whose hook names nothing live, is not counted here any more than
// it is by a live session (gt-b9ud0).
//
// A seat whose state cannot be read is skipped: it is not proven held, the live
// seats are already counted, and counting every unreadable directory would let
// a bead-database hiccup fill every seat and refuse every sling.
//
// The seat is rendered on the pool's overflow agent. No per-polecat agent
// survives its session's death — the agent is the tmux session's GT_AGENT, and
// `gt polecat list` leaves the field empty for a seat with no live session — so
// the overflow seat, the one the town cap bounds, is the seat such a polecat is
// counted against. On a pool that also runs a pro seat, a dead pro occupant is
// counted on the overflow seat instead; that can only over-count it, refusing a
// sling rather than admitting past the cap.
func poolDeadHookedSessions(townRoot string, pool *config.PolecatPool, disposition polecatDispositionFunc, live []poolSession) ([]poolSession, error) {
	if townRoot == "" || pool == nil || pool.OverflowAgent == "" || disposition == nil {
		return nil, nil
	}
	liveSeats := make(map[string]bool, len(live))
	for _, s := range live {
		if s.rig != "" && s.polecat != "" {
			liveSeats[s.rig+"/"+s.polecat] = true
		}
	}
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return nil, err
	}
	var out []poolSession
	for _, rigName := range rigs {
		names, err := listPolecatDirectoryNames(filepath.Join(townRoot, rigName))
		if err != nil {
			return nil, fmt.Errorf("listing polecats in %s: %w", rigName, err)
		}
		for _, name := range names {
			if liveSeats[rigName+"/"+name] {
				continue
			}
			state, err := disposition(rigName, name)
			if err != nil || !state.HookHeld || !state.Disposition.CountsTowardCapacity {
				continue
			}
			out = append(out, poolSession{
				name:       "dead/" + rigName + "/" + name,
				agent:      pool.OverflowAgent,
				rig:        rigName,
				polecat:    name,
				deadHooked: true,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// poolOccupancyError labels a poolOccupiedSessions failure by the read that
// produced it: sessions distinguishes a session list the pool could not read
// from landing seats it could not confirm. Error and Unwrap delegate to the
// underlying error, so a caller that does not care about the source sees the
// failure it would have seen directly.
type poolOccupancyError struct {
	sessions bool
	err      error
}

func (e *poolOccupancyError) Error() string { return e.err.Error() }
func (e *poolOccupancyError) Unwrap() error { return e.err }

// poolSeatWorkFunc reads work beads by ID in one call, keyed by ID. Missing
// IDs are left out (beads.Client.ShowMultiple). It is the batched read the
// landing seats are confirmed with; nil is a caller with no store to read.
type poolSeatWorkFunc func(ids []string) (map[string]*beads.Issue, error)

// poolSeatWorkFor is the batched work-bead read a caller in townRoot wants, or
// nil when there is no town to read. The town's client routes each ID to its
// own database, so a landing seat's bead is read wherever it lives (a bead
// slung from the town carries the hq- prefix).
func poolSeatWorkFor(townRoot string) poolSeatWorkFunc {
	if townRoot == "" {
		return nil
	}
	return func(ids []string) (map[string]*beads.Issue, error) {
		return beads.New(townRoot).ShowMultiple(ids)
	}
}

// poolLandingSessions lists the seats mid-landing as occupants of the pool's
// seat: the polecats whose work bead still carries gt:ready-to-land and whose
// session is gone because `gt done` ended it.
//
// The seat's intent record is the cheap half of the signal, and the reason
// this costs no store call in the common case: it is a small JSON file per
// seat under .runtime/agents, written by `gt done` (desired=submitted) and
// cleared by the landing worker when it records the result, or by the next
// sling onto the seat. The record alone is not trusted — it can outlive a
// submission that was pulled back for rework, and the patrol scan corrects
// that a tick later (gt-xs1ni) — so the seats it names are confirmed against
// the bead in ONE batched read, and a seat whose bead no longer reads as
// submitted is a free seat.
//
// A record whose polecat directory is gone is left out, the way townhealth
// reads the same directory: the seat is gone, and its record outlived it.
//
// pool is the seat the town's polecats run; a landing seat is rendered on its
// overflow agent, the class the pool owns. A nil pool (or one with no
// overflow_agent) owns no seat, so nothing is rendered and no bead is read.
func poolLandingSessions(townRoot string, pool *config.PolecatPool, work poolSeatWorkFunc) ([]poolSession, error) {
	if townRoot == "" || pool == nil || pool.OverflowAgent == "" || work == nil {
		return nil, nil
	}

	type landing struct{ rig, name, bead string }
	var candidates []landing
	dir := filepath.Join(constants.TownRuntimePath(townRoot), "agents")
	err := filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && path == dir {
				// No seat has ever recorded an intent: nothing is mid-landing.
				return filepath.SkipDir
			}
			return err
		}
		if e.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		rig, name, ok := polecatSeatRecord(dir, path)
		if !ok {
			return nil
		}
		rec, rerr := intent.ReadPath(path)
		if rerr != nil {
			// A record that cannot be read is not a seat that is free: the
			// picture fails rather than undersell the occupied seats, as the
			// claim set does (gt-t8q5).
			return fmt.Errorf("reading intent record for %s/%s: %w", rig, name, rerr)
		}
		if !rec.Submitted() || rec.WorkBead == "" {
			// A submission with no bead names nothing to confirm: it holds no
			// seat.
			return nil
		}
		if _, serr := os.Stat(filepath.Join(townRoot, rig, "polecats", name)); serr != nil {
			return nil // the seat is gone; the record outlived it
		}
		candidates = append(candidates, landing{rig: rig, name: name, bead: rec.WorkBead})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading landing seats from %s: %w", dir, err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		if _, ok := seen[c.bead]; ok {
			continue
		}
		seen[c.bead] = struct{}{}
		ids = append(ids, c.bead)
	}
	sort.Strings(ids)
	issues, err := work(ids)
	if err != nil {
		// A bead that could not be read is not a bead that landed.
		return nil, fmt.Errorf("reading landing seats' work beads: %w", err)
	}

	out := make([]poolSession, 0, len(candidates))
	for _, c := range candidates {
		if !polecat.IsSubmittedWork(issues[c.bead]) {
			continue // the bead no longer waits to land: the seat is free
		}
		// created is left zero on purpose: a landing seat is not a spawn, and
		// the spec dispatcher's stagger (min_spawn_gap) must not arm from a
		// submission.
		out = append(out, poolSession{name: "landing/" + c.rig + "/" + c.name, agent: pool.OverflowAgent})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// polecatSeatRecord splits an intent record's path under dir
// ("<rig>/polecat.<name>.json") into the rig and polecat it names, reporting
// ok=false for a record that is not a polecat's — a witness, refinery or
// crew seat holds no pool seat.
func polecatSeatRecord(dir, path string) (rigName, polecatName string, ok bool) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "", "", false
	}
	rigName = filepath.Dir(rel)
	stem := strings.TrimSuffix(filepath.Base(rel), ".json")
	role, name, named := strings.Cut(stem, ".")
	if rigName == "." || !named || role != constants.RolePolecat || name == "" || strings.Contains(name, ".") {
		return "", "", false
	}
	return rigName, name, true
}

// knownRigNames reads the rig registry.
func knownRigNames(townRoot string) ([]string, error) {
	rigsConfig, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return nil, fmt.Errorf("loading the rig registry: %w", err)
	}
	names := make([]string, 0, len(rigsConfig.Rigs))
	for name := range rigsConfig.Rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
