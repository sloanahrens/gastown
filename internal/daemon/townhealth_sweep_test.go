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
