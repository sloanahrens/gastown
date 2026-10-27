package metrics

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/daemonlog"
)

// BeadStore is the issue-store surface the metrics sources read: the board,
// the ready queue and the blocked list. It is deliberately narrower than
// beads.Client, and *beads.Beads satisfies it, so a source reaches bd through
// the seam's argv, routing and failure classification instead of exec'ing it.
type BeadStore interface {
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	ReadyFiltered(opts beads.ReadyOptions) ([]*beads.Issue, error)
	Blocked() ([]*beads.Issue, error)
}

var _ BeadStore = (*beads.Beads)(nil)

// BeadStores opens the store that reads dir's database. The real one is built
// with beads.WithCallContext, so the deadline a source puts on one read bounds
// the bd subprocess too.
type BeadStores func(ctx context.Context, dir string) BeadStore

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
