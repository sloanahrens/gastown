package dashboard

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// A seats read that fails is not a town with no polecats. The hub keeps the
// last good list, flags the failure, and — because the snapshot it hands the
// alert rules has not changed — neither re-announces a stalled polecat when the
// reader recovers nor acts on a change that never happened (gt-q6h8e).
func TestFailedSeatsReadKeepsThePreviousSeatsAndHoldsTheAlerts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	stalled := Polecat{Rig: "gastown", Name: "agate", Bead: "gt-abc", State: StateStalled}
	calls := 0
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Summary: func() Summary {
			calls++
			if calls == 2 {
				return Summary{SeatsError: true} // the seats reader dropped one poll
			}
			return Summary{Polecats: []Polecat{stalled}}
		},
	})

	h.pollSummary() // the first snapshot a rule sees is a baseline, not a change
	h.pollSummary() // the seats read fails

	st := h.State()
	if len(st.Polecats) != 1 || st.Polecats[0].Name != "agate" {
		t.Fatalf("a failed seats read dropped the previous seats: %+v", st.Polecats)
	}
	if r := st.Reads[SourceSeats]; !r.Error || r.At.IsZero() {
		t.Errorf("the failed seats read is not flagged with its last good age: %+v", r)
	}
	if len(st.Alerts) != 0 {
		t.Errorf("a failed seats read raised alerts: %+v", st.Alerts)
	}

	h.pollSummary() // the reader recovers with the same stalled polecat
	if st := h.State(); len(st.Alerts) != 0 {
		t.Errorf("recovering from a failed seats read re-announced stalled polecats: %+v", st.Alerts)
	}
	if r := h.State().Reads[SourceSeats]; r.Error {
		t.Errorf("a recovered seats read is still flagged: %+v", r)
	}
}

// A ready-to-land read that fails keeps the previous oldest waiting bead, so
// the stuck-queue rule stays armed instead of re-alerting on the next jam
// (gt-q6h8e).
func TestFailedReadyReadKeepsTheStuckQueueAlertArmed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-40 * time.Minute)
	fail := false
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Summary: func() Summary {
			if fail {
				return Summary{ReadyError: true, Polecats: []Polecat{{Rig: "gastown", Name: "agate", State: StateWorking}}}
			}
			n := 1
			return Summary{ReadyToLand: &n, OldestReady: &oldest, Polecats: []Polecat{{Rig: "gastown", Name: "agate", State: StateWorking}}}
		},
	})

	h.pollSummary()
	if st := h.State(); len(st.Alerts) != 1 {
		t.Fatalf("a bead waiting 40 minutes did not alert: %+v", st.Alerts)
	}

	// Past the cooldown, so a rule that re-armed over the failed read would fire.
	now = now.Add(11 * time.Minute)
	fail = true
	h.pollSummary()
	st := h.State()
	if st.Summary == nil || st.Summary.OldestReady == nil {
		t.Fatal("a failed ready-to-land read dropped the oldest waiting bead")
	}
	if len(st.Alerts) != 1 {
		t.Errorf("a failed ready-to-land read re-alerted: %+v", st.Alerts)
	}

	now = now.Add(11 * time.Minute)
	fail = false
	h.pollSummary()
	if st := h.State(); len(st.Alerts) != 1 {
		t.Errorf("the stuck-queue alert fired again after a failed read: %+v", st.Alerts)
	}
}

// Machine, Spend, OM and Queue each keep the last good value when their read
// fails, and the payload says so: the pane ages and greys that value instead of
// showing it as current (gt-q6h8e).
func TestFailedMachineSpendOMQueueReadsKeepTheValueAndSaySo(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	ok := true
	boom := errors.New("read failed")
	h := NewHub(Config{
		Now:     func() time.Time { return now },
		Machine: func() (Machine, error) { return Machine{Load1: 4, At: now}, errIf(!ok, boom) },
		Spend:   func() json.RawMessage { return jsonIf(ok) },
		OM:      func() *OM { return omIf(ok) },
		Queue:   func() *Queue { return queueIf(ok, now) },
	})

	h.pollMachine()
	h.pollSpend()
	h.pollOM()
	h.pollQueue()
	ok = false
	h.pollMachine()
	h.pollSpend()
	h.pollOM()
	h.pollQueue()

	st := h.State()
	if st.Machine.Load1 != 4 {
		t.Errorf("a failed machine read dropped the last good sample: %+v", st.Machine)
	}
	if len(st.Spend) == 0 || st.OM == nil || st.Queue == nil {
		t.Errorf("a failed read dropped a last good value: spend=%s om=%v queue=%v", st.Spend, st.OM, st.Queue)
	}
	for _, source := range []string{SourceMachine, SourceSpend, SourceOM, SourceQueue} {
		r := st.Reads[source]
		if !r.Error {
			t.Errorf("%s: a failed read is not flagged: %+v", source, r)
		}
		if !r.Stale {
			t.Errorf("%s: a failed read is not marked stale: %+v", source, r)
		}
		if r.Every <= 0 || r.At.IsZero() {
			t.Errorf("%s: the payload carries no age or interval to judge: %+v", source, r)
		}
	}

	// The page reads this off the state JSON, so the status travels with the
	// value rather than beside it in a field only a test can see.
	payload, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	for _, want := range []string{`"reads"`, `"machine"`, `"queue"`, `"every_sec"`, `"error":true`, `"stale":true`} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("the state payload has no %s", want)
		}
	}

	// A read that has not failed but has gone past a few of its own intervals is
	// stale too, which is what a poller that stopped reporting looks like.
	ok = true
	h.pollMachine()
	now = now.Add(4 * h.cfg.MachineEvery)
	if r := h.State().Reads[SourceMachine]; !r.Stale || r.Error {
		t.Errorf("a machine reading older than a few intervals is not stale: %+v", r)
	}
}

func errIf(bad bool, err error) error {
	if bad {
		return err
	}
	return nil
}

func jsonIf(ok bool) json.RawMessage {
	if !ok {
		return nil
	}
	return json.RawMessage(`{"balance":12}`)
}

func omIf(ok bool) *OM {
	if !ok {
		return nil
	}
	return &OM{Backend: "claude"}
}

func queueIf(ok bool, now time.Time) *Queue {
	if !ok {
		return nil
	}
	return &Queue{At: now}
}
