package slot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/events"
)

// WaitReason is why an Acquire waited instead of being handed a free slot.
//
// The enum is closed on purpose (gt-dc81): a 29-minute wait read out of `gt
// slot status` is only actionable when it says whether the caller queued behind
// a token holder, an unwrapped suite, or a Docker probe that could not answer —
// the three have different fixes.
type WaitReason string

const (
	// WaitReasonTokenHeld: every slot this caller may take is held by another
	// live process. This is the priority inversion the overseer's amendment on
	// gt-dc81 was written for: a refinery gate waited 29 min behind unwrapped
	// polecat suites while five MRs queued.
	WaitReasonTokenHeld WaitReason = "token_held"

	// WaitReasonUnwrappedContainers: gate containers are running with no slot
	// held for them, so no slot may be handed out (gt-tuiy).
	WaitReasonUnwrappedContainers WaitReason = "unwrapped_containers"

	// WaitReasonDaemonUnreachable: the `docker ps` probe could not confirm the
	// Docker VM is idle — the daemon is unreachable, wedged, or refuses the
	// socket — so the gate waits rather than granting unverified (gt-a8kx).
	WaitReasonDaemonUnreachable WaitReason = "daemon_unreachable"
)

// waitWatch accumulates what kept one Acquire call from granting, so the
// finished wait can be attributed after it ends (gt-dc81). A wait does not end
// with the blocker that started it — a refinery queued behind a polecat suite
// can be blocked by an inconclusive docker probe on its last poll — so each
// poll's blocked time is credited to the reason seen on that poll and the
// dominant reason wins. One instance per Acquire; not safe for concurrent use.
type waitWatch struct {
	// start is the first lock attempt, i.e. where WaitedFor is measured from.
	start time.Time
	// reasonTime is blocked time per reason; order records first sighting so a
	// tie reports the reason that blocked first.
	reasonTime map[WaitReason]time.Duration
	order      []WaitReason
	// holder is the first token holder observed blocking this wait — who the
	// caller queued behind, not whoever happens to hold the slot at grant time
	// (the blocker is usually gone by then, having just released).
	holder *Owner
	// containers is the first unwrapped-suite listing observed, and dockerErr
	// the first inconclusive-probe error, kept for the same reason.
	containers []string
	dockerErr  string
}

func newWaitWatch() *waitWatch {
	return &waitWatch{start: time.Now(), reasonTime: map[WaitReason]time.Duration{}}
}

// credit attributes one blocked poll of duration d to reason.
func (w *waitWatch) credit(reason WaitReason, d time.Duration) {
	if reason == "" {
		return
	}
	if _, seen := w.reasonTime[reason]; !seen {
		w.order = append(w.order, reason)
	}
	w.reasonTime[reason] += d
}

// waited is the wall time from the first lock attempt to now.
func (w *waitWatch) waited() time.Duration { return time.Since(w.start) }

// reason is the reason that accounts for the largest share of the wait, or ""
// when nothing ever blocked this Acquire.
func (w *waitWatch) reason() WaitReason {
	var best WaitReason
	var bestD time.Duration
	for _, r := range w.order {
		if d := w.reasonTime[r]; d > bestD {
			best, bestD = r, d
		}
	}
	return best
}

func (w *waitWatch) noteHolder(owner *Owner) {
	if w.holder == nil {
		w.holder = owner
	}
}

func (w *waitWatch) noteContainers(names []string) {
	if len(w.containers) == 0 {
		w.containers = names
	}
}

func (w *waitWatch) noteDockerErr(err error) {
	if w.dockerErr == "" && err != nil {
		w.dockerErr = err.Error()
	}
}

// waitInfo is the finished account of one wait, shared by the slot_wait event
// and the ring file entry so the two cannot disagree.
type waitInfo struct {
	Reason     WaitReason
	Waited     time.Duration
	Timeout    time.Duration
	TimedOut   bool
	Holder     *Owner
	Containers []string
	DockerErr  string
}

// info snapshots the watch against the timeout the caller asked for.
func (w *waitWatch) info(timeout time.Duration, timedOut bool) waitInfo {
	return waitInfo{
		Reason:     w.reason(),
		Waited:     w.waited(),
		Timeout:    timeout,
		TimedOut:   timedOut,
		Holder:     w.holder,
		Containers: w.containers,
		DockerErr:  w.dockerErr,
	}
}

// blockedInfo is info for a pass that is blocked right now: it carries the
// reason the pass itself cannot grant rather than the dominant one reason()
// would report. credit() runs after the sleep, so during a pass the reason has
// no time against it yet and reason() is still empty (gt-78b8).
func (w *waitWatch) blockedInfo(timeout time.Duration, reason WaitReason) waitInfo {
	info := w.info(timeout, false)
	info.Reason = reason
	return info
}

// describe renders the reason and its evidence for a human reader, e.g.
// "token held by gastown/refinery pid 62965". The wait line wraps this in its
// own parentheses, so the holder's pid is not parenthesized here.
func (i waitInfo) describe() string {
	switch i.Reason {
	case WaitReasonTokenHeld:
		if i.Holder != nil {
			return fmt.Sprintf("token held by %s pid %d", i.Holder.Role, i.Holder.PID)
		}
		return "token held by another process"
	case WaitReasonUnwrappedContainers:
		if len(i.Containers) > 0 {
			return "unwrapped container suite: " + strings.Join(i.Containers, ", ")
		}
		return "unwrapped container suite"
	case WaitReasonDaemonUnreachable:
		if i.DockerErr != "" {
			return "docker probe inconclusive: " + i.DockerErr
		}
		return "docker probe inconclusive"
	default:
		return ""
	}
}

// HistoryFile is the ring file under .runtime/locks/ holding the town's recent
// wait outcomes — every grant and every call that gave up — so an operator can
// compute wait percentiles without grepping logs (gt-dc81).
const HistoryFile = "container-gate-history.jsonl"

// HistoryLimit is how many outcomes the ring file keeps. Never below the 20
// gt-dc81 asks for: a p95 over fewer than 20 samples is noise.
const HistoryLimit = 50

// HistoryPath returns the ring file's path.
func HistoryPath(townRoot string) string {
	return filepath.Join(LockDir(townRoot), HistoryFile)
}

// historyLockPath is the flock target for ring-file reads and writes. A
// separate file, not the ring itself: entries are rewritten through a temp file
// and a rename, which would leave a lock held on the replaced inode.
func historyLockPath(townRoot string) string {
	return filepath.Join(LockDir(townRoot), "container-gate-history.lock")
}

// HistoryEntry is one wait outcome in the ring file: what waited for the slot,
// for how long, why, and how long the hold then lasted.
//
// Timed-out waits are entries too (TimedOut), with HeldS absent. Excluding them
// would bias the percentile an operator computes toward the waits that
// succeeded: a caller that gave up after an hour is the tail that matters most,
// and it is the one a grant-only ring would silently drop (gt-dc81).
//
// HeldS is a pointer so that a hold with no release on record — a killed holder
// that never reached Release — is distinguishable from a hold of zero seconds;
// whether such an entry is still in progress is what ResolveHolds answers
// against the live pool. Slot is the slot the waiter was first in line for, and
// is the slot a timed-out wait names for the part of the pool it never reached.
type HistoryEntry struct {
	TS       string     `json:"ts"`
	Role     string     `json:"role"`
	Slot     int        `json:"slot"`
	PID      int        `json:"pid"`
	WaitedS  float64    `json:"waited_s"`
	HeldS    *float64   `json:"held_s,omitempty"`
	TimeoutS float64    `json:"timeout_s,omitempty"`
	TimedOut bool       `json:"timed_out,omitempty"`
	Reason   WaitReason `json:"reason,omitempty"`
	// HolderRole/HolderPID are the token holder waited behind, for
	// WaitReasonTokenHeld; Containers the unwrapped suite, for
	// WaitReasonUnwrappedContainers; DockerError the failed probe, for
	// WaitReasonDaemonUnreachable. The overseer's gt-dc81 amendment requires the
	// reason to be legible here, not just its enum name.
	HolderRole  string   `json:"holder_role,omitempty"`
	HolderPID   int      `json:"holder_pid,omitempty"`
	Containers  []string `json:"containers,omitempty"`
	DockerError string   `json:"docker_error,omitempty"`
}

// History returns the wait outcomes recorded in the ring file, oldest first.
// A missing ring file is an empty history, not an error: a town that has never
// run a container-backed suite has nothing to report.
func History(townRoot string) ([]HistoryEntry, error) {
	unlock, err := lockHistory(townRoot)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return readHistory(townRoot)
}

// lockHistory takes the ring file's cross-process lock, creating LockDir if
// needed. Callers must invoke the returned unlock.
func lockHistory(townRoot string) (func(), error) {
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}
	fl := flock.New(historyLockPath(townRoot))
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("acquiring history file lock: %w", err)
	}
	return func() { _ = fl.Unlock() }, nil
}

// readHistory reads the ring file, skipping unparseable lines. Called with the
// history lock held.
func readHistory(townRoot string) ([]HistoryEntry, error) {
	data, err := os.ReadFile(HistoryPath(townRoot)) //nolint:gosec // path derives from the town root
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading slot history: %w", err)
	}
	var entries []HistoryEntry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e HistoryEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// writeHistory replaces the ring file, keeping the newest HistoryLimit entries.
// Called with the history lock held.
func writeHistory(townRoot string, entries []HistoryEntry) error {
	if len(entries) > HistoryLimit {
		entries = entries[len(entries)-HistoryLimit:]
	}
	var buf strings.Builder
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("marshaling slot history entry: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := atomicfile.WriteFile(HistoryPath(townRoot), []byte(buf.String()), 0644); err != nil {
		return fmt.Errorf("writing slot history: %w", err)
	}
	return nil
}

// recordWaitResult appends one wait outcome to the ring file. A grant is
// recorded at acquire time, not at release, so a holder killed mid-suite still
// leaves evidence of how long it waited (its hold is simply left open).
func recordWaitResult(townRoot, role string, slot int, pid int, info waitInfo) error {
	unlock, err := lockHistory(townRoot)
	if err != nil {
		return err
	}
	defer unlock()

	entries, err := readHistory(townRoot)
	if err != nil {
		return err
	}
	entries = append(entries, HistoryEntry{
		TS:          time.Now().UTC().Format(time.RFC3339),
		Role:        role,
		Slot:        slot,
		PID:         pid,
		WaitedS:     info.Waited.Seconds(),
		TimeoutS:    info.Timeout.Seconds(),
		TimedOut:    info.TimedOut,
		Reason:      info.Reason,
		HolderRole:  holderRole(info),
		HolderPID:   holderPID(info),
		Containers:  info.Containers,
		DockerError: info.DockerErr,
	})
	return writeHistory(townRoot, entries)
}

// completeHold closes the open entry for one holder, recording how long the
// hold lasted. reported is false when no matching open entry was found, which
// means this Release had nothing to close.
func completeHold(townRoot, role string, slot, pid int, held time.Duration) (reported bool, err error) {
	unlock, err := lockHistory(townRoot)
	if err != nil {
		return false, err
	}
	defer unlock()

	entries, err := readHistory(townRoot)
	if err != nil {
		return false, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		// The newest open entry for this slot and holder: an earlier one
		// belongs to a previous hold of the same slot.
		if e.HeldS != nil || e.Slot != slot || e.PID != pid || e.Role != role {
			continue
		}
		seconds := held.Seconds()
		entries[i].HeldS = &seconds
		return true, writeHistory(townRoot, entries)
	}
	return false, nil
}

func holderRole(info waitInfo) string {
	if info.Holder == nil {
		return ""
	}
	return info.Holder.Role
}

func holderPID(info waitInfo) int {
	if info.Holder == nil {
		return 0
	}
	return info.Holder.PID
}

// WaitSummary is the wait distribution over a history slice.
type WaitSummary struct {
	N   int
	P50 time.Duration
	P95 time.Duration
	Max time.Duration
}

// SummarizeWaits computes P50/P95/Max over the waits in entries, by nearest
// rank. It exists so the one question the slot was built to be judged on — is
// it constricting the town? — is answerable from `gt slot status` alone.
//
// Timed-out waits are included at their timeout: dropping them would report the
// distribution of the waits that succeeded, which is the flattering half of a
// gate that is refusing callers outright.
func SummarizeWaits(entries []HistoryEntry) WaitSummary {
	if len(entries) == 0 {
		return WaitSummary{}
	}
	waits := make([]time.Duration, 0, len(entries))
	for _, e := range entries {
		waits = append(waits, time.Duration(e.WaitedS*float64(time.Second)))
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	return WaitSummary{
		N:   len(waits),
		P50: nearestRank(waits, 0.50),
		P95: nearestRank(waits, 0.95),
		Max: waits[len(waits)-1],
	}
}

// nearestRank returns the p-quantile of sorted by the nearest-rank method: the
// smallest sample whose rank covers p. Exact for small samples, where an
// interpolated percentile would invent a wait nobody experienced.
func nearestRank(sorted []time.Duration, p float64) time.Duration {
	rank := int(float64(len(sorted)) * p)
	if float64(rank) < float64(len(sorted))*p {
		rank++
	}
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// emitWaitEvent emits the slot_wait event for a grant, or for a caller that
// gave up at its timeout. Telemetry never fails a grant: a caller holding a
// slot it cannot report on is still the correct outcome, and a town whose event
// log is unwritable has worse problems.
func emitWaitEvent(townRoot, role string, slot int, info waitInfo) {
	payload := map[string]interface{}{
		"role":      role,
		"slot":      slot,
		"waited_s":  info.Waited.Seconds(),
		"timeout_s": info.Timeout.Seconds(),
		"outcome":   waitOutcome(info),
		"message":   waitMessage(role, slot, info),
	}
	if rig := rigFromRole(role); rig != "" {
		// The actor is the gate itself ("gt"), so --rig filtering has to come
		// from the payload; parseGtEventLine reads it there.
		payload["rig"] = rig
	}
	if info.Reason != "" {
		payload["reason"] = string(info.Reason)
	}
	if info.Holder != nil {
		payload["held_by"] = map[string]interface{}{"role": info.Holder.Role, "pid": info.Holder.PID}
	}
	if len(info.Containers) > 0 {
		payload["containers"] = info.Containers
	}
	if info.DockerErr != "" {
		payload["docker_error"] = info.DockerErr
	}
	if err := events.LogTo(townRoot, events.TypeSlotWait, events.ActorGt, payload, events.VisibilityBoth); err != nil {
		fmt.Fprintf(probeWriter, "gt slot: recording slot_wait event: %v\n", err)
	}
}

// emitHoldEvent emits the slot_hold event for a release. exitCode is nil when
// the holder has no command status to report (an in-process holder, or one
// whose child never started).
func emitHoldEvent(townRoot, role string, slot int, held time.Duration, exitCode *int) {
	payload := map[string]interface{}{
		"role":    role,
		"slot":    slot,
		"held_s":  held.Seconds(),
		"message": holdMessage(role, slot, held, exitCode),
	}
	if rig := rigFromRole(role); rig != "" {
		payload["rig"] = rig
	}
	if exitCode != nil {
		payload["exit_status"] = *exitCode
	}
	if err := events.LogTo(townRoot, events.TypeSlotHold, events.ActorGt, payload, events.VisibilityBoth); err != nil {
		fmt.Fprintf(probeWriter, "gt slot: recording slot_hold event: %v\n", err)
	}
}

// waitOutcome names how a wait ended, for readers that key off the event stream
// rather than the message.
func waitOutcome(info waitInfo) string {
	if info.TimedOut {
		return "timeout"
	}
	return "granted"
}

// waitMessage is the human line gt feed --plain prints. It is carried in the
// payload rather than derived in internal/tui/feed so the facts and the
// wording that describes them have one owner.
func waitMessage(role string, slot int, info waitInfo) string {
	waited := info.Waited.Round(time.Second)
	if info.TimedOut {
		return fmt.Sprintf("%s gave up on container-gate slot %d after waiting %s (cap %s; %s)",
			role, slot, waited, info.Timeout.Round(time.Second), info.describe())
	}
	if info.Reason == "" {
		return fmt.Sprintf("%s acquired container-gate slot %d with no wait", role, slot)
	}
	return fmt.Sprintf("%s waited %s for container-gate slot %d (%s)", role, waited, slot, info.describe())
}

func holdMessage(role string, slot int, held time.Duration, exitCode *int) string {
	msg := fmt.Sprintf("%s released container-gate slot %d after %s", role, slot, held.Round(time.Second))
	if exitCode != nil {
		msg += fmt.Sprintf(" (exit %d)", *exitCode)
	}
	return msg
}

// rigFromRole is the rig a role string belongs to ("gastown/refinery" →
// "gastown"), or "" for a role with no rig part (the "pid-<n>" placeholder an
// unnamed `gt slot run` uses).
func rigFromRole(role string) string {
	rig, _, found := strings.Cut(role, "/")
	if !found || rig == "" {
		return ""
	}
	return rig
}
