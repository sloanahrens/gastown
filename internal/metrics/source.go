package metrics

import (
	"context"
	"time"
)

// Runner runs argv and returns its stdout. A source that shells out takes its
// runner from Env, so a test answers without a process.
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// Env is everything a source reaches outside its own process. Every read of
// the town, the clock or a file goes through these seams, so a source has no
// os or time call of its own and its test needs no host.
type Env struct {
	// Town is the town root every path a source reads is joined to.
	Town string
	// Run runs a command and returns its stdout.
	Run Runner
	// Now is the time the snapshot is taken; Collect writes it as the record's ts.
	Now func() time.Time
	// ReadFile reads a whole file.
	ReadFile func(string) ([]byte, error)
	// Tail reads at most maxBytes from the end of path, for a log too large to
	// read whole.
	Tail func(path string, maxBytes int64) ([]byte, error)
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
