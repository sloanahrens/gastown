package dashboard

import (
	"testing"
	"time"
)

// The strip's alarm is the reader's verdict, so every branch that can raise
// one is driven here: what each reading earns, and the boundary its threshold
// turns at (gt-ywlei).
func TestStripAlarmsTurnWithTheReadingsTheStripHolds(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	behind := func(n int) *Summary { return &Summary{Behind: &n} }
	fresh := ReadStatus{At: now, Every: 60}
	aged := ReadStatus{At: now.Add(-10 * time.Minute), Every: 60}
	failed := ReadStatus{At: now.Add(-10 * time.Minute), Every: 60, Error: true}
	polecats := func(states ...string) []Polecat {
		out := make([]Polecat, 0, len(states))
		for _, s := range states {
			out = append(out, Polecat{State: s})
		}
		return out
	}

	for _, tc := range []struct {
		name  string
		state State
		want  StripAlarms
	}{
		{
			name:  "a town nobody has read earns no alarm at all",
			state: State{Now: now},
		},
		{
			name:  "a queue older than the warning but short of the bad reading is amber",
			state: State{Now: now, Summary: &Summary{OldestReady: ago(stripWaitWarn + time.Minute)}},
			want:  StripAlarms{Waiting: alarmWarn},
		},
		{
			name:  "a queue on the bad reading is red",
			state: State{Now: now, Summary: &Summary{OldestReady: ago(stripWaitBad)}},
			want:  StripAlarms{Waiting: alarmBad},
		},
		{
			name:  "a queue under the warning is nothing to act on",
			state: State{Now: now, Summary: &Summary{OldestReady: ago(stripWaitWarn - time.Minute)}},
		},
		{
			name: "a ready read that failed is red however old its count",
			state: State{Now: now, Summary: &Summary{OldestReady: ago(time.Minute)},
				Reads: map[string]ReadStatus{SourceReady: failed}},
			want: StripAlarms{Waiting: alarmBad},
		},
		{
			name: "a ready read gone old is amber, and its age does not soften it",
			state: State{Now: now, Summary: &Summary{OldestReady: ago(time.Minute)},
				Reads: map[string]ReadStatus{SourceReady: aged}},
			want: StripAlarms{Waiting: alarmWarn},
		},
		{
			name: "a current ready read leaves the age of the oldest to speak",
			state: State{Now: now, Summary: &Summary{OldestReady: ago(stripWaitBad)},
				Reads: map[string]ReadStatus{SourceReady: fresh}},
			want: StripAlarms{Waiting: alarmBad},
		},
		{
			name:  "an install on the warning distance is nothing to act on",
			state: State{Now: now, Summary: behind(stripBehindWarn)},
		},
		{
			name:  "an install past it is amber",
			state: State{Now: now, Summary: behind(stripBehindWarn + 1)},
			want:  StripAlarms{Behind: alarmWarn},
		},
		{
			name:  "an install nobody has read is not a current one, and not an alarm either",
			state: State{Now: now, Summary: &Summary{}},
		},
		{
			name:  "a stalled polecat is red",
			state: State{Now: now, Polecats: polecats(StateWorking, StateStalled)},
			want:  StripAlarms{Working: alarmBad},
		},
		{
			name:  "a quiet polecat is amber",
			state: State{Now: now, Polecats: polecats(StateWorking, StateQuiet)},
			want:  StripAlarms{Working: alarmWarn},
		},
		{
			name:  "the worst of them is the one the strip wears",
			state: State{Now: now, Polecats: polecats(StateQuiet, StateNeedsHuman)},
			want:  StripAlarms{Working: alarmBad},
		},
		{
			name:  "a polecat in a state this reader does not know earns nothing",
			state: State{Now: now, Polecats: polecats("warping")},
		},
		{
			name: "a parked rig is stood down on purpose, its stalled polecats with it",
			state: State{Now: now, Polecats: []Polecat{
				{State: StateStalled, RigParked: true},
				{State: StateWorking, RigParked: false},
			}},
		},
		{
			name:  "the states that are the town working as intended earn nothing",
			state: State{Now: now, Polecats: polecats(StateQueued, StateSpawning, StateGating, StateIdle)},
		},
		{
			name:  "a list nobody could read is red",
			state: State{Now: now, Attention: &Attention{Failed: []string{"escalations"}}},
			want:  StripAlarms{NeedsYou: alarmBad},
		},
		{
			name:  "a decision waiting on the human is red",
			state: State{Now: now, Attention: &Attention{Rows: []AttentionRow{{Owner: "operator"}}, Mine: 1}},
			want:  StripAlarms{NeedsYou: alarmBad},
		},
		{
			name:  "rows only the overseer has to move are not the human's alarm",
			state: State{Now: now, Attention: &Attention{Rows: []AttentionRow{{Owner: "overseer"}}}},
		},
		{
			name: "a cap with no human row read is a count nobody made",
			state: State{Now: now, Attention: &Attention{
				Rows: []AttentionRow{{Owner: "overseer"}}, More: 3,
			}},
			want: StripAlarms{NeedsYou: alarmWarn},
		},
		{
			name:  "a list with nothing waiting is nothing to act on",
			state: State{Now: now, Attention: &Attention{}},
		},
		{
			name:  "a reader that has not reported yet is not an alarm",
			state: State{Now: now, Attention: &Attention{Pending: []string{"questions"}}},
		},
		{
			name:  "a list that has not been built yet is not an alarm",
			state: State{Now: now, Escalations: &Escalations{}, Questions: &Questions{}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stripAlarms(&tc.state); *got != tc.want {
				t.Errorf("stripAlarms = %+v, want %+v", *got, tc.want)
			}
		})
	}
}

// Every state this package can put on a polecat has a row in the alarm table,
// and only the three that mean somebody should look are colored: the reader
// learns a state the moment the town does, and a state missing from the table
// would be a polecat the strip cannot see (gt-ywlei).
func TestTheAlarmTableCoversEveryStateTheReaderSends(t *testing.T) {
	t.Parallel()

	states := []string{
		StateWorking, StateQuiet, StateGating, StateQueued, StateSpawning, StateStalled,
		StateReviewNeeded, StateRecovery, StateNeedsHuman, StateNeedsOverseer, StateNeedsOperator,
		StateParked, StateIdle, StateUnknown,
	}
	want := map[string]string{
		StateQuiet: alarmWarn, StateStalled: alarmBad,
		StateNeedsHuman: alarmBad, StateNeedsOverseer: alarmBad, StateNeedsOperator: alarmBad,
	}
	for _, state := range states {
		got, ok := polecatAlarm[state]
		if !ok {
			t.Errorf("polecatAlarm has no row for %q, so the strip cannot color it", state)
			continue
		}
		if got != want[state] {
			t.Errorf("polecatAlarm colors %q %q, want %q", state, got, want[state])
		}
	}
	for state := range polecatAlarm {
		if !contains(states, state) {
			t.Errorf("polecatAlarm names %q, which this package cannot put on a polecat", state)
		}
	}
	// A token the stylesheet does not define draws the alarm in the body
	// color, which is an alarm nobody sees.
	for state, alarm := range polecatAlarm {
		if alarm != "" && alarm != alarmWarn && alarm != alarmBad {
			t.Errorf("polecatAlarm colors %q %q, which is not one of the strip's tokens", state, alarm)
		}
	}
}

// The hub decides the verdicts from the state it holds, so every snapshot the
// page is handed carries them and the page holds no threshold of its own.
func TestTheHubStampsTheStripOnEveryStateItHandsOut(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	h := NewHub(Config{Now: func() time.Time { return now }})
	behind := stripBehindWarn + 1
	h.state.Summary = &Summary{Behind: &behind}

	st := h.State()
	if st.Strip == nil || st.Strip.Behind != alarmWarn {
		t.Fatalf("State().Strip = %+v, want the reader's own verdict on the install", st.Strip)
	}
}
