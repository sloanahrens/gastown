package health_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/steveyegge/gastown/internal/jev/health"
)

// logLine renders one fixture log line: a stamp and the answers field as
// written, "null" for a line whose call came back empty.
func logLine(ts time.Time, answers string) string {
	return fmt.Sprintf(`{"ts":%q,"answers":%s}`, ts.UTC().Format(time.RFC3339), answers)
}

// answered builds a line inside the last day that carries an answer.
func answered(now time.Time, ago time.Duration) string {
	return logLine(now.Add(-ago), `{"q":"a"}`)
}

// dayLines builds n answered lines inside baseline day day, day 1 the closest
// to now.
func dayLines(now time.Time, day, n int) []string {
	var lines []string
	for i := 1; i <= n; i++ {
		lines = append(lines, logLine(now.Add(-time.Duration(day)*health.Window-time.Duration(i)*time.Minute), `{"q":"a"}`))
	}
	return lines
}

// baselineLines builds perDay answered lines for each of the seven baseline
// days.
func baselineLines(now time.Time, perDay int) []string {
	var lines []string
	for day := 1; day <= health.BaselineDays; day++ {
		lines = append(lines, dayLines(now, day, perDay)...)
	}
	return lines
}

// parseLines reads fixture log text into the lines a row is judged on.
func parseLines(lines []string) []health.Line {
	return health.ParseLog([]byte(strings.Join(lines, "\n")))
}

// cursorInput is one battery's input with a cursor written age ago.
func cursorInput(now time.Time, age time.Duration, lines ...string) health.Input {
	return health.Input{Cursor: now.Add(-age), HasCursor: true, Log: parseLines(lines)}
}

// logInput is one battery's input for a battery that keeps no cursor.
func logInput(lines ...string) health.Input {
	return health.Input{Log: parseLines(lines)}
}

func enabled(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		out[key] = true
	}
	return out
}

// TestHealthStatuses drives each status rule from a fixture: the status the
// row reports and whether that status fails the command.
func TestHealthStatuses(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name            string
		enabled         []string
		scheduledSlings bool
		inputs          map[string]health.Input
		want            map[string]health.Status
		fails           bool
	}{
		{
			name:    "ok",
			enabled: []string{"jev_dedupe"},
			inputs:  map[string]health.Input{"dedupe": cursorInput(now, 5*time.Minute)},
			want:    map[string]health.Status{"dedupe": health.OK},
		},
		{
			name:    "ok at the stale bound",
			enabled: []string{"jev_dedupe"},
			inputs:  map[string]health.Input{"dedupe": cursorInput(now, health.StaleAfter)},
			want:    map[string]health.Status{"dedupe": health.OK},
		},
		{
			name:    "stale",
			enabled: []string{"jev_dedupe"},
			inputs:  map[string]health.Input{"dedupe": cursorInput(now, health.StaleAfter+time.Second)},
			want:    map[string]health.Status{"dedupe": health.Stale},
			fails:   true,
		},
		{
			name:   "disabled",
			inputs: map[string]health.Input{"dedupe": cursorInput(now, time.Minute)},
			want:   map[string]health.Status{"dedupe": health.Disabled},
		},
		{
			name:    "late",
			enabled: []string{"jev_notes"},
			inputs:  map[string]health.Input{"notes": logInput(logLine(now.Add(-9*health.Window), `{"q":"a"}`))},
			want:    map[string]health.Status{"notes": health.Late},
			fails:   true,
		},
		{
			name:    "no pass recorded",
			enabled: []string{"jev_dedupe", "jev_notes"},
			want: map[string]health.Status{
				"dedupe": health.Stale,
				"notes":  health.Late,
			},
			fails: true,
		},
		{
			name:    "idle by design, progress",
			enabled: []string{"jev_progress"},
			want:    map[string]health.Status{"progress": health.Idle},
		},
		{
			name:    "idle by design, failcause",
			enabled: []string{"jev_failcause"},
			want:    map[string]health.Status{"failcause": health.Idle},
		},
		{
			name:    "idle by design, targets while scheduled_slings is off",
			enabled: []string{"jev_targets"},
			want:    map[string]health.Status{"targets": health.Idle},
		},
		{
			name:            "targets is judged once scheduled_slings is on",
			enabled:         []string{"jev_targets"},
			scheduledSlings: true,
			inputs:          map[string]health.Input{"targets": logInput(answered(now, time.Hour))},
			want:            map[string]health.Status{"targets": health.OK},
		},
		{
			name:    "stale wins over low",
			enabled: []string{"jev_dedupe"},
			inputs: map[string]health.Input{"dedupe": {
				Cursor: now.Add(-2 * time.Hour), HasCursor: true,
				Log: parseLines(baselineLines(now, 100)),
			}},
			want:  map[string]health.Status{"dedupe": health.Stale},
			fails: true,
		},
		{
			name:    "low",
			enabled: []string{"jev_dedupe"},
			inputs: map[string]health.Input{"dedupe": cursorInput(now, 5*time.Minute,
				append(baselineLines(now, 100), answered(now, time.Hour))...)},
			want:  map[string]health.Status{"dedupe": health.Low},
			fails: true,
		},
		{
			name:    "ok at a quarter of the median",
			enabled: []string{"jev_dedupe"},
			inputs: map[string]health.Input{"dedupe": {
				Cursor: now.Add(-5 * time.Minute), HasCursor: true,
				Log: parseLines(append(baselineLines(now, 100), answeredLines(now, 25)...)),
			}},
			want: map[string]health.Status{"dedupe": health.OK},
		},
		{
			name:    "unanswered",
			enabled: []string{"jev_dedupe"},
			inputs: map[string]health.Input{"dedupe": cursorInput(now, 5*time.Minute,
				answered(now, time.Hour), logLine(now.Add(-2*time.Hour), "null"),
				logLine(now.Add(-3*time.Hour), `{}`))},
			want:  map[string]health.Status{"dedupe": health.Unanswered},
			fails: true,
		},
		{
			name:    "ok at half unanswered",
			enabled: []string{"jev_dedupe"},
			inputs: map[string]health.Input{"dedupe": cursorInput(now, 5*time.Minute,
				answered(now, time.Hour), answered(now, 2*time.Hour),
				logLine(now.Add(-3*time.Hour), "null"), logLine(now.Add(-4*time.Hour), "null"))},
			want: map[string]health.Status{"dedupe": health.OK},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := health.Report(health.Config{
				Enabled:         enabled(tc.enabled...),
				ScheduledSlings: tc.scheduledSlings,
			}, tc.inputs, nil, now)

			byName := make(map[string]health.Row, len(rows))
			for _, r := range rows {
				byName[r.Name] = r
			}
			for name, want := range tc.want {
				assert.Equal(t, want, byName[name].Status, "%s: status (reason %q)", name, byName[name].Reason)
			}
			assert.Equal(t, tc.fails, health.ExitFails(rows), "exit decision")
		})
	}
}

// answeredLines builds n answered lines inside the last day.
func answeredLines(now time.Time, n int) []string {
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, answered(now, time.Duration(i+1)*time.Minute))
	}
	return lines
}

// TestHealthRowCounts checks the counts a row's verdict rests on, including
// the 24-hour window's edges.
func TestHealthRowCounts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	lines := []string{
		answered(now, time.Hour),
		logLine(now.Add(-2*time.Hour), "null"),
		answered(now, 23*time.Hour),
		// Just inside the window: at now-24h+1s.
		answered(now, health.Window-time.Second),
		// Just outside the window: at now-24h-1s, and inside the first
		// baseline day.
		logLine(now.Add(-health.Window-time.Second), "null"),
		// Neither names a pass a window can count.
		`{"ts":`,
		`{"answers":{"q":"a"}}`,
	}
	lines = append(lines, dayLines(now, 1, 3)...) // day 1 then holds 4 lines with the outside one
	lines = append(lines, dayLines(now, 2, 2)...)
	lines = append(lines, dayLines(now, 3, 5)...)
	lines = append(lines, dayLines(now, 4, 4)...)
	lines = append(lines, dayLines(now, 5, 6)...)
	lines = append(lines, dayLines(now, 6, 7)...)
	lines = append(lines, dayLines(now, 7, 8)...)

	inputs := map[string]health.Input{"dedupe": cursorInput(now, 5*time.Minute, lines...)}
	ledger := map[string]int{"jev-dedupe": 2, "jev-thrash": 1}

	rows := health.Report(health.Config{Enabled: enabled("jev_dedupe")}, inputs, ledger, now)

	var dedupe health.Row
	for _, r := range rows {
		if r.Name == "dedupe" {
			dedupe = r
		}
	}

	assert.Equal(t, 4, dedupe.Lines24, "the four lines in the window count, the line just outside does not")
	assert.Equal(t, 5, dedupe.Median7, "the median of [2 4 4 5 6 7 8] lines a day")
	assert.Equal(t, 2, dedupe.LedgerCalls, "only the battery's own caller's calls count")
	assert.InDelta(t, 0.25, dedupe.NoAnswerShare, 1e-9, "one of the four window lines carries no answers")
	assert.Equal(t, health.OK, dedupe.Status, "reason %q", dedupe.Reason)
}
