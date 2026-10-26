package metrics

import (
	"context"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/daemonlog"
)

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
