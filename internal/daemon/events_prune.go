package daemon

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

// The events_prune patrol bounds the town's raw event log (gt-ori5j). Its
// readers (the feed curator, await-signal, gt feed --follow) want new lines
// and at most minutes of history; a week covers gt feed --since for humans.
const (
	defaultEventsPruneInterval = time.Hour
	defaultEventsPruneMaxAge   = 7 * 24 * time.Hour
	defaultEventsPruneMaxBytes = 16 << 20
)

// eventsPruneSettings resolves the patrol's interval and prune bounds from
// daemon.json, falling back to the defaults for absent or invalid values.
func eventsPruneSettings(config *DaemonPatrolConfig) (time.Duration, events.PruneOptions) {
	interval := defaultEventsPruneInterval
	opts := events.PruneOptions{MaxAge: defaultEventsPruneMaxAge, MaxBytes: defaultEventsPruneMaxBytes}
	if config == nil || config.Patrols == nil || config.Patrols.EventsPrune == nil {
		return interval, opts
	}
	c := config.Patrols.EventsPrune
	if d, err := time.ParseDuration(c.IntervalStr); err == nil && d > 0 {
		interval = d
	}
	if d, err := time.ParseDuration(c.MaxAgeStr); err == nil && d > 0 {
		opts.MaxAge = d
	}
	if c.MaxBytes > 0 {
		opts.MaxBytes = c.MaxBytes
	}
	return interval, opts
}

// pruneEventsLog prunes <town>/.events.jsonl when the patrol is due. It runs
// inline on the heartbeat: a prune that drops nothing reads only the head of
// the file, and one that rewrites copies a few MB under the writers' lock.
// Due-ness comes from the persisted last-run time, so the daemon's frequent
// restarts do not postpone it (gt-ima2, gt-gxpwc).
func (d *Daemon) pruneEventsLog() {
	if !d.isPatrolActive("events_prune") {
		return
	}
	interval, opts := eventsPruneSettings(d.patrolConfig)
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "events_prune", time.Time{}, now, interval)
	if !dec.due {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("events_prune: WARNING: %s — %s", dec.warn, dec.note)
	}

	res, err := events.Prune(filepath.Join(d.config.TownRoot, events.EventsFile), opts, now)
	switch {
	case errors.Is(err, events.ErrPruneLockBusy):
		d.logger.Printf("events_prune: writers' lock busy — retrying next heartbeat")
		return
	case err != nil:
		d.logger.Printf("events_prune: error: %v", err)
	case res.Pruned():
		d.logger.Printf("events_prune: dropped %d lines, %d -> %d bytes", res.LinesDropped, res.BytesBefore, res.BytesAfter)
	}
	if err := savePatrolLastRun(d.config.TownRoot, "events_prune", now); err != nil {
		d.logger.Printf("events_prune: WARNING: cannot persist last-run time (%v)", err)
	}
}
