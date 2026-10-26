package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// The money group: what the town is spending, read from the files the Cost
// panel already reads. Before changing a column or a file's shape, read
// internal/dashboard/usage.go and internal/dashboard/claude_plan.go, which hold
// the payload types and the fold this source reuses.
//
// The Claude plan columns come from the status line's series,
// <town>/.runtime/claude-plan/samples.jsonl: one JSON object per reading, with
// t, five_hour, seven_day and each limit's resets_at in epoch seconds. The
// fold is dashboard.ComputeClaudePlan, the same one the plan meter draws, so
// the percent and the pace below are the panel's numbers.
//
//	claude_plan_weekly_pct            the newest reading's seven-day percentage
//	claude_plan_five_hour_pct         its five-hour percentage
//	claude_plan_weekly_pace_per_day_1h  the weekly percentage's rise per day
//	                                  over the current window's last hour
//	claude_plan_stale                 the newest reading is old or its window
//	                                  has ended, so the percentages above are
//	                                  the last reading and not the current week
//
// The DeepSeek columns come from the health watch's reading,
// <town>/.runtime/watch/spend.json: {"ts", "per_hour", "balance"}, rewritten
// as the watch measures the account.
//
//	deepseek_per_hour      the spend rate in dollars per hour
//	deepseek_balance       the account's remaining balance in dollars
//	deepseek_spend_age_s   how old the reading is, so a rate that stopped
//	                       being measured is not read as the current one
//
// The token columns come from the usage scan's per-record files,
// <town>/.runtime/usage/beads/*.json, one dashboard.UsageRecord per bead and
// kind. A record counts when it is complete and its end falls in the last
// hour; each model's column is the uncached tokens the record spent on it,
// input, output and cache creation, the total the Cost panel's per-model table
// adds. The model name is kept as the writer spelled it, with any character
// outside letters, digits, '-' and '_' turned into '_' so the column name is a
// table header.
//
//	tokens_<model>_1h  uncached tokens on that model in the last hour, nil
//	                   when the directory names the model but no complete
//	                   record for it ended in the hour
//
// The token columns follow the models the directory holds rather than the
// hour's data: a model's column is nil, not absent, while its records are
// outside the window, so the columns a snapshot carries do not change as a
// model's use moves in and out of the hour.
//
// Three columns count what the group could not use, so a nil column that came
// from a broken input is distinguishable from one whose file is simply not
// there:
//
//	money_read_failures       the plan series, the spend document or the usage
//	                          listing the group could not read at all: only
//	                          that input's columns stay nil, and the other two
//	                          still answer
//	money_unreadable_records  usage record files the directory listed and the
//	                          read failed for, so the token sums cover the
//	                          records that could be read and this counts the
//	                          ones that were not
//	money_unparsed_inputs     the inputs the group could not turn into a
//	                          reading: a plan line that will not parse or
//	                          carries no instant, a spend document that will
//	                          not decode or carries no timestamp, and a usage
//	                          record that will not decode
//
// A file that is not there leaves its columns nil: no reading is not a reading
// of zero, and a town without a Claude plan or a usage scan still records its
// other spending. A read that fails for any other reason is one failed input,
// not a failed group: its columns stay nil, money_read_failures counts it, and
// Collect carries on with the inputs that still answer. The group has no
// failure of its own, so Collect returns no error: an unreadable usage
// directory costs its token sums and a count, not the DeepSeek rate or the
// plan percentages. A line of the series that will not parse, and a spend
// document that will not decode or carries no timestamp, are skipped rather
// than failing the group, and counted. A usage record that cannot be read or
// decoded is skipped too — it is one run of a directory of them, not the
// group's input — and counted in the column for how it failed.
type moneySource struct{}

var (
	moneyClaudePlanFile = filepath.Join("claude-plan", "samples.jsonl")
	moneySpendFile      = filepath.Join("watch", "spend.json")
	moneyUsageDir       = filepath.Join("usage", "beads")
)

func (moneySource) Group() string { return "money" }

func (moneySource) Collect(_ context.Context, env Env) (map[string]any, error) {
	cols := map[string]any{
		"claude_plan_weekly_pct":             nil,
		"claude_plan_five_hour_pct":          nil,
		"claude_plan_weekly_pace_per_day_1h": nil,
		"claude_plan_stale":                  nil,
		"deepseek_per_hour":                  nil,
		"deepseek_balance":                   nil,
		"deepseek_spend_age_s":               nil,
	}
	gaps := &moneyGaps{}
	moneyAddClaudePlan(env, cols, gaps)
	moneyAddDeepSeek(env, cols, gaps)
	moneyAddTokens(env, cols, gaps)
	cols["money_unreadable_records"] = gaps.unreadableRecords
	cols["money_unparsed_inputs"] = gaps.unparsedInputs
	cols["money_read_failures"] = gaps.readFailures
	return cols, nil
}

// moneyGaps counts what the group could not use, so an input that broke is a
// number in the record and not a column that silently reads as absent.
type moneyGaps struct {
	// readFailures is the inputs the group could not read at all, other than a
	// file that is not there. Each costs only its own columns.
	readFailures int
	// unreadableRecords is usage record files the listing named but the read
	// failed for, other than a file that went away between the two.
	unreadableRecords int
	// unparsedInputs is lines, documents and records the group could not turn
	// into a reading.
	unparsedInputs int
}

// moneyAddClaudePlan fills the plan columns from the status line's tail.
func moneyAddClaudePlan(env Env, cols map[string]any, gaps *moneyGaps) {
	data, err := env.Tail(filepath.Join(constants.TownRuntimePath(env.Town), moneyClaudePlanFile), sourceLogTailBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		gaps.readFailures++
		return
	}
	var samples []dashboard.ClaudePlanSample
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var sample dashboard.ClaudePlanSample
		// A reading with no instant cannot be placed against any other, so a
		// line without one is as useless as a line that will not parse, and
		// both are counted rather than dropped in silence.
		if json.Unmarshal(line, &sample) != nil || sample.T <= 0 {
			gaps.unparsedInputs++
			continue
		}
		samples = append(samples, sample)
	}
	if len(samples) == 0 {
		return
	}
	plan := dashboard.ComputeClaudePlan(samples, env.Now(), dashboard.ClaudePlanDefaultUSD)
	cols["claude_plan_weekly_pct"] = plan.WeeklyPct
	cols["claude_plan_five_hour_pct"] = plan.FiveHourPct
	cols["claude_plan_weekly_pace_per_day_1h"] = plan.PacePerDay1h
	cols["claude_plan_stale"] = plan.Stale
}

// moneyAddDeepSeek fills the DeepSeek columns from the watch's reading.
func moneyAddDeepSeek(env Env, cols map[string]any, gaps *moneyGaps) {
	path := filepath.Join(constants.TownRuntimePath(env.Town), moneySpendFile)
	data, err := env.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		gaps.readFailures++
		return
	}
	var doc struct {
		TS      time.Time `json:"ts"`
		PerHour *float64  `json:"per_hour"`
		Balance *float64  `json:"balance"`
	}
	// A document that will not decode, like one with no timestamp, is no
	// reading at all: it is counted so a nil rate is not mistaken for a watch
	// that has never written the file.
	if err := json.Unmarshal(data, &doc); err != nil {
		gaps.unparsedInputs++
		return
	}
	if doc.TS.IsZero() {
		gaps.unparsedInputs++
		return
	}
	now := env.Now()
	if doc.PerHour != nil {
		cols["deepseek_per_hour"] = *doc.PerHour
	}
	if doc.Balance != nil {
		cols["deepseek_balance"] = *doc.Balance
	}
	cols["deepseek_spend_age_s"] = int64(now.Sub(doc.TS) / time.Second)
}

// moneyAddTokens fills one column per model from the usage scan's records.
func moneyAddTokens(env Env, cols map[string]any, gaps *moneyGaps) {
	// An Env built for sources that never list a directory has no seam to
	// read the usage files with; there is no scan to report, and the token
	// columns stay out rather than panicking.
	if env.ReadDir == nil {
		return
	}
	dir := filepath.Join(constants.TownRuntimePath(env.Town), moneyUsageDir)
	entries, err := env.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		gaps.readFailures++
		return
	}
	now := env.Now()
	cutoff := now.Add(-time.Hour)
	tokens := map[string]int64{}
	named := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir || !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		data, err := env.ReadFile(filepath.Join(dir, entry.Name))
		if err != nil {
			// A record that went away since the listing is not a gap: its
			// data is no longer in the directory to count. A read that failed
			// is, because the sums are missing that record's spend.
			if !errors.Is(err, fs.ErrNotExist) {
				gaps.unreadableRecords++
			}
			continue
		}
		var rec dashboard.UsageRecord
		if json.Unmarshal(data, &rec) != nil {
			gaps.unparsedInputs++
			continue
		}
		inWindow := rec.Complete && !rec.End.IsZero() &&
			!rec.End.Before(cutoff) && !rec.End.After(now)
		for model, m := range rec.Models {
			column := moneyTokenColumn(model)
			if column == "" {
				continue
			}
			named[column] = true
			if inWindow {
				tokens[column] += m.Input + m.Output + m.CacheCreation
			}
		}
	}
	for column := range named {
		key := "tokens_" + column + "_1h"
		if n, ok := tokens[column]; ok {
			cols[key] = n
		} else {
			cols[key] = nil
		}
	}
}

// moneyTokenColumn is a model's name as a column: letters, digits, '-' and '_'
// kept, every other character one '_', so two identifiers never need quoting
// and a model that moves to a new slash-separated name gets a new column.
func moneyTokenColumn(model string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, model)
}
