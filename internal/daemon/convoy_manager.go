package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/convoy"
	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	defaultStrandedScanInterval = 30 * time.Second
	eventPollInterval           = 5 * time.Second
	eventPollMaxBackoff         = 60 * time.Second
	// Beads lifecycle events use CURRENT_TIMESTAMP in Dolt, which is second
	// precision. Poll with a 1s overlap so transitions that happen in the same
	// second as the previous high-water mark are still visible next cycle.
	eventPollLookback = 1 * time.Second

	// convoyGracePeriod is how long after creation a convoy is immune from
	// auto-close. This prevents a race where the daemon's stranded scan
	// fires before the sling's bd dep add is visible in Dolt. See GH#2303.
	convoyGracePeriod = 5 * time.Minute

	// requiredStoreName is the store convoy lookups read through. Convoys are
	// hq-* prefixed, so a close event from any rig resolves against the
	// town-level store; without it the event poll can only skip (gt-i36h).
	requiredStoreName = "hq"

	// requiredStoreAlertKey is the stable identity of "the daemon cannot open
	// the town store", so repeated firings record onto one escalation bead and
	// recovery closes it (gt-vwry).
	requiredStoreAlertKey = "daemon-convoy:required-store-unavailable"

	// storeOpenRetryInitial and storeOpenRetryMax bound the backoff between
	// attempts to open a store that would not open. A store missed while Dolt
	// was restarting is the case that matters — it comes back on a later
	// attempt with a fresh handle (gt-i36h).
	storeOpenRetryInitial = 5 * time.Second
	storeOpenRetryMax     = 5 * time.Minute

	// storeRecoveryEscalationAfter is how long the town store may stay
	// unopenable before the daemon escalates to the mayor. Reopening is silent
	// otherwise: every convoy check is skipped and the only trace is a log
	// line, which is how this ran unremarked until an operator read the log
	// (gt-i36h).
	storeRecoveryEscalationAfter = 10 * time.Minute

	// missingStoreLogInterval rate-limits the degraded-mode log line. The line
	// is written from the per-store poll path, so unthrottled it wrote one
	// entry every few seconds for every rig — 178 of them during gt-i36h,
	// which is itself why nobody read it.
	missingStoreLogInterval = 5 * time.Minute
)

// storeOpenResult is one attempt to open beads stores: the stores that opened,
// and the names that were wanted but would not open.
//
// The missing names are the point. A store that failed to open used to be
// dropped from the map with no record that it had ever been wanted, so nothing
// could retry it: the convoy manager saw a non-empty map, read it as complete,
// and skipped convoy lookups for every event that followed — indefinitely
// (gt-i36h).
type storeOpenResult struct {
	Stores  map[string]beadsdk.Storage
	Missing []string
}

// storeRecoveryState is the manager's state for the stores it could not open.
// The zero value means "no open attempt has confirmed the store set yet", which
// is what makes a manager holding a partial map from daemon startup retry
// instead of assuming completeness. Guarded by ConvoyManager.storesMu.
type storeRecoveryState struct {
	// confirmed is true once an open attempt returned a non-empty store set
	// with nothing missing. Until then the opener is called whenever its
	// backoff allows, which is how the set is completed.
	confirmed bool
	// missing is the names the last attempt wanted but could not open.
	missing []string
	// attempts counts consecutive attempts that left something outstanding; it
	// sets the delay before the next one.
	attempts int
	// nextAttempt is the earliest time the opener may be called again. Its zero
	// value means "now", so the first attempt is not delayed.
	nextAttempt time.Time
	// since is when the current outstanding streak began.
	since time.Time
	// escalated records that the streak has been escalated, so the bound fires
	// once per streak rather than once per attempt.
	escalated bool
	// loggedAt rate-limits the degraded-mode log line.
	loggedAt time.Time
}

// strandedConvoyInfo matches the JSON output of `gt convoy stranded --json`.
type strandedConvoyInfo struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	TrackedCount int       `json:"tracked_count"`
	ReadyCount   int       `json:"ready_count"`
	ReadyIssues  []string  `json:"ready_issues"`
	CreatedAt    time.Time `json:"created_at"`
	BaseBranch   string    `json:"base_branch,omitempty"`
	// Agent is the runtime agent requested when the convoy's beads were slung
	// (--agent). Re-feeding must use it: without it the daemon re-dispatches
	// with the rig default, silently overriding the routing decision that put
	// the bead on a specific agent (gt-yg24).
	Agent string `json:"agent,omitempty"`
	// Formula is the formula requested when the convoy's beads were slung
	// (--formula). Re-feeding must use it: without it the daemon re-dispatches
	// under gt sling's default formula, silently overriding the formula the
	// original sling asked for (gt-4lor).
	Formula string `json:"formula,omitempty"`
	// Owned reports whether the convoy carries the gt:owned label. Owned
	// convoys have a designated owner responsible for their own dispatch
	// cadence; the stranded scan must not auto-feed them (gt-qw4u).
	Owned bool `json:"owned,omitempty"`
}

// ConvoyManager monitors beads events for issue closes and periodically scans for stranded convoys.
// It handles both event-driven completion checks (via convoy.CheckConvoysForIssue) and periodic
// stranded convoy feeding/cleanup.
//
// Event polling watches ALL beads stores (town-level hq + per-rig) so that close events from
// any rig are detected. Convoys live in the hq store, so convoy lookups always use hqStore.
// Parked rigs are skipped during event polling.
type ConvoyManager struct {
	townRoot     string
	scanInterval time.Duration
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	logger       func(format string, args ...interface{})

	// holdLatch remembers the operator dispatch hold the feeder last saw, so a
	// hold is logged once per state change rather than on every scan.
	holdLatch dispatch.HoldLatch

	// stores maps store names to beads stores for event polling.
	// Key "hq" is the town-level store (used for convoy lookups).
	// Other keys are rig names (e.g., "gastown", "beads", "shippercrm").
	// Populated lazily via openStores if nil at startup (e.g., Dolt not ready).
	// Protected by storesMu.
	stores   map[string]beadsdk.Storage
	storesMu sync.Mutex

	// openStores is called to (re)open beads stores whenever the store set is
	// not known-complete: at daemon startup when Dolt wasn't ready, and on each
	// retry while a store an earlier attempt missed is still missing. It
	// reports the names it wanted but could not open, which is what the retry
	// loop works from — a store that opens on a later attempt is picked up here
	// with a fresh handle (gt-i36h). Once the set is complete it is not called
	// again. May be nil to disable opening (the stores provided upfront are all
	// there will ever be).
	openStores func() storeOpenResult

	// storeRecovery tracks stores an open attempt could not open. See
	// storeRecoveryState; guarded by storesMu.
	storeRecovery storeRecoveryState

	// escalate raises and clearEscalation closes the escalation for a town
	// store the daemon cannot reopen. Both are wired by the daemon before
	// Start() and read from the polling goroutine afterwards; nil disables
	// alerting, which is the case in tests. See SetAlertHooks.
	escalate        func(key, source, message string)
	clearEscalation func(reason string, keys ...string)

	// originBranchesCache caches each rig's origin polecat branch list (and any
	// lookup error) for the duration of one stranded scan, so a convoy with
	// many ready issues (or an unreachable remote) costs at most one ls-remote
	// per rig. Cleared at the top of every scan by resetOriginBranches.
	// Protected by originBranchesMu.
	originBranchesCache map[string]originBranchesResult
	originBranchesMu    sync.Mutex

	// scanAlertKeysClaimed tracks dead-holder alert keys already raised or
	// cleared during the current scan, so N ready issues sharing one per-rig
	// condition (the origin remote being unreadable) cost one gt escalate
	// subprocess, not N. Reset alongside originBranchesCache. Protected by
	// originBranchesMu.
	scanAlertKeysClaimed map[string]bool

	// isRigParked reports whether a rig is currently parked/docked.
	// Parked rigs are skipped during event polling. May be nil (never parked).
	isRigParked func(string) bool

	// convoyStatus reads one convoy's status, reporting false when it cannot be
	// read. Nil reads it from the town store the daemon was started with; a test
	// supplies one to drive the closed-convoy guard without a Dolt server.
	convoyStatus func(convoyID string) (string, bool)

	gtPath string

	// started guards against double-call of Start() which would spawn duplicate goroutines.
	started atomic.Bool

	// recoveryMode is set true when an event-poll failure is detected (indicating
	// Dolt is down). While set, runStrandedScan uses a shorter 5s interval so it
	// retries quickly once Dolt comes back. Cleared after the first successful scan.
	recoveryMode atomic.Bool

	// scanMu serializes calls to scan() from runStrandedScan, runStartupSweep,
	// and the Dolt recovery callback. Without this, concurrent scans can spawn
	// duplicate convoy checks for the same stranded convoy.
	scanMu sync.Mutex

	// pollGate lets a scheduled_maintenance gc pause this manager's Dolt
	// reads: the event poll tick and the stranded scan take the read side
	// through tryBeginTick and skip the tick while Pause holds the write side
	// (a live reader racing gc; see the gc design doc, Problem).
	pollGate sync.RWMutex

	// pausing is set while Pause waits for the write side, so no new tick
	// starts and the in-flight one can drain; see Pause.
	pausing atomic.Bool

	// lastEventIDs tracks per-store high-water marks for event polling.
	// Key matches stores map keys ("hq", "gastown", etc.).
	lastEventIDs sync.Map // map[string]time.Time

	// seeded is true once the first poll cycle has run (warm-up).
	// The first cycle advances high-water marks without processing events,
	// preventing a burst of historical event replay on daemon restart.
	seeded atomic.Bool

	// processedCloses tracks issue IDs whose current closed state has already
	// been processed. This prevents duplicate convoy checks when the same close
	// event is seen from multiple stores or across poll cycles where high-water
	// marks don't perfectly deduplicate (e.g., event replication). The entry is
	// cleared when the issue is reopened so a later close is processed again.
	// See GH #1798.
	processedCloses sync.Map // map[string]bool

	// processedLifecycleEvents tracks close/reopen event IDs that have already
	// been handled. This allows the 1s overlap window above without replaying
	// the same lifecycle events on every poll.
	processedLifecycleEvents sync.Map // map[string]bool
}

// NewConvoyManager creates a new convoy manager.
// scanInterval controls the periodic stranded scan; 0 uses default (30s).
// stores maps store names ("hq", rig names) to beads stores for event polling.
// nil stores disables event-driven convoy checks (stranded scan still runs),
// unless openStores is provided for lazy initialization.
// openStores completes the store set: it is called when the set is not yet
// known-complete, which covers Dolt not being ready at daemon startup and any
// store a previous attempt missed. It must be non-nil whenever stores may be
// short, or the store that is missing is never retried (gt-i36h).
// isRigParked reports whether a rig should be skipped during polling (nil = never parked).
// gtPath is the resolved path to the gt binary for subprocess calls.
func NewConvoyManager(townRoot string, logger func(format string, args ...interface{}), gtPath string, scanInterval time.Duration, stores map[string]beadsdk.Storage, openStores func() storeOpenResult, isRigParked func(string) bool) *ConvoyManager {
	if scanInterval <= 0 {
		scanInterval = defaultStrandedScanInterval
	}
	if isRigParked == nil {
		isRigParked = func(string) bool { return false }
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &ConvoyManager{
		townRoot:     townRoot,
		scanInterval: scanInterval,
		ctx:          ctx,
		cancel:       cancel,
		logger:       logger,
		stores:       copyStores(stores),
		openStores:   openStores,
		isRigParked:  isRigParked,
		gtPath:       gtPath,
	}
}

// copyStores takes a private copy of a store map, preserving nil.
//
// The manager writes to m.stores as it adopts stores a retry reopened, and it
// must never write to the caller's map: the daemon keeps its own reference to
// the map it hands over (d.beadsStores) and ranges over it from the patrol
// goroutine (hasActiveWork), so writing through to it would be a concurrent map
// iteration and write — a fatal error the daemon cannot recover from, against a
// patrol run that only wanted to know whether work was in flight (gt-i36h).
//
// The stores themselves are shared, not cloned: this is about who owns the map,
// not the handles in it. Handles already held are kept by both sides.
func copyStores(stores map[string]beadsdk.Storage) map[string]beadsdk.Storage {
	if stores == nil {
		return nil
	}
	owned := make(map[string]beadsdk.Storage, len(stores))
	for name, store := range stores {
		owned[name] = store
	}
	return owned
}

// Start begins the convoy manager goroutines (event poll + stranded scan).
// It is safe to call multiple times; subsequent calls are no-ops.
func (m *ConvoyManager) Start() error {
	if !m.started.CompareAndSwap(false, true) {
		m.logger("Convoy: Start() already called, ignoring duplicate")
		return nil
	}
	m.wg.Add(2)
	go m.runEventPoll()
	go m.runStrandedScan()
	// Run a one-shot sweep to catch convoys that completed during any previous
	// outage or while the daemon was stopped.
	go m.runStartupSweep()
	return nil
}

// Stop gracefully stops the convoy manager and closes any beads stores it owns.
func (m *ConvoyManager) Stop() {
	m.cancel()
	m.wg.Wait()

	// Close stores (whether eagerly passed or lazily opened)
	m.storesMu.Lock()
	stores := m.stores
	m.stores = nil
	m.storesMu.Unlock()
	for name, store := range stores {
		if store != nil {
			if err := store.Close(); err != nil {
				m.logger("Convoy: error closing beads store (%s): %v", name, err)
			} else {
				m.logger("Convoy: closed beads store (%s)", name)
			}
		}
	}
}

// SetAlertHooks wires the escalation sink used when the town store cannot be
// reopened. The daemon passes its escalateAlert/clearAlerts so a store the
// daemon cannot open reaches the mayor instead of only daemon.log (gt-i36h).
// Call before Start(): the hooks are read from the polling goroutines without
// a lock, like every other manager callback. Nil (the default) keeps the
// manager log-only, which is what tests want.
func (m *ConvoyManager) SetAlertHooks(escalate func(key, source, message string), clear func(reason string, keys ...string)) {
	m.escalate = escalate
	m.clearEscalation = clear
}

// retryMissingStores calls the opener when the store set is not known-complete
// and its backoff has elapsed, folding whatever opens into the live map. Stores
// already held are kept and the duplicate handle closed: a retry must not take
// a handle away from a reader — convoyStatusOf, feedHold — that is
// mid-call holding it, and re-opening healthy stores every retry would churn
// connections while Dolt is flaky.
func (m *ConvoyManager) retryMissingStores(now time.Time) {
	m.storesMu.Lock()
	opener := m.openStores
	rec := &m.storeRecovery
	due := opener != nil && !rec.confirmed && !now.Before(rec.nextAttempt)
	m.storesMu.Unlock()
	if !due {
		return
	}

	result := opener()

	// The opener holds no lock while it dials Dolt (seconds when the server is
	// slow), and the escalation below shells out to `gt escalate`, which can
	// take minutes under load. Both stay outside storesMu, which the feeder
	// takes on its own path.
	m.storesMu.Lock()
	alert := m.mergeStoreOpenResultLocked(result, now)
	m.storesMu.Unlock()

	m.deliverStoreAlert(alert)
}

// storeAlert is an escalation to raise or clear once storesMu is released.
type storeAlert struct {
	raise bool
	clear bool
	msg   string
}

// mergeStoreOpenResultLocked folds one open attempt into the live store set and
// updates recovery state. It returns the alert to deliver once the caller has
// released storesMu.
func (m *ConvoyManager) mergeStoreOpenResultLocked(result storeOpenResult, now time.Time) storeAlert {
	rec := &m.storeRecovery

	reopened := 0
	for name, store := range result.Stores {
		if store == nil {
			continue
		}
		if _, held := m.stores[name]; held {
			// Already polling this name: keep that handle and close the
			// duplicate so the connections it opened are not leaked.
			if err := store.Close(); err != nil {
				m.logger("Convoy: error closing duplicate beads store (%s): %v", name, err)
			}
			continue
		}
		if m.stores == nil {
			m.stores = make(map[string]beadsdk.Storage, len(result.Stores))
		}
		m.stores[name] = store
		reopened++
	}

	// An attempt that returns nothing at all — every open refused, or a
	// compatibility failure that discarded a whole map — is no evidence of
	// completeness, so only a non-empty set with nothing missing confirms it.
	// "Nothing missing" must come from an attempt that actually reported
	// something: an empty result (no stores opened, nothing named missing)
	// is uninformative, not proof the missing stores were resolved, and must
	// not rubber-stamp a store set this attempt never touched.
	attempted := len(result.Stores) > 0 || len(result.Missing) > 0
	complete := attempted && len(m.stores) > 0 && len(result.Missing) == 0
	if complete {
		if rec.confirmed {
			return storeAlert{}
		}
		alert := storeAlert{}
		if rec.since.IsZero() {
			m.logger("Convoy: %d beads store(s) ready", len(m.stores))
		} else {
			m.logger("Convoy: beads stores recovered after %s (%d store(s), %d reopened)",
				now.Sub(rec.since).Round(time.Second), len(m.stores), reopened)
			alert.clear = true
		}
		rec.confirmed = true
		rec.missing = nil
		rec.attempts = 0
		rec.nextAttempt = time.Time{}
		rec.since = time.Time{}
		rec.escalated = false
		rec.loggedAt = time.Time{}
		return alert
	}

	rec.confirmed = false
	// Keep the last known names when this attempt reported none: it learned
	// nothing, and the names are what the log line and the escalation say.
	if len(result.Missing) > 0 {
		rec.missing = result.Missing
	}
	if rec.since.IsZero() {
		rec.since = now
	}
	rec.attempts++
	delay := storeOpenBackoff(rec.attempts)
	rec.nextAttempt = now.Add(delay)

	what := "beads"
	if len(rec.missing) > 0 {
		what = strings.Join(rec.missing, ", ")
	}
	m.logger("Convoy: %s store unavailable, retrying in %s (attempt %d)", what, delay, rec.attempts)

	return m.pendingStoreAlertLocked(now)
}

// pendingStoreAlertLocked reports the escalation to raise for a town store that
// has stayed unopenable past storeRecoveryEscalationAfter, once per streak. The
// town store is the one that matters: without it every convoy lookup is
// skipped, while a missing rig store costs only that rig's events.
func (m *ConvoyManager) pendingStoreAlertLocked(now time.Time) storeAlert {
	rec := &m.storeRecovery
	if m.escalate == nil || rec.escalated || !m.requiredStoreMissingLocked() {
		return storeAlert{}
	}
	if now.Sub(rec.since) < storeRecoveryEscalationAfter {
		return storeAlert{}
	}
	rec.escalated = true
	// One line, and short: gt escalate truncates the title at
	// maxEscalationTitleLen, and the loss lands on whatever the sentence ends
	// with. The launchd warning an operator needs is on the bead (gt-i36h) and
	// in the daemon log this line points at.
	return storeAlert{
		raise: true,
		msg: fmt.Sprintf(
			"Convoy close detection is skipped town-wide: the %s store has not opened for %s. Convoy completion runs on the stranded scan alone until it does.",
			requiredStoreName, now.Sub(rec.since).Round(time.Second)),
	}
}

// requiredStoreMissingLocked reports whether the town store is either absent
// from the map or named as missing by the last attempt.
func (m *ConvoyManager) requiredStoreMissingLocked() bool {
	if _, held := m.stores[requiredStoreName]; !held {
		return true
	}
	for _, name := range m.storeRecovery.missing {
		if name == requiredStoreName {
			return true
		}
	}
	return false
}

// deliverStoreAlert raises or clears the town-store escalation. Called with
// storesMu released: `gt escalate` shells out, and the feeder takes that lock.
func (m *ConvoyManager) deliverStoreAlert(alert storeAlert) {
	switch {
	case alert.raise && m.escalate != nil:
		m.escalate(requiredStoreAlertKey, "daemon/convoy", alert.msg)
	case alert.clear && m.clearEscalation != nil:
		m.clearEscalation("town store reopened", requiredStoreAlertKey)
	}
}

// storeOpenBackoff returns the delay before the next open attempt: exponential
// from storeOpenRetryInitial, capped at storeOpenRetryMax. attempts is 1-based.
func storeOpenBackoff(attempts int) time.Duration {
	delay := storeOpenRetryInitial
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= storeOpenRetryMax {
			return storeOpenRetryMax
		}
	}
	return delay
}

// logMissingRequiredStore reports that convoy lookups are being skipped, at
// most once per missingStoreLogInterval. It is written from the per-store poll
// path, which runs every few seconds for every rig, so unthrottled it was both
// the only signal that this was happening and too noisy to read.
func (m *ConvoyManager) logMissingRequiredStore(polled string) {
	now := time.Now()
	m.storesMu.Lock()
	rec := &m.storeRecovery
	if !rec.loggedAt.IsZero() && now.Sub(rec.loggedAt) < missingStoreLogInterval {
		m.storesMu.Unlock()
		return
	}
	rec.loggedAt = now
	detail := ""
	if len(rec.missing) > 0 {
		detail = fmt.Sprintf(" (missing: %s)", strings.Join(rec.missing, ", "))
	}
	if retryIn := rec.nextAttempt.Sub(now); retryIn > 0 {
		detail += fmt.Sprintf(", next reopen attempt in %s", retryIn.Round(time.Second))
	} else if !rec.confirmed {
		detail += ", reopen attempt in flight"
	}
	m.storesMu.Unlock()

	m.logger("Convoy: %s store unavailable, skipping convoy lookups for %s events%s",
		requiredStoreName, polled, detail)
}

// runEventPoll polls GetAllEventsSince every 5s and processes close events.
// If stores aren't available at startup (e.g., Dolt not ready), retries
// lazily via the openStores callback until stores become available.
func (m *ConvoyManager) runEventPoll() {
	defer m.wg.Done()

	m.storesMu.Lock()
	hasStores := len(m.stores) > 0
	hasOpener := m.openStores != nil
	m.storesMu.Unlock()

	if !hasStores && !hasOpener {
		m.logger("Convoy: no beads stores and no opener, event polling disabled")
		return
	}

	currentInterval := eventPollInterval
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if !m.tryBeginTick() {
				continue // paused for a scheduled gc; the next tick polls
			}
			currentInterval = m.pollTick(currentInterval, ticker)
			m.pollGate.RUnlock()
		}
	}
}

// pollTick is one event-poll tick body, run under pollGate's read side. It
// returns the (possibly backed-off) poll interval.
func (m *ConvoyManager) pollTick(currentInterval time.Duration, ticker *time.Ticker) time.Duration {
	// Complete the store set before deciding what to poll. A store an
	// earlier attempt missed is retried here, with backoff, until it
	// opens — without this the manager polls the stores it has, skips
	// convoy lookups through the one it does not, and never asks again
	// (gt-i36h).
	m.retryMissingStores(time.Now())

	// Take a snapshot of stores for this tick to avoid holding the
	// lock across potentially slow network/Dolt calls.
	m.storesMu.Lock()
	snapshot := make(map[string]beadsdk.Storage, len(m.stores))
	for k, v := range m.stores {
		snapshot[k] = v
	}
	m.storesMu.Unlock()

	if len(snapshot) == 0 {
		// Nothing open yet. The opener is the only work there is, and
		// retryMissingStores owns its cadence.
		return currentInterval
	}

	hadError := m.pollStoresSnapshot(snapshot)
	// Exponential backoff on consecutive errors to avoid hammering
	// a recovering Dolt server. Reset on success. (GH#2686)
	if hadError {
		newInterval := currentInterval * 2
		if newInterval > eventPollMaxBackoff {
			newInterval = eventPollMaxBackoff
		}
		if newInterval != currentInterval {
			currentInterval = newInterval
			ticker.Reset(currentInterval)
			m.logger("Convoy: poll backoff → %s", currentInterval)
		}
	} else if currentInterval != eventPollInterval {
		currentInterval = eventPollInterval
		ticker.Reset(currentInterval)
		m.logger("Convoy: poll recovered, interval reset to %s", currentInterval)
	}
	return currentInterval
}

// tryBeginTick takes pollGate's read side for one poll or scan tick. It
// fails, and the tick is skipped, while Pause holds or is waiting for the
// write side. A successful call must be followed by pollGate.RUnlock.
func (m *ConvoyManager) tryBeginTick() bool {
	if m.pausing.Load() {
		return false
	}
	return m.pollGate.TryRLock()
}

// Pause stops the event poll and stranded scan from starting a new tick, and
// waits up to timeout for one already running to finish. It reports false
// (and holds nothing) when the in-flight tick outlasts timeout. A successful
// Pause must be followed by Resume. Used by scheduled_maintenance gc around
// each database's dolt_gc('--full').
//
// It polls TryLock rather than blocking in Lock: an abandoned Lock cannot be
// canceled, and behind a hung tick it would leave a pending writer that
// blocks every later tick and a goroutine per attempt. Polling alone could
// starve, since a tick that runs longer than its interval re-takes the read
// side right after releasing it; the pausing flag closes that by making
// tryBeginTick refuse new ticks while Pause waits, so only the ticks already
// in flight have to drain. Pause assumes a single caller (the gc cycle,
// serialized by maintenanceGCRunning).
func (m *ConvoyManager) Pause(timeout time.Duration) bool {
	m.pausing.Store(true)
	defer m.pausing.Store(false)
	deadline := time.Now().Add(timeout)
	for {
		if m.pollGate.TryLock() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Resume undoes a successful Pause.
func (m *ConvoyManager) Resume() {
	m.pollGate.Unlock()
}

// pollStoresSnapshot polls events from all non-parked stores in the snapshot.
// The first call is a warm-up: it advances high-water marks without
// processing events, preventing a burst of historical replay on restart.
// A per-cycle seen set deduplicates close events across stores so each
// issueID is processed at most once per poll cycle.
// Returns true if any store poll encountered an error.
func (m *ConvoyManager) pollStoresSnapshot(stores map[string]beadsdk.Storage) bool {
	seen := make(map[string]bool)
	hadError := false
	for name, store := range stores {
		if name != "hq" && m.isRigParked(name) {
			continue
		}
		if err := m.pollStore(name, store, stores, seen); err != nil {
			hadError = true
		}
	}
	m.seeded.CompareAndSwap(false, true)
	return hadError
}

// pollStore fetches new events from a single store and processes close events.
// Convoy lookups always use the hq store since convoys are hq-* prefixed.
// The stores snapshot is passed to avoid accessing m.stores without the lock.
// The seen set deduplicates issueIDs across stores within a poll cycle.
// Returns an error if the poll failed (used by caller for backoff decisions).
func (m *ConvoyManager) pollStore(name string, store beadsdk.Storage, stores map[string]beadsdk.Storage, seen map[string]bool) error {
	// Load per-store high-water mark.
	// Default to Unix epoch (not zero time) because Go's zero time.Time
	// (0001-01-01) causes Dolt's SQL driver to produce +Inf when converting
	// to a float parameter, triggering "Error 1366: +Inf is not a valid
	// value for double". Unix epoch is safe for all SQL backends.
	highWater := time.Unix(0, 0).UTC()
	if v, ok := m.lastEventIDs.Load(name); ok {
		highWater = v.(time.Time)
	}
	querySince := highWater
	if !highWater.Equal(time.Unix(0, 0).UTC()) {
		querySince = highWater.Add(-eventPollLookback)
		if querySince.Before(time.Unix(0, 0).UTC()) {
			querySince = time.Unix(0, 0).UTC()
		}
	}

	events, err := store.GetAllEventsSince(m.ctx, querySince)
	if err != nil {
		if isInfNaNError(err) {
			// A corrupted row in the events table has +Inf/-Inf/NaN stored in a
			// double column (e.g. created_at serialized from Go's zero time.Time).
			// Advance the high-water mark to now so future polls skip past the
			// bad row entirely. Events before now are missed, but the stranded
			// convoy scanner will catch any completions that were lost.
			now := time.Now().UTC()
			m.lastEventIDs.Store(name, now)
			m.logger("Convoy: event poll (%s): +Inf/NaN row detected, advancing HWM to %s to skip corrupt data", name, now.Format(time.RFC3339))
			return nil
		}
		m.logger("Convoy: event poll error (%s): %v", name, err)
		// Signal recovery mode so the stranded scan shortens its interval and
		// retries quickly once Dolt comes back.
		m.recoveryMode.Store(true)
		return err
	}

	// Advance high-water mark from all events
	for _, e := range events {
		if e.CreatedAt.After(highWater) {
			highWater = e.CreatedAt
		}
	}
	m.lastEventIDs.Store(name, highWater)

	// First poll cycle is warm-up only: advance marks, skip processing.
	// This prevents replaying the entire event history on daemon restart.
	if !m.seeded.Load() {
		for _, e := range events {
			if e.ID == "" {
				continue
			}
			if isCloseEvent(e) || isReopenEvent(e) {
				m.processedLifecycleEvents.Store(e.ID, true)
			}
		}
		return nil
	}

	// Convoy lookups read through the hq store (convoys are hq-* prefixed).
	// Missing it, every close event in this store is uncheckable: skip, but say
	// so at a rate an operator can read, and leave getting it back to the retry
	// path (retryMissingStores) rather than logging this forever (gt-i36h).
	hqStore := stores[requiredStoreName]
	if hqStore == nil {
		m.logMissingRequiredStore(name)
		return nil
	}

	for _, e := range events {
		issueID := e.IssueID
		if issueID == "" {
			continue
		}

		if isCloseEvent(e) || isReopenEvent(e) {
			if _, alreadyHandled := m.processedLifecycleEvents.LoadOrStore(e.ID, true); alreadyHandled {
				continue
			}
		}

		if isReopenEvent(e) {
			// Reopening starts a new close epoch for this issue. Clear both the
			// per-cycle and cross-cycle dedup so a later close is processed again.
			delete(seen, issueID)
			m.processedCloses.Delete(issueID)
			continue
		}

		if !isCloseEvent(e) {
			continue
		}

		// Deduplicate: skip if already processed this issueID in this poll cycle
		// (same close may appear in multiple stores or as multiple event types).
		// Reopen events clear this marker so close→reopen→close can be processed
		// twice even when all three events land in the same poll cycle.
		if seen[issueID] {
			continue
		}
		seen[issueID] = true

		// Cross-cycle dedup: skip if this issue's close was already processed
		// in a previous poll cycle. The same close event can appear from
		// multiple stores (replication) or across poll cycles when high-water
		// marks don't perfectly filter. See GH #1798.
		if _, alreadyProcessed := m.processedCloses.LoadOrStore(issueID, true); alreadyProcessed {
			continue
		}

		m.logger("Convoy: close detected: %s (from %s)", issueID, name)
		resolver := convoy.NewStoreResolver(m.townRoot, stores)
		convoy.CheckConvoysForIssue(m.ctx, hqStore, m.townRoot, issueID, "Convoy", m.logger, m.gtPath, m.isRigParked, resolver)
		convoy.FireCrossRigDepNotifications(m.ctx, issueID, m.townRoot, stores, m.logger)
	}
	return nil
}

// isInfNaNError reports whether err is a Dolt/SQL error about an invalid float
// value (+Inf, -Inf, NaN) in a double column. These errors arise when a
// corrupted row (e.g. created_at written from Go's zero time.Time via an old
// driver path) is encountered during a query. The caller should advance the
// high-water mark to skip past the offending row rather than entering
// permanent backoff.
func isInfNaNError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Dolt wraps values in single quotes: "'+Inf' is not a valid value for 'double'"
	// Match both quoted and unquoted forms.
	return strings.Contains(msg, "+Inf is not a valid value") ||
		strings.Contains(msg, "'+Inf' is not a valid value") ||
		strings.Contains(msg, "-Inf is not a valid value") ||
		strings.Contains(msg, "'-Inf' is not a valid value") ||
		strings.Contains(msg, "NaN is not a valid value") ||
		strings.Contains(msg, "'NaN' is not a valid value")
}

func isCloseEvent(e *beadsdk.Event) bool {
	if e == nil {
		return false
	}
	if e.EventType == beadsdk.EventClosed {
		return true
	}
	return e.EventType == beadsdk.EventStatusChanged &&
		e.NewValue != nil &&
		*e.NewValue == "closed"
}

func isReopenEvent(e *beadsdk.Event) bool {
	if e == nil {
		return false
	}
	if e.EventType == beadsdk.EventReopened {
		return true
	}
	return e.EventType == beadsdk.EventStatusChanged &&
		e.OldValue != nil &&
		*e.OldValue == "closed" &&
		(e.NewValue == nil || *e.NewValue != "closed")
}

// runStrandedScan is the periodic stranded convoy scan loop.
// During recovery mode (after Dolt poll errors) the interval shrinks to 5s
// so a successful scan fires promptly once Dolt comes back. Recovery mode is
// cleared after the first successful scan.
func (m *ConvoyManager) runStrandedScan() {
	defer m.wg.Done()

	ticker := time.NewTicker(m.scanInterval)
	defer ticker.Stop()

	// Run once immediately, then on interval
	m.scan()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			// While in recovery mode, shorten the next tick so we retry quickly
			// after a Dolt outage without waiting the full scan interval.
			if m.recoveryMode.Load() {
				ticker.Reset(5 * time.Second)
			} else {
				ticker.Reset(m.scanInterval)
			}
			m.scan()
		}
	}
}

// scan runs one stranded scan cycle: find stranded convoys, feed or close each.
// Serialized by scanMu to prevent concurrent scans from spawning duplicate checks.
func (m *ConvoyManager) scan() {
	if !m.tryBeginTick() {
		m.logger("Convoy: stranded scan skipped: paused for scheduled gc")
		return
	}
	defer m.pollGate.RUnlock()

	m.scanMu.Lock()
	defer m.scanMu.Unlock()

	// Fresh remote state per scan; see originBranches.
	m.resetOriginBranches()

	stranded, err := m.findStranded()
	if err != nil {
		m.logger("Convoy: stranded scan failed: %s", util.FirstLine(err.Error()))
		return
	}
	// Successful scan: clear recovery mode so the ticker returns to normal interval.
	m.recoveryMode.Store(false)

	for _, c := range stranded {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		if c.ReadyCount > 0 {
			if c.Owned {
				// Owned convoys have a designated owner managing their own
				// dispatch cadence (e.g. the deacon's rejection-aware
				// redispatch). The system-managed stranded scan must not
				// race that owner (gt-qw4u).
				m.logger("Convoy %s: owned, skipping auto-feed (owner manages dispatch)", c.ID)
				continue
			}
			m.feedFirstReady(c)
		} else if c.TrackedCount == 0 {
			// Empty convoy — but skip if it was just created (GH#2303).
			// The sling's bd dep add may not be visible in Dolt yet.
			if !c.CreatedAt.IsZero() && time.Since(c.CreatedAt) < convoyGracePeriod {
				m.logger("Convoy %s: empty but within grace period (created %s ago) — skipping", c.ID, time.Since(c.CreatedAt).Round(time.Second))
				continue
			}
			m.closeEmptyConvoy(c.ID)
		} else {
			// Tracked issues exist but none are ready. This could mean:
			// (a) all tracked issues are closed → convoy should auto-close
			// (b) issues are blocked/in-progress → needs agent review
			// Run convoy check to handle case (a); it's a no-op for (b).
			m.logger("Convoy %s: %d tracked issues, 0 ready — checking completion", c.ID, c.TrackedCount)
			m.checkConvoyCompletion(c.ID)
		}
	}
}

// findStranded runs `gt convoy stranded --json` and parses the output.
func (m *ConvoyManager) findStranded() ([]strandedConvoyInfo, error) {
	cmd := exec.CommandContext(m.ctx, m.gtPath, "convoy", "stranded", "--json")
	cmd.Dir = m.townRoot
	cmd.Env = bdReadOnlyRoutingEnv(m.townRoot)
	util.SetProcessGroup(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s", util.FirstLine(stderr.String()))
	}

	var stranded []strandedConvoyInfo
	if err := json.Unmarshal(stdout.Bytes(), &stranded); err != nil {
		// Include first line of raw output for debugging (e.g., non-JSON warnings on stdout)
		raw := util.FirstLine(stdout.String())
		return nil, fmt.Errorf("parsing stranded JSON: %w (raw: %q)", err, raw)
	}

	return stranded, nil
}

// feedFirstReady iterates through all ready issues in a stranded convoy and
// dispatches the first one that can be successfully slung. Issues are skipped
// (with logging) when the prefix is unresolvable, the rig has no route, the
// rig is parked, or the sling command fails. This ensures convoys progress
// even when some issues target unavailable rigs.
func (m *ConvoyManager) feedFirstReady(c strandedConvoyInfo) {
	if len(c.ReadyIssues) == 0 {
		return
	}

	// The operator's town-wide hold parks every automatic dispatcher
	// (gt-ifijm). The convoy stays stranded and ready, so the first scan after
	// the hold lifts feeds it.
	reason := dispatch.OperatorHold(m.townRoot)
	if m.holdLatch.Changed(reason) {
		if reason != "" {
			m.logger("Convoy feed: not feeding stranded convoys (first held: %s): %s", c.ID, reason)
		} else {
			m.logger("Convoy feed: operator dispatch hold lifted; feeding resumes")
		}
	}
	if reason != "" {
		return
	}

	for _, issueID := range c.ReadyIssues {
		prefix := beads.ExtractPrefix(issueID)
		if prefix == "" {
			m.logger("Convoy %s: no prefix for %s, skipping", c.ID, issueID)
			continue
		}

		rig := beads.GetRigNameForPrefix(m.townRoot, prefix)
		if rig == "" {
			m.logger("Convoy %s: no rig for %s (prefix %s), skipping", c.ID, issueID, prefix)
			continue
		}

		if m.isRigParked(rig) {
			m.logger("Convoy %s: rig %s is parked, skipping %s", c.ID, rig, issueID)
			continue
		}

		// A per-rig ESTOP holds this rig's issues; the town hold was
		// answered above (gt-ifijm).
		if reason := dispatch.RigHold(m.townRoot, rig); reason != "" {
			m.logger("Convoy %s: not feeding %s: %s", c.ID, issueID, reason)
			continue
		}

		// One read of the bead's record answers both hold checks in this loop
		// (gt-ghyfx): the rejection gate here and the hold check below.
		hold, haveStore := m.feedHold(rig, issueID)
		switch {
		case !haveStore:
			// No open store for the rig is a town-level gap the store alert
			// already reports; the scan proceeds as for a clean record, but
			// says so rather than folding it in silently (gt-udrrw, gt-jj29p).
			m.logger("Convoy %s: could not confirm rejection-marker state for %s, proceeding as clear: no open store for rig %s", c.ID, issueID, rig)
		case hold.MergeRejection:
			// This bead was previously rejected and reopened for recovery
			// (RECOVERED_BEAD). Redispatch of rejected work belongs solely
			// to the deacon, which applies cooldown/escalation gating and
			// resumes on the surviving branch. The stranded scan must defer
			// to it rather than race it with a fresh sling (gt-qw4u).
			m.logger("Convoy %s: %s carries a rejection marker, deferring to deacon, skipping", c.ID, issueID)
			continue
		case hold.Unreadable:
			// A record the store holds but cannot hand back could carry a
			// rejection, so the bead is held here (fail-closed). The gate
			// used to proceed as clear and leave the skip to the hold check
			// below, which failed closed on the same read; the bead was
			// never fed, but the dead-holder and surviving-branch checks ran
			// on a record nobody could read (gt-ghyfx).
			m.logger("Convoy %s: %s not dispatched: %s — cannot rule out a merge rejection (fail-closed)", c.ID, issueID, hold.Reason)
			continue
		}

		if assignee := m.issueAssignee(rig, issueID); assignee != "" {
			// A known previous holder distinguishes "dead session, might have
			// unpreserved work" from "never held, nothing to lose" — only the
			// former needs the fail-closed origin+worktree check below
			// (gt-utt4). Issues with no assignee fall through to the
			// fail-open surviving-branch check in the else branch below.
			if feed, escalateMsg := m.resolveDeadHolderWork(rig, assignee, issueID); !feed {
				if escalateMsg != "" {
					m.logger("Convoy %s: %s", c.ID, escalateMsg)
				}
				continue
			}
		} else if branch, ok := m.survivingBranchFor(rig, issueID); ok {
			// The previous holder is gone, but its branch is still on origin:
			// the work is preserved — either mid-flight (killed by a town
			// halt or park, which never runs `gt done`) or already submitted
			// to the merge queue. Feeding it anyway spawns a second polecat
			// from main on the same bead, which is the spawn storm this scan
			// caused on gt-ibt8 (4 polecats) and gt-da2x (3). Skipping keeps
			// the convoy able to progress on its other ready issues while a
			// human or the deacon decides; the deacon's redispatch passes
			// --force explicitly when a live holder really is wanted.
			m.logger("Convoy %s: %s has surviving branch %s on origin — work preserved, skipping feed (resume with: gt sling %s %s --branch %s)",
				c.ID, issueID, branch, issueID, rig, branch)
			continue
		}

		// A hold other than a rejection is acted on after the dead-holder/
		// worktree checks above, which keep their escalation for a dead
		// holder's unpreserved work: that warning is worth more than the
		// earlier, quieter skip (gt-tq6l). The record was read once, at the
		// rejection gate.
		if hold.Reason != "" {
			m.logger("Convoy %s: %s not dispatched: %s", c.ID, issueID, hold.Reason)
			continue
		}

		// A convoy the operator closed between the stranded scan and here must
		// not be fed. The scan's list is a snapshot from the top of the cycle
		// (findStranded) and the checks above it are not cheap, so a close
		// lands inside the window: closing hq-cv-smk2e --force still left the
		// daemon re-slinging its bead seconds later (gt-4lbz). The check sits
		// as close to the sling as it can, since that window is what it closes.
		if !m.convoyOpen(c.ID) {
			m.logger("Convoy %s: closed since the stranded scan — %s not fed", c.ID, issueID)
			return
		}

		// Re-dispatch with the agent the bead was slung with, never the rig
		// default: the rig default is what silently re-routed mayor-ruled
		// beads after a failed sling (gt-yg24).
		agent, agentDesc := convoy.FeedDispatchAgent(c.Agent, m.townRoot, rig)

		m.logger("Convoy %s: feeding %s to %s (%s)", c.ID, issueID, rig, agentDesc)

		slingArgs := []string{"sling", issueID, rig, "--no-boot", "--actor=daemon/convoy:" + c.ID}
		if c.BaseBranch != "" {
			slingArgs = append(slingArgs, "--base-branch="+c.BaseBranch)
		}
		if agent != "" {
			slingArgs = append(slingArgs, "--agent="+agent)
		}
		// Re-dispatch with the formula the bead was slung with, never gt
		// sling's own default: a formula bond that fails and rolls back
		// leaves the convoy open, and re-feeding it without --formula ran
		// the bead under mol-polecat-work instead of the formula the
		// original sling asked for (gt-4lor).
		if formula := strings.TrimSpace(c.Formula); formula != "" {
			slingArgs = append(slingArgs, "--formula="+formula)
			m.logger("Convoy %s: feeding %s with formula %q recorded on convoy at sling time", c.ID, issueID, formula)
		}
		cmd := exec.CommandContext(m.ctx, m.gtPath, slingArgs...)
		cmd.Dir = m.townRoot
		cmd.Env = bdMutationRoutingEnv(m.townRoot)
		util.SetProcessGroup(cmd)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		runErr := cmd.Run()
		// Timing lines ride on stderr in both outcomes (gt-llg8): a failed sling
		// is the one most worth attributing.
		for _, l := range slingTimingLines(stderr.String()) {
			m.logger("Convoy %s: sling %s: %s", c.ID, issueID, l)
		}
		if runErr != nil {
			// A refusal is not a failure (gt-xidg, A3): the rig's merge queue
			// is over its configured ceiling, so the sling declined to add
			// more work to it. The bead keeps the readiness the convoy scan
			// gave it and nothing here touches its status, so the next tick
			// offers it again — once the queue drains, the same bead feeds.
			// Logging it as a deferral keeps "the town is at capacity"
			// distinguishable from "the sling broke" in daemon.log.
			// A surviving-work refusal (the dead holder's work is on a
			// branch, or cannot be verified; gt-vm5g4) is deferred the same
			// way: it waits for an operator, it is not a failure.
			if reason, ok := slingDeferralReason(stderr.String()); ok {
				m.logger("Convoy %s: deferring %s: %s", c.ID, issueID, reason)
				continue
			}
			m.logger("Convoy %s: sling %s failed: %s", c.ID, issueID, slingErrorLine(stderr.String()))
			continue
		}
		return // Successfully dispatched one issue
	}

	m.logger("Convoy %s: no dispatchable issues (all %d skipped)", c.ID, len(c.ReadyIssues))
}

// convoyOpen reports whether the convoy is still open, so the feeder can skip
// one the operator closed after the stranded scan read it.
func (m *ConvoyManager) convoyOpen(convoyID string) bool {
	status, ok := m.convoyStatusOf(convoyID)
	if !ok {
		// Fail open: the stranded scan already read this convoy as open, and a
		// status the town store cannot answer must not stop the feeder. Logged
		// so this is distinguishable in daemon.log from a confirmed-open convoy
		// (gt-rif8).
		m.logger("Convoy %s: status unreadable, failing open (assuming still open)", convoyID)
		return true
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "closed", "tombstone":
		return false
	}
	return true
}

// convoyStatusOf reads one convoy's status from the town-level store the
// convoys live in, reporting false when it cannot be read. m.convoyStatus
// stands in for that read in tests.
func (m *ConvoyManager) convoyStatusOf(convoyID string) (string, bool) {
	if m.convoyStatus != nil {
		return m.convoyStatus(convoyID)
	}
	m.storesMu.Lock()
	store := m.stores["hq"]
	m.storesMu.Unlock()
	if store == nil {
		return "", false
	}
	issue, err := store.GetIssue(m.ctx, convoyID)
	if err != nil || issue == nil {
		return "", false
	}
	return string(issue.Status), true
}

// listOriginBranchesFn is a seam for tests. Production uses
// polecat.ListOriginPolecatBranches.
var listOriginBranchesFn = polecat.ListOriginPolecatBranches

// survivingBranchFor reports whether issueID already has a polecat branch on
// the rig's origin remote, and returns it. A surviving branch means the work
// is preserved even though the holder's session is gone, so the stranded scan
// must not feed a fresh polecat for it (gt-ibt8, gt-da2x).
//
// Every failure mode — no repo for the rig, unreachable remote, git error —
// reports false (fail open): an unreadable remote must not stall the stranded
// scan's normal feeding, which is what keeps convoys moving.
func (m *ConvoyManager) survivingBranchFor(rig, issueID string) (string, bool) {
	branches := m.originBranches(rig)
	if len(branches) == 0 {
		return "", false
	}
	matches := polecat.MatchSurvivingBranches(branches, issueID)
	if len(matches) == 0 {
		return "", false
	}
	return matches[0], true
}

// originBranchesResult is one rig's origin polecat-branch listing, or the
// error that prevented it. Kept together in the cache so a caller that must
// fail closed on an unreadable remote (resolveDeadHolderWork) and one that
// fails open on it (survivingBranchFor, for issues with no known holder) can
// share the same single lookup per rig per scan.
type originBranchesResult struct {
	branches []string
	err      error
}

// originBranches returns the polecat branches on the rig's origin remote,
// caching the result for the duration of one stranded scan (see resetOriginBranches).
// A lookup error is swallowed here (returns nil): this is the fail-open path
// used for issues with no known previous holder, where there is nothing a
// stale remote could cause to be discarded. Callers that need to know whether
// the lookup actually succeeded use originBranchesWithErr.
//
// The cache matters: an ls-remote against an unreachable remote blocks for the
// full query timeout, and scan holds scanMu for the whole cycle, so an
// uncached lookup per ready issue would stall every other convoy's feed.
// One ls-remote per rig per scan bounds that to a single timeout.
func (m *ConvoyManager) originBranches(rig string) []string {
	return m.originBranchesWithErr(rig).branches
}

// originBranchesWithErr is originBranches without swallowing the lookup
// error, for callers that must fail closed rather than treat "unreadable" as
// "empty" (gt-utt4).
func (m *ConvoyManager) originBranchesWithErr(rig string) originBranchesResult {
	m.originBranchesMu.Lock()
	defer m.originBranchesMu.Unlock()

	if m.originBranchesCache == nil {
		m.originBranchesCache = make(map[string]originBranchesResult)
	}
	if res, ok := m.originBranchesCache[rig]; ok {
		return res
	}

	branches, err := listOriginBranchesFn(filepath.Join(m.townRoot, rig))
	res := originBranchesResult{branches: branches, err: err}
	m.originBranchesCache[rig] = res
	return res
}

// resetOriginBranches drops the per-scan branch cache so the next scan sees
// fresh remote state.
func (m *ConvoyManager) resetOriginBranches() {
	m.originBranchesMu.Lock()
	m.originBranchesCache = nil
	m.scanAlertKeysClaimed = nil
	m.originBranchesMu.Unlock()
}

// feedHold reads the hold rule for issueID from its rig's store (gt-tq6l,
// gt-ghyfx). The rule, merge-rejection marker included, lives in convoy, which
// the event-driven continuation feed shares; this only picks the rig's store.
// ok is false when the rig has no open store.
//
// A rig with no open store reports no hold, the fail-open posture the sibling
// issueAssignee check keeps for the same gap: a store that never opened is a
// town-level condition, already escalated by the store alert, and blocking
// dispatch on it would stall every convoy feeding that rig. A store that is
// open but cannot read the record is different: that is an unreadable hold,
// and the bead is not fed.
func (m *ConvoyManager) feedHold(rig, issueID string) (hold convoy.Hold, ok bool) {
	m.storesMu.Lock()
	store := m.stores[rig]
	m.storesMu.Unlock()
	if store == nil {
		return convoy.Hold{}, false
	}
	return convoy.FeedHold(m.ctx, store, issueID, nil), true
}

// issueAssignee returns issueID's assignee via the already-open per-rig
// store, or "" if the store is unavailable, the issue can't be read, or it
// has no assignee. An empty return is read by callers as "no known previous
// holder" — the same fail-open posture as feedHold's missing store, for the
// same reason: a rig whose store never opened must not block dispatch of issues
// unrelated to that gap.
func (m *ConvoyManager) issueAssignee(rig, issueID string) string {
	m.storesMu.Lock()
	store := m.stores[rig]
	m.storesMu.Unlock()
	if store == nil {
		return ""
	}

	issue, err := store.GetIssue(m.ctx, issueID)
	if err != nil || issue == nil {
		return ""
	}
	return issue.Assignee
}

// deadHolderWorktreeState is what resolveDeadHolderWork finds when it looks
// at a dead holder's own worktree, past whatever survivingBranchFor already
// found on origin. WorktreePath is "" when there is nothing to check —
// either the assignee resolves to no worktree, or the worktree's current
// branch does not encode issueID (the seat was reused, or reallocated to a
// different bead since).
type deadHolderWorktreeState struct {
	WorktreePath string
	Branch       string
	Status       *git.UncommittedWorkStatus
}

// deadHolderWorktreeStateFn resolves a dead holder's worktree state. A var so
// tests can substitute a fake without real git repos; production uses
// defaultDeadHolderWorktreeState.
var deadHolderWorktreeStateFn = defaultDeadHolderWorktreeState

// defaultDeadHolderWorktreeState inspects the assignee's worktree directly,
// using the same path resolution the deacon's stale-hook scan uses. Returns a
// zero-value state (not an error) when there is simply nothing to check;
// returns an error only when a worktree that should hold issueID's work could
// not be read, which the caller must treat as "state undetermined" rather
// than "clean" (gt-utt4).
func defaultDeadHolderWorktreeState(townRoot, assignee, issueID string) (deadHolderWorktreeState, error) {
	path := deacon.AssigneeWorktreePath(townRoot, assignee)
	if path == "" {
		return deadHolderWorktreeState{}, nil
	}

	g := git.NewGit(path)
	branch, err := g.CurrentBranch()
	if err != nil {
		return deadHolderWorktreeState{}, fmt.Errorf("reading current branch: %w", err)
	}
	meta, ok := polecat.ParseGeneratedBranchName(branch)
	if !ok || meta.Issue != issueID {
		// Not this issue's branch: the worktree was reused (or predates the
		// generated-name convention). Nothing here is attributable to issueID.
		return deadHolderWorktreeState{}, nil
	}

	// CheckUncommittedWorkLocalFailClosed, not CheckUncommittedWorkLocal: a
	// clone that never fetched origin has no comparison ref for this branch,
	// and the plain local check reads that as "0 unpushed" — the exact
	// silent-discard this function exists to prevent (gt-utt4).
	status, err := g.CheckUncommittedWorkLocalFailClosed()
	if err != nil {
		return deadHolderWorktreeState{}, fmt.Errorf("checking worktree state: %w", err)
	}
	return deadHolderWorktreeState{WorktreePath: path, Branch: branch, Status: status}, nil
}

// deadHolderAlertKey scopes an escalation to one issue's own dead-holder
// worktree state (uncommitted edits, an unreadable worktree, a failed
// preserve push).
func deadHolderAlertKey(rig, issueID string) string {
	return "daemon-convoy:dead-holder:" + rig + ":" + issueID
}

// deadHolderOriginAlertKey scopes an escalation to one rig's origin remote
// being unreadable during dead-holder recovery. This is a per-rig condition,
// not a per-issue one, so every ready issue on the same unreachable rig
// shares one alert (and, via claimAlertOnceThisScan, one gt escalate call per
// scan) instead of raising its own.
func deadHolderOriginAlertKey(rig string) string {
	return "daemon-convoy:dead-holder-origin:" + rig
}

// resolveDeadHolderWork decides whether it is safe to feed a fresh polecat
// for issueID, whose readiness came from a dead assignee session (gt-utt4).
// feed=true means neither the origin remote nor the holder's own worktree
// found anything at risk. Every other outcome skips feeding this cycle and
// raises or clears the relevant alert itself, so the alert tracks the
// condition live rather than sticking once raised: a surviving branch or a
// successfully preserved push skip quietly (escalateMsg==""); an unreadable
// remote, an unreadable worktree, or uncommitted edits with no safe
// auto-preserve skip and escalate.
func (m *ConvoyManager) resolveDeadHolderWork(rig, assignee, issueID string) (feed bool, escalateMsg string) {
	originKey := deadHolderOriginAlertKey(rig)
	issueKey := deadHolderAlertKey(rig, issueID)

	origin := m.originBranchesWithErr(rig)
	if origin.err != nil {
		msg := fmt.Sprintf("%s: origin branch state could not be determined (%s) — %s's session is dead and its work might be unpreserved, not re-slinging blind",
			issueID, util.FirstLine(origin.err.Error()), assignee)
		m.raiseDeadHolderAlert(originKey, msg)
		return false, msg
	}
	m.clearDeadHolderAlert(originKey, "origin branch state now readable")

	if matches := polecat.MatchSurvivingBranches(origin.branches, issueID); len(matches) > 0 {
		m.logger("Convoy: %s has surviving branch %s on origin (dead holder %s) — work preserved, skipping feed (resume with: gt sling %s %s --branch %s)",
			issueID, matches[0], assignee, issueID, rig, matches[0])
		m.clearDeadHolderAlert(issueKey, "surviving branch found on origin")
		return false, ""
	}

	state, err := deadHolderWorktreeStateFn(m.townRoot, assignee, issueID)
	if err != nil {
		msg := fmt.Sprintf("%s: %s's worktree state could not be determined (%s), not re-slinging blind",
			issueID, assignee, util.FirstLine(err.Error()))
		m.raiseDeadHolderAlert(issueKey, msg)
		return false, msg
	}
	if state.WorktreePath == "" {
		m.clearDeadHolderAlert(issueKey, "no worktree attributable to this issue")
		return true, ""
	}

	// Real uncommitted work — non-runtime modified/untracked/unmerged paths,
	// or a stash — has no safe auto-preserve; there is nothing to push for
	// it. Gas Town's own runtime dirt (.beads/, .claude/, CLAUDE.local.md,
	// ...) is excluded via NonRuntimePaths the same way gt done and the
	// deacon's stale-hook scan already exclude it, so leftover tool state
	// left by the dead session does not block recovery forever.
	dirt := state.Status.NonRuntimePaths()
	if len(dirt) > 0 || state.Status.StashCount > 0 {
		msg := fmt.Sprintf("%s: %s's worktree has uncommitted work (%d file(s), stash=%d) that cannot be auto-preserved by pushing — resolve by hand, then reopen",
			issueID, assignee, len(dirt), state.Status.StashCount)
		m.raiseDeadHolderAlert(issueKey, msg)
		return false, msg
	}
	if state.Status.UnpushedCommits == 0 {
		m.clearDeadHolderAlert(issueKey, "worktree clean, nothing to preserve")
		return true, ""
	}

	// Clean of real dirt with commits that never reached origin: safe to
	// publish, since this only pushes what the holder already committed.
	if err := preserveWorktreeBranch(git.NewGit(state.WorktreePath), state.Branch); err != nil {
		msg := fmt.Sprintf("%s: pushing %s's unpushed work on %s failed (%s), not re-slinging blind — resolve by hand, then reopen",
			issueID, assignee, state.Branch, util.FirstLine(err.Error()))
		m.raiseDeadHolderAlert(issueKey, msg)
		return false, msg
	}
	m.logger("Convoy: %s's dead holder %s had unpushed commits on %s, pushed to preserve — skipping feed", issueID, assignee, state.Branch)
	m.clearDeadHolderAlert(issueKey, "unpushed work pushed to preserve")
	return false, ""
}

// raiseDeadHolderAlert escalates key at most once per stranded scan
// (claimAlertOnceThisScan): repeated ready issues sharing a per-rig condition
// must not each pay for their own synchronous gt escalate subprocess while
// scanMu is held.
func (m *ConvoyManager) raiseDeadHolderAlert(key, msg string) {
	if m.escalate == nil || !m.claimAlertOnceThisScan(key) {
		return
	}
	m.escalate(key, "daemon/convoy", msg)
}

// clearDeadHolderAlert is raiseDeadHolderAlert's counterpart: closes key at
// most once per scan once the condition it guarded has resolved.
func (m *ConvoyManager) clearDeadHolderAlert(key, reason string) {
	if m.clearEscalation == nil || !m.claimAlertOnceThisScan(key) {
		return
	}
	m.clearEscalation(reason, key)
}

// claimAlertOnceThisScan reports whether key has not yet been raised or
// cleared during the current scan, claiming it if so. Reset alongside
// originBranchesCache at the top of every scan (resetOriginBranches).
func (m *ConvoyManager) claimAlertOnceThisScan(key string) bool {
	m.originBranchesMu.Lock()
	defer m.originBranchesMu.Unlock()
	if m.scanAlertKeysClaimed == nil {
		m.scanAlertKeysClaimed = make(map[string]bool)
	}
	if m.scanAlertKeysClaimed[key] {
		return false
	}
	m.scanAlertKeysClaimed[key] = true
	return true
}

// preserveWorktreeBranch pushes branch's tip to origin so it survives even
// though the local worktree that made it is about to be replaced. Mirrors
// preserveBranchBeforeNuke's fallback (internal/cmd/polecat.go): on a
// rejected push (remote branch moved on), publish under a
// <branch>-<sha7> side ref instead — that name cannot conflict, so a
// rejection there means the remote itself is unusable.
func preserveWorktreeBranch(g *git.Git, branch string) error {
	tip, err := g.Rev("refs/heads/" + branch)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", branch, err)
	}
	tip = strings.TrimSpace(tip)
	if tip == "" {
		return fmt.Errorf("resolve %s: empty sha", branch)
	}

	pushTo := branch
	if err := g.Push("origin", branch+":"+pushTo, false); err != nil {
		pushTo = branch + "-" + deadHolderRefSuffixSHA(tip)
		if fallbackErr := g.Push("origin", branch+":"+pushTo, false); fallbackErr != nil {
			return fmt.Errorf("push %s:%s failed (%v) and fallback push %s:%s failed (%v)",
				branch, branch, err, branch, pushTo, fallbackErr)
		}
	}
	return g.VerifyPushedCommit("origin", pushTo, tip)
}

// deadHolderRefSuffixSHA is the short-sha suffix for a fallback preserve ref
// (<branch>-<sha7>): enough to identify the commit, short enough to read.
func deadHolderRefSuffixSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// checkConvoyCompletion runs gt convoy check to auto-close a convoy whose
// tracked issues may all be closed. This handles the case where the event poll
// missed the close events (e.g., daemon restart, Dolt latency).
func (m *ConvoyManager) checkConvoyCompletion(convoyID string) {
	cmd := exec.CommandContext(m.ctx, m.gtPath, "convoy", "check", convoyID)
	cmd.Dir = m.townRoot
	cmd.Env = bdMutationRoutingEnv(m.townRoot)
	util.SetProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		m.logger("Convoy %s: completion check failed: %s", convoyID, util.FirstLine(stderr.String()))
	}
}

// closeEmptyConvoy runs gt convoy check to auto-close an empty convoy.
func (m *ConvoyManager) closeEmptyConvoy(convoyID string) {
	m.logger("Convoy %s: auto-closing (empty)", convoyID)

	cmd := exec.CommandContext(m.ctx, m.gtPath, "convoy", "check", convoyID)
	cmd.Dir = m.townRoot
	cmd.Env = bdMutationRoutingEnv(m.townRoot)
	util.SetProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		m.logger("Convoy %s: check failed: %s", convoyID, util.FirstLine(stderr.String()))
	}
}

// runStartupSweep runs one convoy check pass after a brief delay to catch
// convoys that completed while the daemon was stopped or Dolt was unavailable.
// It waits 10 seconds so Dolt has time to stabilize before the first query.
// This goroutine is not tracked in wg because it is short-lived (exits after
// a single scan) and does not need to participate in the Stop() shutdown.
func (m *ConvoyManager) runStartupSweep() {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-m.ctx.Done():
		return
	case <-timer.C:
	}
	m.logger("Convoy: running startup sweep for stranded convoys")
	m.scan()
}
