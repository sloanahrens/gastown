package dashboard

import (
	"strings"
	"testing"
	"time"
)

// landingsRateReading is a Landings window whose hours hold 48 landings in all,
// with the rolling last-hour count the trend builder sends. A nil lastHour is a
// feed that carries no landed_last_hour field at all, which an older page's
// reading does.
func landingsRateReading(lastHour any) map[string]any {
	hour := func(i int) string { return velocityPaneHour(i).Format(time.RFC3339) }
	trend := map[string]any{"hours": []map[string]any{
		{"hour": hour(0), "landed": 20, "rejected": 0},
		{"hour": hour(1), "landed": 20, "rejected": 0},
		{"hour": hour(2), "landed": 8, "rejected": 0},
	}}
	if lastHour != nil {
		trend["landed_last_hour"] = lastHour
	}
	return map[string]any{"trend": trend}
}

// The figures row carries the landing rate right after the day's count: the
// 24-hour average per hour to one decimal, and the rolling last hour as the
// feed sent it, each with a tooltip saying what it counts (gt-n89i71).
func TestLandingsRateFigures(t *testing.T) {
	t.Parallel()

	t.Run("the rate and the last hour read off the feed", func(t *testing.T) {
		t.Parallel()

		p := newPageDOM(t)
		p.call(t, "renderTrend", landingsRateReading(3))
		row := readFigureRow(t, p)

		if got := figureNamed(t, row, "landings/h 24h"); got.Value != "2.0" {
			t.Errorf("landings/h 24h reads %q, want 2.0 (48 landings over 24 hours)", got.Value)
		} else if !strings.Contains(got.Title, "24 hours") {
			t.Errorf("landings/h 24h tooltip %q does not say what it counts", got.Title)
		}
		if got := figureNamed(t, row, "last hour"); got.Value != "3" {
			t.Errorf("last hour reads %q, want 3", got.Value)
		} else if !strings.Contains(got.Title, "60 minutes") {
			t.Errorf("last hour tooltip %q does not say what it counts", got.Title)
		}
	})

	t.Run("a feed without the field draws a dash, never zero", func(t *testing.T) {
		t.Parallel()

		p := newPageDOM(t)
		p.call(t, "renderTrend", landingsRateReading(nil))
		row := readFigureRow(t, p)

		if got := figureNamed(t, row, "last hour"); got.Value != "–" {
			t.Errorf("last hour reads %q with no landed_last_hour field, want a dash", got.Value)
		}
	})
}
