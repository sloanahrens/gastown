package beads

import (
	"errors"
	"reflect"
	"testing"
)

type journalConfigStub struct {
	value  string
	getErr error
	setErr error
	sets   [][2]string
}

func (s *journalConfigStub) ConfigGet(string) (string, error) { return s.value, s.getErr }
func (s *journalConfigStub) ConfigSet(k, v string) error {
	s.sets = append(s.sets, [2]string{k, v})
	return s.setErr
}

func TestEnsureEventsJournal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		stub      journalConfigStub
		wantWrote bool
		wantSets  [][2]string
		wantErr   bool
	}{
		{name: "on is left alone", stub: journalConfigStub{value: "true"}},
		{name: "off is turned on", stub: journalConfigStub{value: "false"}, wantWrote: true, wantSets: [][2]string{{"events-journal", "true"}}},
		{name: "unset is turned on", stub: journalConfigStub{}, wantWrote: true, wantSets: [][2]string{{"events-journal", "true"}}},
		{name: "unreadable is not written", stub: journalConfigStub{getErr: errors.New("down")}, wantErr: true},
		{name: "refused write is an error", stub: journalConfigStub{value: "false", setErr: errors.New("ro")}, wantSets: [][2]string{{"events-journal", "true"}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := tc.stub
			wrote, err := EnsureEventsJournal(&stub)
			if wrote != tc.wantWrote || (err != nil) != tc.wantErr || !reflect.DeepEqual(stub.sets, tc.wantSets) {
				t.Errorf("wrote=%v err=%v sets=%v; want wrote=%v err=%v sets=%v", wrote, err, stub.sets, tc.wantWrote, tc.wantErr, tc.wantSets)
			}
		})
	}
}

func TestEventsJournalProbeEnv(t *testing.T) {
	t.Parallel()
	got := EventsJournalProbeEnv([]string{"A=1", "BD_EVENTS_JOURNAL=1", "BEADS_DIR=/other"}, "/rig/.beads")
	want := []string{"A=1", "BEADS_DIR=/rig/.beads"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EventsJournalProbeEnv = %v, want %v", got, want)
	}
}
