package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townhealth"
)

// TestSweepRecordOfKnown pins that a read state becomes a known record with
// the red cycle count and the last green time copied as they are (gt-ffs6cx).
func TestSweepRecordOfKnown(t *testing.T) {
	t.Parallel()
	greenAt := time.Date(2026, 11, 22, 19, 0, 0, 0, time.UTC)
	got := sweepRecordOf(tierSweepState{RedCycles: 3, LastGreenAt: greenAt}, nil)
	want := townhealth.SweepRecord{Known: true, RedCycles: 3, LastGreenAt: greenAt}
	if got != want {
		t.Errorf("sweepRecordOf = %+v, want %+v", got, want)
	}
}

// TestSweepRecordOfUnread pins that an unread state is unknown whatever it
// holds: a record that could not be read is never green (gt-ffs6cx).
func TestSweepRecordOfUnread(t *testing.T) {
	t.Parallel()
	greenAt := time.Date(2026, 11, 22, 19, 0, 0, 0, time.UTC)
	got := sweepRecordOf(tierSweepState{RedCycles: 3, LastGreenAt: greenAt}, errors.New("the record file cannot be read"))
	want := townhealth.SweepRecord{}
	if got != want {
		t.Errorf("sweepRecordOf with an error = %+v, want the unknown record %+v", got, want)
	}
}

// TestCombineSweepRecordsMerges pins how the swept rigs' records become the
// town's one record: the largest red cycle count, the oldest green time, and
// unknown when any rig is unread or the slice is empty (gt-gm0ek2).
func TestCombineSweepRecordsMerges(t *testing.T) {
	t.Parallel()
	older := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		recs []townhealth.SweepRecord
		want townhealth.SweepRecord
	}{
		{"largest red cycles and oldest green", []townhealth.SweepRecord{
			{Known: true, RedCycles: 1, LastGreenAt: newer},
			{Known: true, RedCycles: 3, LastGreenAt: older},
		}, townhealth.SweepRecord{Known: true, RedCycles: 3, LastGreenAt: older}},
		{"a rig with no green time leaves the town without one", []townhealth.SweepRecord{
			{Known: true, RedCycles: 1, LastGreenAt: older},
			{Known: true, RedCycles: 2},
			{Known: true, RedCycles: 1, LastGreenAt: newer},
		}, townhealth.SweepRecord{Known: true, RedCycles: 2}},
		{"an unread record is unknown", []townhealth.SweepRecord{
			{Known: true, RedCycles: 1, LastGreenAt: older},
			{Known: false},
		}, townhealth.SweepRecord{}},
		{"an empty slice is unknown", nil, townhealth.SweepRecord{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := combineSweepRecords(tc.recs); got != tc.want {
				t.Errorf("combineSweepRecords(%+v) = %+v, want %+v", tc.recs, got, tc.want)
			}
		})
	}
}
