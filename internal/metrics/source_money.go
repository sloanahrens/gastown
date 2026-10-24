package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
//	tokens_<model>_1h  uncached tokens on that model in the last hour
//
// A file that is not there leaves its columns nil: no reading is not a reading
// of zero, and a town without a Claude plan or a usage scan still records its
// other spending. A read that fails for any other reason fails Collect, so the
// group is null and the error is in the record. A line of the series that will
// not parse, and a spend document that will not decode or carries no
// timestamp, are skipped rather than failing the group. A usage record that
// cannot be read or decoded is skipped too: it is one run of a directory of
// them, not the group's input.
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
	if err := moneyAddClaudePlan(env, cols); err != nil {
		return nil, err
	}
	if err := moneyAddDeepSeek(env, cols); err != nil {
		return nil, err
	}
	if err := moneyAddTokens(env, cols); err != nil {
		return nil, err
	}
	return cols, nil
}

// moneyAddClaudePlan fills the plan columns from the status line's tail.
func moneyAddClaudePlan(env Env, cols map[string]any) error {
	data, err := env.Tail(filepath.Join(constants.TownRuntimePath(env.Town), moneyClaudePlanFile), sourceLogTailBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", moneyClaudePlanFile, err)
	}
	var samples []dashboard.ClaudePlanSample
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var sample dashboard.ClaudePlanSample
		// A reading with no instant cannot be placed against any other, so a
		// line without one is as useless as a line that will not parse.
		if json.Unmarshal(line, &sample) != nil || sample.T <= 0 {
			continue
		}
		samples = append(samples, sample)
	}
	if len(samples) == 0 {
		return nil
	}
	plan := dashboard.ComputeClaudePlan(samples, env.Now(), dashboard.ClaudePlanDefaultUSD)
	cols["claude_plan_weekly_pct"] = plan.WeeklyPct
	cols["claude_plan_five_hour_pct"] = plan.FiveHourPct
	cols["claude_plan_weekly_pace_per_day_1h"] = plan.PacePerDay1h
	cols["claude_plan_stale"] = plan.Stale
	return nil
}

// moneyAddDeepSeek fills the DeepSeek columns from the watch's reading.
func moneyAddDeepSeek(env Env, cols map[string]any) error {
	path := filepath.Join(constants.TownRuntimePath(env.Town), moneySpendFile)
	data, err := env.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", moneySpendFile, err)
	}
	var doc struct {
		TS      time.Time `json:"ts"`
		PerHour *float64  `json:"per_hour"`
		Balance *float64  `json:"balance"`
	}
	// A document with no timestamp is a reading nobody measured at a time, so
	// it cannot be aged and is no reading at all.
	if err := json.Unmarshal(data, &doc); err != nil || doc.TS.IsZero() {
		return nil
	}
	now := env.Now()
	if doc.PerHour != nil {
		cols["deepseek_per_hour"] = *doc.PerHour
	}
	if doc.Balance != nil {
		cols["deepseek_balance"] = *doc.Balance
	}
	cols["deepseek_spend_age_s"] = int64(now.Sub(doc.TS) / time.Second)
	return nil
}

// moneyAddTokens fills one column per model from the usage scan's records.
func moneyAddTokens(env Env, cols map[string]any) error {
	dir := filepath.Join(constants.TownRuntimePath(env.Town), moneyUsageDir)
	entries, err := env.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing %s: %w", moneyUsageDir, err)
	}
	now := env.Now()
	cutoff := now.Add(-time.Hour)
	tokens := map[string]int64{}
	for _, entry := range entries {
		if entry.IsDir || !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		data, err := env.ReadFile(filepath.Join(dir, entry.Name))
		if err != nil {
			continue
		}
		var rec dashboard.UsageRecord
		if json.Unmarshal(data, &rec) != nil || rec.End.IsZero() || !rec.Complete {
			continue
		}
		if rec.End.Before(cutoff) || rec.End.After(now) {
			continue
		}
		for model, m := range rec.Models {
			column := moneyTokenColumn(model)
			if column == "" {
				continue
			}
			tokens[column] += m.Input + m.Output + m.CacheCreation
		}
	}
	for column, n := range tokens {
		cols["tokens_"+column+"_1h"] = n
	}
	return nil
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
