package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// A rig's operational state is assembled from two layers, and only the
// expensive one is memoized.
//
// The wisp layer is local and ephemeral - what `gt rig park` writes - and reads
// as a stat plus a small file, so it is evaluated on every call: parking a rig
// takes effect on the next evaluation rather than on the next expiry.
//
// The identity bead is global and synced - what `gt rig dock` writes - and
// reading it costs a bd subprocess. That read is 0.4s on an idle host but
// spends its entire 60s budget (internal/beads.bdSubprocessTimeout) when the
// host is CPU-starved, and one heartbeat evaluates it per rig from roughly ten
// call sites: each patrol's rig filter (getPatrolRigs), witness auto-start,
// refinery auto-start, and the convoy manager's isRigParked callback. A
// saturated host therefore stalled the heartbeat for a minute per rig per call
// site - during exactly the window the heartbeat exists to cover, since some of
// those call sites are the ones that restart dead agents (gt-4nu3). So the
// bead read is memoized, for this long.
//
// The window is deliberately shorter than the default heartbeat interval (3m):
// each tick's first evaluation reads the bead itself, so a dock lands within one
// tick - the cadence the daemon decides auto-start on anyway - while the call
// sites within a tick share one read. (`gt rig dock` stops the rig's witness and
// refinery itself and drops the rig from daemon.json's patrols, so this read is
// the backstop, not the mechanism.)
const rigOperationalCacheTTL = 60 * time.Second

// A bead read that fails is memoized for this long instead.
//
// It fails closed either way - the rig is reported not operational and nothing
// is auto-started for it - so retrying sooner cannot start anything earlier:
// the next auto-start decision is the next heartbeat tick. What a retry does
// buy is another full 60s subprocess budget on a host that is already starved,
// which is the cost this bead is about. Holding the failure for a tick bounds
// the worst case at one read per rig per tick instead of one per call site. The
// escalation raised alongside it is what keeps the suppressed rig visible in the
// meantime.
const rigOperationalFailureCacheTTL = 3 * time.Minute

// rigBeadVerdict is what a rig's identity bead says about the rig's own state.
type rigBeadVerdict int

const (
	// rigBeadActive is a bead carrying no status label: nothing global says the
	// rig is stopped.
	rigBeadActive rigBeadVerdict = iota
	rigBeadParked
	rigBeadDocked
)

// rigStatusAlertKey is the escalation fingerprint for one rig's unverifiable
// status. One key per rig is what keeps a failing host from filing a fresh
// alert on every heartbeat.
func rigStatusAlertKey(rigName string) string {
	return "rig-status:" + rigName
}

// rigStatusFailureCategory names why a rig's identity bead could not be read.
//
// The causes call for different responses - a timeout is a starved host, where
// the cost is the problem and the retry is bounded by the memo above; a missing
// bead is data, where every other surface that reads that bead will fail the
// same way - so the log line has to tell them apart instead of emitting the same
// "(assuming not operational)" for both (gt-4nu3).
func rigStatusFailureCategory(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "lookup timed out"
	case errors.Is(err, beads.ErrNotFound):
		return "rig bead missing"
	default:
		return "lookup failed"
	}
}

// rigBeadEntry is one memoized identity-bead read: the verdict it produced, or
// the category of the failure that kept it from producing one.
type rigBeadEntry struct {
	verdict   rigBeadVerdict
	failed    string // failure category when the read could not answer
	expiresAt time.Time
	// alertOpen records that this rig's failure has an escalation standing, so
	// a later read that answers clears it rather than leaving the operator with
	// a stale alert for a rig that recovered.
	alertOpen bool
}

// rigBeadStoreResult reports what storing an entry did to the rig's open
// escalation, so the caller raises or clears it outside the lock (both are
// subprocess work).
type rigBeadStoreResult struct {
	open  bool // a failure with no alert standing: raise one
	clear bool // an answering read for a rig whose failure had one
}

// rigOperationalCache memoizes identity-bead reads, and owns the escalation
// bookkeeping that goes with them. Concurrent by design: see the
// Daemon.rigOperational field for the goroutines that share it.
type rigOperationalCache struct {
	mu      sync.Mutex
	entries map[string]rigBeadEntry
	// wispConfigWarned tracks rigs already logged for a missing wisp config, so
	// that notice stays once per rig per process instead of once per evaluation
	// (gt-k07). It rides this cache's lock because the same concurrent callers
	// write it - the field it replaced was documented "heartbeat goroutine only"
	// while rigPool workers were already calling the function that wrote it.
	wispConfigWarned map[string]bool
	// escalations holds one serial queue per rig; see enqueueEscalation.
	escalations map[string]chan func()
}

// get returns the memoized read for rigName when it is still fresh.
func (c *rigOperationalCache) get(rigName string, now time.Time) (rigBeadEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[rigName]
	if !ok || !now.Before(entry.expiresAt) {
		return rigBeadEntry{}, false
	}
	return entry, true
}

// store memoizes a bead read for rigName and reports the escalation the caller
// owes. failed is the failure category for a read that could not answer, and
// empty for one that did.
func (c *rigOperationalCache) store(rigName string, verdict rigBeadVerdict, failed string, ttl time.Duration, now time.Time) rigBeadStoreResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]rigBeadEntry)
	}

	// One open alert per rig: a failure that follows a failure keeps the alert
	// it already has, and a read that answers closes it.
	var result rigBeadStoreResult
	alertWasOpen := false
	if previous, ok := c.entries[rigName]; ok {
		alertWasOpen = previous.alertOpen
	}
	switch {
	case alertWasOpen && failed == "":
		result.clear = true
	case !alertWasOpen && failed != "":
		result.open = true
	}

	c.entries[rigName] = rigBeadEntry{
		verdict:   verdict,
		failed:    failed,
		expiresAt: now.Add(ttl),
		alertOpen: alertWasOpen || result.open,
	}
	return result
}

// markWispConfigWarned reports whether this is the first time rigName's missing
// wisp config has been seen in this process.
func (c *rigOperationalCache) markWispConfigWarned(rigName string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wispConfigWarned == nil {
		c.wispConfigWarned = make(map[string]bool)
	}
	if c.wispConfigWarned[rigName] {
		return false
	}
	c.wispConfigWarned[rigName] = true
	return true
}

// escalationQueueDepth bounds how many escalations one rig may have pending.
// Reaching it means the rig is flapping faster than escalations drain, which is
// alert noise rather than a new condition.
const escalationQueueDepth = 8

// enqueueEscalation runs work on rigName's serial queue.
//
// A rig's raise and clear must not overtake each other. A clear that lands after
// a later raise closes the alert for a rig that is still failing, and a raise
// that lands after a clear leaves a healthy rig with an open alert; both calls
// are subprocess work (a raise retries gt escalate up to maxEscalationRetries
// times) that runs off the caller's goroutine, so nothing else orders them.
// Work is queued in the order the cache asked for it, which is the order the
// conditions occurred in.
func (c *rigOperationalCache) enqueueEscalation(rigName string, logf func(string, ...interface{}), work func()) {
	c.mu.Lock()
	if c.escalations == nil {
		c.escalations = make(map[string]chan func())
	}
	queue, ok := c.escalations[rigName]
	if !ok {
		queue = make(chan func(), escalationQueueDepth)
		c.escalations[rigName] = queue
		go func() {
			for work := range queue {
				work()
			}
		}()
	}
	c.mu.Unlock()

	select {
	case queue <- work:
	default:
		// Never silent: the failure itself still has its log line, and the next
		// failure episode raises again once this queue drains.
		logf("rig-status: escalation queue for %s is full (%d pending), dropping an escalation", rigName, escalationQueueDepth)
	}
}

// alertRigStatusUnverified escalates a rig whose docked/parked status cannot be
// read. This is the condition that keeps every agent for that rig from being
// auto-started, so it must not be visible only as a warning line in a log
// nobody reads while the rig sits dead (gt-4nu3).
func (d *Daemon) alertRigStatusUnverified(rigName, category string, err error) {
	alert := d.rigStatusAlert
	if alert == nil {
		return // zero-value Daemon (unit tests): no town to escalate into
	}
	message := fmt.Sprintf(
		"Rig %s: cannot verify docked/parked status (%s), so no agents are auto-started for it until a lookup succeeds.\n%v",
		rigName, category, err)
	// Escalate off the heartbeat goroutine: escalateAlert retries gt escalate
	// (each attempt a bd write of its own, up to three of them) against the very
	// host that is starving, and the heartbeat is what this memo exists to keep
	// moving.
	key := rigStatusAlertKey(rigName)
	d.rigOperational.enqueueEscalation(rigName, d.logger.Printf, func() {
		alert(key, "rig-status", message)
	})
}

// clearRigStatusUnverified closes the escalation for a rig whose status is
// readable again, so the alert does not outlive the condition it describes.
//
// The clear is driven by the cache entry that was open, not by "this read
// succeeded": clearing on every healthy read would add a `gt escalate clear`
// subprocess per rig per tick, which is the class of cost this file exists to
// remove. The cost of that choice is an alert raised before a daemon restart:
// the new process has no open entry to close, so that one stays until an
// operator sees the rig working again.
func (d *Daemon) clearRigStatusUnverified(rigName string) {
	clear := d.rigStatusClear
	if clear == nil {
		return
	}
	reason := fmt.Sprintf("rig %s docked/parked status readable again", rigName)
	key := rigStatusAlertKey(rigName)
	d.rigOperational.enqueueEscalation(rigName, d.logger.Printf, func() {
		clear(reason, key)
	})
}
