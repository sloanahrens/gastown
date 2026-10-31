package metrics

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/daemonlog"
)

// BeadStore is the issue-store surface the metrics sources read: the board,
// the ready queue, the blocked list, and a bead's notes by id. It is
// deliberately narrower than beads.Client, and *beads.Beads satisfies it, so a
// source reaches bd through the seam's argv, routing and failure classification
// instead of exec'ing it.
type BeadStore interface {
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	ReadyFiltered(opts beads.ReadyOptions) ([]*beads.Issue, error)
	Blocked() ([]*beads.Issue, error)
	// ShowMultiple returns the beads it found among ids, keyed by id. An id it
	// does not hold is left out rather than refused: a bead that is gone is an
	// answer. A read that routes ids to several stores answers beads.ErrNotFound
	// when one group's ids were all missing, and still returns what the other
	// groups found; showBeads reads that error as none found for that group.
	ShowMultiple(ids []string) (map[string]*beads.Issue, error)
}

var _ BeadStore = (*beads.Beads)(nil)

// BeadStores opens the store that reads dir's database. The real one is built
// with beads.WithCallContext, so the deadline a source puts on one read bounds
// the bd subprocess too.
type BeadStores func(ctx context.Context, dir string) BeadStore

// storeShowChunk is how many bead ids one store read asks for: a town with many
// reviewed beads is several reads, not one command line of every id.
const storeShowChunk = 64

// isBeadID reports whether id can be a bead id: non-empty, and not beginning
// with a dash. A store reads the ids it is given as arguments, and bd reads an
// argument beginning with a dash as a flag, so an id a log names that looks
// like one must never reach the read. Every bead id this town writes is a word
// with a prefix (gt-a1, hq-wisp-x), so nothing a real bead carries is refused.
func isBeadID(id string) bool {
	return id != "" && !strings.HasPrefix(id, "-")
}

// showBeads returns the beads the store in dir holds among ids, keyed by id.
// It reads storeShowChunk ids per call and never sends the store an id bd would
// read as a flag. A bead the store does not hold is absent, not an error, and a
// chunk that holds none of its ids is absent too. A missing seam or a failed
// read is an error.
func showBeads(ctx context.Context, env Env, dir string, ids []string) (map[string]*beads.Issue, error) {
	if env.Beads == nil {
		return nil, fmt.Errorf("no bead store seam for %s", dir)
	}
	safe := make([]string, 0, len(ids))
	for _, id := range ids {
		if isBeadID(id) {
			safe = append(safe, id)
		}
	}
	found := make(map[string]*beads.Issue, len(safe))
	if len(safe) == 0 {
		return found, nil
	}
	store := env.Beads(ctx, dir)
	if store == nil {
		return nil, fmt.Errorf("no bead store for %s", dir)
	}
	for start := 0; start < len(safe); start += storeShowChunk {
		chunk := safe[start:min(start+storeShowChunk, len(safe))]
		shown, err := store.ShowMultiple(chunk)
		if err != nil && !errors.Is(err, beads.ErrNotFound) {
			return nil, err
		}
		// ErrNotFound is an answer, not a broken read: those ids were not found.
		// A routed read can report it for one group while the other groups still
		// answered, so whatever this read returned is kept and the next chunk
		// still reads. Any other error, returned above, is a broken read and is
		// never read as beads that are gone.
		for id, issue := range shown {
			found[id] = issue
		}
	}
	return found, nil
}

// Runner runs argv and returns its stdout. A source that shells out takes its
// runner from Env, so a test answers without a process.
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// RunnerEnv runs argv with extraEnv added to the child's environment and
// returns its stdout, for a caller that must pass a value to the child rather
// than read it back afterwards.
type RunnerEnv func(ctx context.Context, argv, extraEnv []string) ([]byte, error)

// RunnerDir runs argv in dir and returns its stdout, for a command that finds
// what it needs from the working directory it starts in rather than from an
// argument.
type RunnerDir func(ctx context.Context, dir string, argv []string) ([]byte, error)

// Env is everything a source reaches outside its own process. Every read of
// the town, the clock or a file goes through these seams, so a source has no
// os or time call of its own and its test needs no host.
type Env struct {
	// Town is the town root every path a source reads is joined to.
	Town string
	// Run runs a command and returns its stdout.
	Run Runner
	// RunEnv runs a command with environment the caller names. The analysis
	// takes it for the extract, which keeps itself inside the town by the
	// environment it is given.
	RunEnv RunnerEnv
	// RunDir runs a command in the directory named. A command that resolves the
	// town from its working directory takes it, so the snapshot's own working
	// directory never decides which town is read.
	RunDir RunnerDir
	// Beads opens the issue store for a directory, for the sources that read
	// the board. A source that needs it treats nil as a wiring error and
	// reports it rather than counting an empty board.
	Beads BeadStores
	// BeadTimeout is one bead store read's deadline. Zero is the queue group's
	// default; a test sets it shorter so a deadline can run out inside a unit
	// test.
	BeadTimeout time.Duration
	// BeadAfter arms a bead read's deadline: it runs f once d has passed and
	// returns a stop function. nil is time.AfterFunc; a test supplies a clock
	// it can run by hand.
	BeadAfter func(d time.Duration, f func()) (stop func() bool)
	// LandingQueue returns the ready-to-land counts the daemon's landing
	// workers published, keyed by rig, as of now: only the rigs whose count is
	// fresh enough to answer for the queue now are in it, and a rig it leaves
	// out is read from its store. nil reads every rig from its store, which is
	// what a process with no landing worker (gt metrics snapshot) does.
	LandingQueue func(now time.Time) map[string]int
	// Now is the time the snapshot is taken; Collect writes it as the record's
	// ts. nil means the wall clock.
	Now func() time.Time
	// ReadFile reads a whole file.
	ReadFile func(string) ([]byte, error)
	// ModTime returns a file's last write time, for a source that must tell a
	// writer that stopped from an hour nothing happened in. An error means the
	// time is unknown, not that the file is old: a source that needs the time
	// must report the error rather than judge the file on a zero time.
	ModTime func(string) (time.Time, error)
	// ReadDir lists a directory, for a source whose files are named by a writer
	// rather than by the source.
	ReadDir func(string) ([]DirEntry, error)
	// Tail reads at most maxBytes from the end of path, for a log too large to
	// read whole.
	Tail func(path string, maxBytes int64) ([]byte, error)
	// Wait blocks for d, the gap a source needs between two readings of a
	// counter so their difference is a rate. A test advances a fake clock here
	// instead of sleeping, so a source that samples has no timer of its own. A
	// source that samples treats nil as an error, because a window it cannot
	// take is a rate it must not invent.
	Wait func(d time.Duration)
	// BurstHost, when set, is where a burst sample takes its host readings
	// from: the idle share, the busiest consumers and the exec tax. A daemon
	// in a container sets it, because top and ps read a host the process is
	// not on, so the readings come from the host agent's probe file. nil runs
	// the host's own commands (burstIdle, burstConsumers, burstExecTax).
	BurstHost func(now time.Time) BurstHostReading
}

// now is the time the snapshot is taken: Env.Now when the caller set one, the
// wall clock otherwise, so a zero Env still stamps its record.
func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// sharedTail answers Tail with each path and bound read once for the snapshot,
// so two groups that window the same log (the queue's tick columns and the
// sweep's minutes both read an hour of the daemon log) copy its bytes once
// rather than once each. Sources run at the same time, so a source that asks
// while a read is under way waits for it instead of starting a second; the
// bytes are returned read-only, as Tail's are.
func (e Env) sharedTail() Env {
	if e.Tail == nil {
		return e
	}
	type key struct {
		path string
		max  int64
	}
	type read struct {
		data []byte
		err  error
	}
	tail := e.Tail
	var mu sync.Mutex
	reads := make(map[key]read)
	e.Tail = func(path string, maxBytes int64) ([]byte, error) {
		k := key{path: path, max: maxBytes}
		mu.Lock()
		defer mu.Unlock()
		if r, ok := reads[k]; ok {
			return r.data, r.err
		}
		data, err := tail(path, maxBytes)
		reads[k] = read{data: data, err: err}
		return data, err
	}
	return e
}

// DirEntry is one entry of a directory listing: the name and whether it is a
// directory, which is all a source needs to choose the files it opens.
type DirEntry struct {
	Name  string
	IsDir bool
}

// sourceLogTailBytes bounds what a source reads of an append-only log through
// Env.Tail, so a series that has run for months costs one read.
const sourceLogTailBytes = 512 << 10

// daemonLogHourBytes bounds what a source reads of the daemon log to cover the
// hour its columns window. A shorter read silently shortens the window, so
// this holds an hour of the daemon's usual line length.
const daemonLogHourBytes = 2000 * 1024

// daemonLogPath is the daemon's live log under town, where the sources that
// count an hour of daemon work read it from.
func daemonLogPath(town string) string {
	return filepath.Join(town, "daemon", daemonlog.FileName)
}

// Source fills one group of the snapshot. A group is the source's identity in
// the record, so it is never renamed.
type Source interface {
	// Group is the key the source's columns are written under.
	Group() string
	// Collect returns the group's columns, or an error that leaves the group
	// null and adds an errors entry.
	Collect(ctx context.Context, env Env) (map[string]any, error)
}
