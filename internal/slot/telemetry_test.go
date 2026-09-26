package slot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

// slotEventsOfType reads the town's raw event log and returns the events of
// one type. The slot's telemetry is written with the explicit town root
// (events.LogTo), so a test town sees it exactly as a real one would.
func slotEventsOfType(t *testing.T, townRoot, eventType string) []events.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(townRoot, events.EventsFile))
	if err != nil {
		return nil
	}
	var out []events.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev events.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unparseable event line in %s: %v", events.EventsFile, err)
		}
		if ev.Type == eventType {
			out = append(out, ev)
		}
	}
	return out
}

func payloadSeconds(t *testing.T, ev events.Event, key string) float64 {
	t.Helper()
	v, ok := ev.Payload[key].(float64)
	if !ok {
		t.Fatalf("event %s payload %q = %#v, want a number", ev.Type, key, ev.Payload[key])
	}
	return v
}

// TestAcquire_MeasuresAndRecordsTheWait is gt-dc81's acceptance criterion 1 and
// 3 in unit form: a second caller queuing behind a holder reports a wait, the
// holder reports ~0, and both land in the ring file with the reason the second
// one waited.
func TestAcquire_MeasuresAndRecordsTheWait(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	holder, err := Acquire(townRoot, "gastown/refinery", 10*time.Second)
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}
	if holder.WaitedFor > time.Second {
		t.Errorf("uncontended Acquire reported a wait of %s, want ~0", holder.WaitedFor)
	}

	type acquired struct {
		h   *Handle
		err error
	}
	done := make(chan acquired, 1)
	go func() {
		h, err := Acquire(townRoot, "gastown/pearl", 15*time.Second)
		done <- acquired{h, err}
	}()

	// Let the waiter take at least one blocked pass, then let it in.
	time.Sleep(300 * time.Millisecond)
	if err := holder.Release(); err != nil {
		t.Fatalf("holder Release: %v", err)
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("waiter Acquire: %v", got.err)
	}
	defer func() { _ = got.h.Release() }()
	if got.h.WaitedFor < 200*time.Millisecond {
		t.Errorf("waiter reported a wait of %s, want at least the 300ms it queued behind the holder", got.h.WaitedFor)
	}

	history, err := History(townRoot)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("History holds %d entries, want one per acquisition: %+v", len(history), history)
	}
	first, second := history[0], history[1]
	if first.Role != "gastown/refinery" || second.Role != "gastown/pearl" {
		t.Fatalf("history roles = %q then %q, want the holder then the waiter", first.Role, second.Role)
	}
	if first.Reason != "" {
		t.Errorf("the uncontended holder recorded reason %q, want none", first.Reason)
	}
	if second.Reason != WaitReasonTokenHeld {
		t.Errorf("waiter recorded reason %q, want %q", second.Reason, WaitReasonTokenHeld)
	}
	if second.HolderRole != "gastown/refinery" {
		t.Errorf("waiter recorded holder_role %q, want the role it queued behind", second.HolderRole)
	}
	if first.HeldS == nil {
		t.Errorf("the holder's released hold has no held_s: %+v", first)
	}
	if second.HeldS != nil {
		t.Errorf("the still-open waiter hold has held_s set: %+v", second)
	}

	// The slot_wait event carries the same account for `gt feed --plain`.
	waits := slotEventsOfType(t, townRoot, events.TypeSlotWait)
	if len(waits) != 2 {
		t.Fatalf("slot_wait events = %d, want one per grant", len(waits))
	}
	waiterEvent := waits[1]
	if got, want := waiterEvent.Payload["role"], "gastown/pearl"; got != want {
		t.Errorf("slot_wait role = %v, want %v", got, want)
	}
	if got, want := waiterEvent.Payload["reason"], string(WaitReasonTokenHeld); got != want {
		t.Errorf("slot_wait reason = %v, want %v", got, want)
	}
	if seconds := payloadSeconds(t, waiterEvent, "waited_s"); seconds < 0.2 {
		t.Errorf("slot_wait waited_s = %v, want the ~0.3s wait or more", seconds)
	}
	if seconds := payloadSeconds(t, waiterEvent, "timeout_s"); seconds != 15 {
		t.Errorf("slot_wait timeout_s = %v, want the caller's 15s", seconds)
	}
	heldBy, ok := waiterEvent.Payload["held_by"].(map[string]interface{})
	if !ok {
		t.Fatalf("slot_wait held_by = %#v, want the owner it queued behind", waiterEvent.Payload["held_by"])
	}
	if heldBy["role"] != "gastown/refinery" {
		t.Errorf("slot_wait held_by.role = %v, want gastown/refinery", heldBy["role"])
	}
	if msg, _ := waiterEvent.Payload["message"].(string); !strings.Contains(msg, "waited") {
		t.Errorf("slot_wait message = %q, want it to name the wait", msg)
	}
	if _, present := waits[0].Payload["reason"]; present {
		t.Errorf("the uncontended grant recorded a wait reason: %+v", waits[0].Payload)
	}
}

// TestRelease_RecordsTheHold covers the slot_hold half: held_s, the exit status
// gt slot run reports, and the guard against recording one hold twice.
func TestRelease_RecordsTheHold(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	h, err := Acquire(townRoot, "gastown/refinery", 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := h.ReleaseWithExit(1); err != nil {
		t.Fatalf("ReleaseWithExit: %v", err)
	}

	// A deferred Release after gt slot run's failure path calls
	// ReleaseWithExit, and both must not double-count the hold.
	if err := h.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	holds := slotEventsOfType(t, townRoot, events.TypeSlotHold)
	if len(holds) != 1 {
		t.Fatalf("slot_hold events = %d, want exactly one per hold", len(holds))
	}
	if got := holds[0].Payload["role"]; got != "gastown/refinery" {
		t.Errorf("slot_hold role = %v, want gastown/refinery", got)
	}
	if got := holds[0].Payload["exit_status"]; got != float64(1) {
		t.Errorf("slot_hold exit_status = %v, want 1", got)
	}
	if seconds := payloadSeconds(t, holds[0], "held_s"); seconds < 0.1 {
		t.Errorf("slot_hold held_s = %v, want the 150ms hold", seconds)
	}

	history, err := History(townRoot)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("History holds %d entries after one acquisition, want 1: %+v", len(history), history)
	}
	if history[0].HeldS == nil || *history[0].HeldS < 0.1 {
		t.Errorf("history held_s = %v, want the ~0.15s hold", history[0].HeldS)
	}
}

// TestAcquire_WaitReasonUnwrappedContainers is the overseer's gt-dc81 amendment
// for the second enum case: a wait behind an unwrapped suite names it.
func TestAcquire_WaitReasonUnwrappedContainers(t *testing.T) {
	townRoot := t.TempDir()

	line := dockerPSLine("stray-id", "dolt/dolt-sql-server:2.2.0", "stray-suite",
		time.Now().Add(-2*time.Minute), nil)
	var calls int32
	restore := SetContainerListerForTest(func() ([]string, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return []string{line}, nil
		}
		return nil, nil
	})
	defer restore()

	// The wait reports itself through probeWriter while it is still waiting
	// (gt-78b8), not only in the record of how it ended.
	prevProbe := probeWriter
	var probeOut strings.Builder
	probeWriter = &probeOut
	defer func() { probeWriter = prevProbe }()

	h, err := Acquire(townRoot, "gastown/refinery", 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire while an unwrapped suite cleared on the second check: %v", err)
	}
	defer func() { _ = h.Release() }()
	if h.WaitedFor < DefaultPollInterval {
		t.Errorf("wait = %s, want at least one poll interval spent behind the unwrapped suite", h.WaitedFor)
	}
	if !strings.Contains(probeOut.String(), "unwrapped container suite") {
		t.Errorf("probe output = %q, want the unwrapped suite named while the caller waited", probeOut.String())
	}

	history, err := History(townRoot)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	entry := history[len(history)-1]
	if entry.Reason != WaitReasonUnwrappedContainers {
		t.Errorf("reason = %q, want %q", entry.Reason, WaitReasonUnwrappedContainers)
	}
	if len(entry.Containers) != 1 || entry.Containers[0] != parseGateContainer(line).Display() {
		t.Errorf("containers = %v, want the container that blocked the wait", entry.Containers)
	}
}

// TestAcquire_WaitReasonDaemonUnreachable is the third enum case: a docker
// check that could not answer is recorded as such rather than as a mystery.
func TestAcquire_WaitReasonDaemonUnreachable(t *testing.T) {
	townRoot := t.TempDir()

	probeErr := errors.New("docker ps did not respond within 5s: context deadline exceeded")
	var calls int32
	restore := SetContainerListerForTest(func() ([]string, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, probeErr
		}
		return nil, nil
	})
	defer restore()

	// The inconclusive probe announces itself through probeWriter; a test
	// binary's stderr is not the place to read that back from.
	prevProbe := probeWriter
	var probeOut strings.Builder
	probeWriter = &probeOut
	defer func() { probeWriter = prevProbe }()

	h, err := Acquire(townRoot, "gastown/refinery", 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire after a wedged docker probe cleared: %v", err)
	}
	defer func() { _ = h.Release() }()

	if !strings.Contains(probeOut.String(), "inconclusive") {
		t.Errorf("probe output = %q, want the wait's cause reported while it waited", probeOut.String())
	}
	// The inconclusive probe is this reason's one reporter: the wait line
	// would say the same thing a second time (gt-78b8).
	if strings.Contains(probeOut.String(), "waiting for container-gate slot") {
		t.Errorf("probe output = %q, want the inconclusive probe reported once", probeOut.String())
	}

	history, err := History(townRoot)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	entry := history[len(history)-1]
	if entry.Reason != WaitReasonDaemonUnreachable {
		t.Errorf("reason = %q, want %q", entry.Reason, WaitReasonDaemonUnreachable)
	}
	if !strings.Contains(entry.DockerError, "did not respond within 5s") {
		t.Errorf("docker_error = %q, want the probe failure that caused the wait", entry.DockerError)
	}
}

// TestHistory_RingFileIsBounded covers the ring's bound and the "survives
// process death" half of gt-dc81's acceptance: the record is on disk as soon as
// the slot is granted, so a holder that dies mid-suite is still accounted for.
func TestHistory_RingFileIsBounded(t *testing.T) {
	townRoot := t.TempDir()

	// Ten more acquisitions than the ring holds, so eviction is observable.
	const over = 10
	for i := 0; i < HistoryLimit+over; i++ {
		err := recordWaitResult(townRoot, fmt.Sprintf("role-%d", i), 0, os.Getpid(), waitInfo{
			Waited:  time.Duration(i) * time.Second,
			Timeout: time.Minute,
		})
		if err != nil {
			t.Fatalf("recordWaitResult %d: %v", i, err)
		}
	}

	history, err := History(townRoot)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != HistoryLimit {
		t.Fatalf("ring file holds %d entries, want the %d-entry bound", len(history), HistoryLimit)
	}
	if len(history) < 20 {
		t.Errorf("ring file holds %d entries, less than the 20 `gt slot status --json` promises", len(history))
	}
	if want := fmt.Sprintf("role-%d", over); history[0].Role != want {
		t.Errorf("oldest kept entry = %q, want %q (the oldest %d evicted)", history[0].Role, want, over)
	}
	if history[len(history)-1].HeldS != nil {
		t.Errorf("an entry written at acquire time already has held_s set: %+v", history[len(history)-1])
	}

	// The ring is never rewritten in place without the lock, so a fresh read
	// from a second process sees every entry a first one wrote.
	data, err := os.ReadFile(HistoryPath(townRoot))
	if err != nil {
		t.Fatalf("reading ring file: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; lines != HistoryLimit {
		t.Errorf("ring file has %d lines on disk, want %d", lines, HistoryLimit)
	}
}

// TestCompleteHold_ClosesTheNewestOpenEntry keeps a re-run of the same slot
// from closing the wrong record: entry N+1 belongs to the new hold, not the one
// the previous holder already closed.
func TestCompleteHold_ClosesTheNewestOpenEntry(t *testing.T) {
	townRoot := t.TempDir()

	for _, waited := range []time.Duration{time.Second, 3 * time.Second} {
		if err := recordWaitResult(townRoot, "gastown/refinery", 0, os.Getpid(), waitInfo{Waited: waited}); err != nil {
			t.Fatalf("recordWaitResult: %v", err)
		}
	}
	if _, err := completeHold(townRoot, "gastown/refinery", 0, os.Getpid(), 7*time.Second); err != nil {
		t.Fatalf("completeHold: %v", err)
	}

	history, err := History(townRoot)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("History holds %d entries, want 2", len(history))
	}
	if history[0].HeldS != nil {
		t.Errorf("the first hold was closed instead of the newest: %+v", history[0])
	}
	if history[1].HeldS == nil || *history[1].HeldS != 7 {
		t.Errorf("newest hold held_s = %v, want 7", history[1].HeldS)
	}

	// A hold nobody closed (killed holder) is matched by nothing, and must not
	// be silently attributed to a later one.
	if reported, err := completeHold(townRoot, "other/role", 0, os.Getpid(), time.Second); err != nil || reported {
		t.Errorf("completeHold for a role with no open entry = (%v, %v), want (false, nil)", reported, err)
	}
}

func TestSummarizeWaits(t *testing.T) {
	if got := SummarizeWaits(nil); got != (WaitSummary{}) {
		t.Errorf("SummarizeWaits(nil) = %+v, want the zero summary", got)
	}

	entries := make([]HistoryEntry, 0, 20)
	for i := 1; i <= 20; i++ {
		entries = append(entries, HistoryEntry{WaitedS: float64(i)})
	}
	got := SummarizeWaits(entries)
	if got.N != 20 {
		t.Errorf("N = %d, want 20", got.N)
	}
	if got.P50 != 10*time.Second {
		t.Errorf("P50 = %s, want 10s", got.P50)
	}
	if got.P95 != 19*time.Second {
		t.Errorf("P95 = %s, want 19s (nearest rank over 20 samples)", got.P95)
	}
	if got.Max != 20*time.Second {
		t.Errorf("Max = %s, want 20s", got.Max)
	}

	// One sample: every quantile is that sample, not a zero.
	single := SummarizeWaits([]HistoryEntry{{WaitedS: 4}})
	if single.P50 != 4*time.Second || single.P95 != 4*time.Second || single.Max != 4*time.Second {
		t.Errorf("single-sample summary = %+v, want 4s across the board", single)
	}
}

// TestWaitMessage covers the line `gt feed --plain` prints: the payload's
// message is what the feed renders, so the facts and the wording describing
// them have to arrive together.
func TestWaitMessage(t *testing.T) {
	info := waitInfo{
		Reason:  WaitReasonTokenHeld,
		Waited:  4*time.Minute + 12*time.Second,
		Timeout: time.Hour,
		Holder:  &Owner{Role: "gastown/polecats/mica", PID: 62965},
	}
	msg := waitMessage("gastown/refinery", 0, info)
	for _, want := range []string{"gastown/refinery", "4m12s", "slot 0", "token held by gastown/polecats/mica pid 62965"} {
		if !strings.Contains(msg, want) {
			t.Errorf("waitMessage = %q, want it to contain %q", msg, want)
		}
	}

	if msg := waitMessage("gastown/pearl", 1, waitInfo{}); !strings.Contains(msg, "no wait") {
		t.Errorf("waitMessage with no blocker = %q, want it to say there was no wait", msg)
	}
}
