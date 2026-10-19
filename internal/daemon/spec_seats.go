package daemon

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/config"
)

// The spec dispatcher's seat settings, read on every tick (gt-jdnab).
//
// A seat is one agent with its own cap, an entry of polecat_pool.seats.
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
// seat map they set. An unreadable block is an error: the ticker parks on it
// rather than tick with a picture that lost a seat to a typo (gt-jdnab). The
// patrol document is read although the seats no longer live in it, because a
// file that does not parse, or still holds a retired key, must park the ticker
// too. Nothing here writes: LoadOrCreateTownSettings parses a town settings
// file and hands back defaults in memory for an absent one, so a tick that
// finds no settings leaves the town as it was (gt-jdnab).
func readSpecSeatPicture(townRoot string) (string, error) {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return "", fmt.Errorf("reading the polecat pool: %w", err)
	}
	if _, err := ReadPatrolConfig(townRoot); err != nil {
		return "", err
	}
	return specSeatPicture(ts.PolecatPool), nil
}

// specSeatPicture renders the seat map, one entry per agent in the loader's
// order (sorted by agent name), then the total cap: "claude-haiku=3
// claude-sonnet=2 deepseek-flash=3(closed 20:00-08:00) max_total=8". A seat
// whose cap is 0 is closed and renders as such. max_total is a pointer, so its
// 0 is the cap explicitly removed and renders.
func specSeatPicture(pool *config.PolecatPool) string {
	if pool == nil {
		return ""
	}
	agents := make([]string, 0, len(pool.Seats))
	for agent := range pool.Seats {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	var parts []string
	for _, agent := range agents {
		seat := pool.Seats[agent]
		part := fmt.Sprintf("%s=%d", agent, seat.Cap)
		if seat.Closed != "" {
			part += "(closed " + seat.Closed + ")"
		}
		parts = append(parts, part)
	}
	if pool.MaxTotal != nil {
		parts = append(parts, fmt.Sprintf("max_total=%d", *pool.MaxTotal))
	}
	return strings.Join(parts, " ")
}
