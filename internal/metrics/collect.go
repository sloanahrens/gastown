package metrics

import (
	"context"
	"fmt"
	"time"
)

// Collect takes one snapshot over sources, giving each its own perSource
// deadline. It always returns a record: a source that returns an error, panics
// or misses its deadline leaves a null group and an errors entry, and the
// other groups still fill. A group name two sources claim keeps the first
// source's columns and is an errors entry for the second, because one key
// holds one source's answer. The sources run at once, so a hung one costs
// perSource once rather than once per source.
func Collect(ctx context.Context, env Env, sources []Source, perSource time.Duration) Record {
	rec := Record{
		V:      Version,
		TS:     env.now().UTC().Format(time.RFC3339),
		Groups: make(map[string]map[string]any, len(sources)),
	}
	done := make([]chan collectedGroup, len(sources))
	deadlines := make([]context.Context, len(sources))
	cancels := make([]context.CancelFunc, len(sources))
	for i, src := range sources {
		deadlines[i], cancels[i] = context.WithTimeout(ctx, perSource)
		done[i] = make(chan collectedGroup, 1)
		go func(i int, src Source) {
			defer func() {
				if p := recover(); p != nil {
					done[i] <- collectedGroup{err: fmt.Errorf("panic: %v", p)}
				}
			}()
			cols, err := src.Collect(deadlines[i], env)
			done[i] <- collectedGroup{cols: cols, err: err}
		}(i, src)
	}
	seen := make(map[string]bool, len(sources))
	for i, src := range sources {
		group := src.Group()
		var got collectedGroup
		select {
		case got = <-done[i]:
		default:
			// The result is read before the deadline is considered: a source
			// that answered before its deadline but whose slot came up after
			// it is not a timeout.
			select {
			case got = <-done[i]:
			case <-deadlines[i].Done():
				// The source ignored its context or lost the race with it. Its
				// result, if it ever comes, is discarded: done is buffered and
				// nothing reads it again.
				if err := ctx.Err(); err != nil {
					// The snapshot's own context ended, so the caller gave up
					// rather than the source missing its deadline.
					got.err = fmt.Errorf("canceled before the group returned: %w", err)
				} else {
					got.err = fmt.Errorf("no result within %s: %w", perSource, deadlines[i].Err())
				}
			}
		}
		cancels[i]()
		if fixedKey(group) && group != PlacementGroup {
			rec.Errors = append(rec.Errors, Error{Group: group, Error: fmt.Sprintf("group name %q is reserved", group)})
			continue
		}
		if seen[group] {
			rec.Errors = append(rec.Errors, Error{Group: group, Error: fmt.Sprintf("group name %q is claimed twice; kept the first source", group)})
			continue
		}
		seen[group] = true
		var cols map[string]any
		if got.err != nil {
			rec.Errors = append(rec.Errors, Error{Group: group, Error: got.err.Error()})
		} else {
			cols = got.cols
		}
		if group == PlacementGroup {
			rec.Placement = cols
			continue
		}
		rec.Groups[group] = cols
	}
	return rec
}

// collectedGroup is one source's outcome: its columns or the error that kept
// them out of the record.
type collectedGroup struct {
	cols map[string]any
	err  error
}
