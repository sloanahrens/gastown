package daemon

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/config"
)

// The spec dispatcher's seat settings, read on every tick (gt-jdnab).
//
// A seat is one agent with its own cap: the pool seat (polecat_pool.agent,
// capped by max_seats) and the second seat (patrols.spec_dispatch
// hooked_agent, max_hooked, prefer_hooked).
// A tick shells out to `gt spec dispatch`, which builds its seat list from
// these files every run, so an edit to the mix lands at the next tick with no
// restart — a kickstart costs the tick or the landing in flight (gt-jdnab).
//
// What the daemon keeps is the picture it last read, for the two things a
// process that dies at the end of every tick cannot do: saying when the mix
// changed ("seats now ..."), and parking the ticker on a file that does not
// parse rather than ticking with a seat silently dropped.
//
// Reads are keyed by the size and mtime of the files they came from, so a tick
// with no operator edit costs one stat per file; a read that failed is retried
// on the next tick.

// specSeatTracker is the seat settings the ticker read last, the stamps of the
// files it read them from, and the picture it last announced.
type specSeatTracker struct {
	mu sync.Mutex
	// stamps is "" until the first read. Every read produces a non-empty
	// stamp string: there is always at least one settings file, present or
	// not.
	stamps  string
	picture string
	err     error
	// shown is the picture the ticker last said out loud, so a change is one
	// line and not one per tick.
	shown string
}

// read returns the seat keys the town's files set, reading them again only when
// a file moved since the last call — or when the last call failed: an unchanged
// stamp is not a read that worked, and a failure the operator never edited away
// (a lock, a permission) clears itself on the next tick rather than parking the
// ticker until a file happens to move.
func (t *specSeatTracker) read(townRoot string) (string, error) {
	stamps := specSeatStamps(specSeatFiles(townRoot))
	t.mu.Lock()
	defer t.mu.Unlock()
	if stamps == t.stamps && t.err == nil {
		return t.picture, t.err
	}
	picture, err := readSpecSeatPicture(townRoot)
	t.stamps, t.picture, t.err = stamps, picture, err
	return picture, err
}

// parked says why the ticker is not running a tick — "" after a read that
// worked. townhealth reports it, so a ticker stopped on a seat file is read as
// parked with a reason rather than as a dispatcher gone silent (gt-jdnab).
func (t *specSeatTracker) parked() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		return ""
	}
	return "the seat settings could not be read: " + t.err.Error()
}

// announce logs the seat picture when it differs from the one last announced:
// one line per change, and one on the first read of a town whose files set a
// seat key, so the log carries the mix the ticker runs with. An empty picture
// is announced only after a set one, when the seats were closed.
func (t *specSeatTracker) announce(logger *log.Logger, picture string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if picture == t.shown {
		return
	}
	t.shown = picture
	if picture == "" {
		logger.Printf("spec_dispatch: seats now none: no seat keys are set")
		return
	}
	logger.Printf("spec_dispatch: seats now %s", picture)
}

// specSeatFiles is the files the seat keys can live in: the town settings
// (polecat_pool, and the patrol document beside it once the town is on the
// two-file layout) and the retired mayor/daemon.json that carries the patrol
// document on a town that has not run gt config migrate. Both are stamped
// whatever the layout holds, because resolving a retired path to its host is
// itself a read (config.WritePath), and the point of the stamp is that a tick
// with no operator edit costs a stat. The reads themselves go through the
// loaders, which follow the layout.
func specSeatFiles(townRoot string) []string {
	return []string{config.TownSettingsPath(townRoot), config.DaemonPatrolConfigPath(townRoot)}
}

// specSeatStamps names the size and mtime of each file, "absent" for one that
// is not there: a file that appears, changes, or goes away moves its stamp.
func specSeatStamps(files []string) string {
	var b strings.Builder
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			fmt.Fprintf(&b, "%s=absent;", f)
			continue
		}
		fmt.Fprintf(&b, "%s=%d@%d;", f, fi.Size(), fi.ModTime().UnixNano())
	}
	return b.String()
}

// readSpecSeatPicture reads the two blocks the seats come from and renders the
// keys they set. An unreadable block is an error: the ticker parks on it rather
// than tick with a picture that lost a seat to a typo (gt-jdnab). Nothing here
// writes: LoadOrCreateTownSettings parses a town settings file and hands back
// defaults in memory for an absent one, so a tick that finds no settings leaves
// the town as it was (gt-jdnab).
func readSpecSeatPicture(townRoot string) (string, error) {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return "", fmt.Errorf("reading the polecat pool: %w", err)
	}
	pc, err := ReadPatrolConfig(townRoot)
	if err != nil {
		return "", err
	}
	var sd *SpecDispatchConfig
	if pc != nil && pc.Patrols != nil {
		sd = pc.Patrols.SpecDispatch
	}
	return specSeatPicture(ts.PolecatPool, sd), nil
}

// specSeatPicture renders the seat keys the files set, in the order the
// dispatcher reads them: "pool_agent=claude-sonnet max_seats=4
// hooked_agent=deepseek-flash max_hooked=2". A key written as its zero is left
// out, because a zero closes the seat it names — max_seats 0 is the pool seat
// closed and max_hooked 0 the second seat closed, the same as removing the key.
// max_total is a pointer, so its 0 is the cap explicitly removed and renders.
func specSeatPicture(pool *config.PolecatPool, sd *SpecDispatchConfig) string {
	var parts []string
	add := func(key string, set bool, value any) {
		if set {
			parts = append(parts, fmt.Sprintf("%s=%v", key, value))
		}
	}
	if pool != nil {
		add("pool_agent", pool.Agent != "", pool.Agent)
		add("max_seats", pool.MaxSeats != 0, pool.MaxSeats)
		add("max_total", pool.MaxTotal != nil, intValue(pool.MaxTotal))
	}
	if sd != nil {
		add("hooked_agent", sd.HookedAgent != "", sd.HookedAgent)
		add("max_hooked", sd.MaxHooked != 0, sd.MaxHooked)
		add("prefer_hooked", sd.PreferHooked, sd.PreferHooked)
	}
	return strings.Join(parts, " ")
}

// intValue is a *int's value, 0 for nil.
func intValue(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
