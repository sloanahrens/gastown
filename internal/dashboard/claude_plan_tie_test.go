package dashboard

import (
	"fmt"
	"testing"
	"time"
)

// The readings one second held on 2026-01-... (gt-x1rq13): several writers
// append to one series, and the ones that lag write a lower percentage for the
// same window in the same second. The first line of the second is a lagging 0,
// the current writers write 7, and the whole week's window resets at
// tiedWeeklyResetsAt.
const (
	tiedSecond      = int64(1795614914)
	tiedWeeklyReset = int64(1796198400)
)

// tiedSample is one reading of the tied second: a weekly percentage under
// tiedWeeklyReset, and a five-hour percentage of the window ending at fiveAt.
func tiedSample(seven, five float64, fiveAt int64) ClaudePlanSample {
	return ClaudePlanSample{
		T: tiedSecond, SevenDay: seven, SevenDayResetsAt: tiedWeeklyReset,
		FiveHour: five, FiveHourResetsAt: fiveAt,
	}
}

// A weekly percentage never falls inside one window, so the newest reading is
// the newest window's MAX: a lagging writer's lower line in the same second
// must not drag the meter down, whichever order the lines were appended in.
func TestComputeClaudePlanTiedSecond(t *testing.T) {
	t.Parallel()

	now := time.Unix(tiedSecond, 0).Add(time.Minute)
	fiveAt := time.Unix(tiedSecond, 0).Add(3 * time.Hour).Unix()

	// The seven lines the Cost panel read as 0% while the week stood at 7%:
	// two lagging writers, 0 and 5, among five current ones.
	seven := []ClaudePlanSample{
		tiedSample(0, 4, fiveAt),
		tiedSample(7, 4, fiveAt),
		tiedSample(5, 4, fiveAt),
		tiedSample(7, 4, fiveAt),
		tiedSample(7, 4, fiveAt),
		tiedSample(7, 4, fiveAt),
		tiedSample(7, 4, fiveAt),
	}

	for i := range seven {
		rotated := append(append([]ClaudePlanSample{}, seven[i:]...), seven[:i]...)
		p := ComputeClaudePlan(rotated, now, ClaudePlanDefaultUSD)
		if p.WeeklyPct != 7 {
			t.Errorf("rotation %d: weekly_pct = %v, want the window's max 7; samples %v", i, p.WeeklyPct, rotated)
		}
	}

	// A lower reading from an earlier second of the same window is no reason to
	// lower the newest second's reading.
	earlier := tiedSample(5, 4, fiveAt)
	earlier.T = tiedSecond - 60
	if p := ComputeClaudePlan([]ClaudePlanSample{earlier, tiedSample(7, 4, fiveAt)}, now, ClaudePlanDefaultUSD); p.WeeklyPct != 7 {
		t.Errorf("with a lower earlier reading: weekly_pct = %v, want 7", p.WeeklyPct)
	}

	// A sample of a window that ended a week ago is not part of this week,
	// however high its percentage.
	ended := tiedSample(99, 4, fiveAt)
	ended.T = tiedSecond - 60
	ended.SevenDayResetsAt = tiedWeeklyReset - claudePlanWeekSecs
	if p := ComputeClaudePlan([]ClaudePlanSample{ended, tiedSample(7, 4, fiveAt)}, now, ClaudePlanDefaultUSD); p.WeeklyPct != 7 {
		t.Errorf("with an ended window's higher reading: weekly_pct = %v, want 7", p.WeeklyPct)
	}

	// The five-hour meter takes the max of its own window, not of the tied
	// second's first line nor of a five-hour window that has ended.
	fiveEnded := tiedSample(7, 90, fiveAt-4*3600)
	fiveEnded.T = tiedSecond - 60
	five := []ClaudePlanSample{fiveEnded, tiedSample(7, 0, fiveAt), tiedSample(7, 12, fiveAt), tiedSample(7, 9, fiveAt)}
	if p := ComputeClaudePlan(five, now, ClaudePlanDefaultUSD); p.FiveHourPct != 12 {
		t.Errorf("five_hour_pct = %v, want the five-hour window's max 12; samples %v", p.FiveHourPct, fmt.Sprint(five))
	}
}
