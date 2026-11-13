package usageattrib

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// The report folds the work and om records a scan wrote into one row per role, model, bead or day.
// It reads no transcript: each record is the scan's account of one run, and the report adds those
// accounts up. A Claude model is reported in uncached tokens (input, cache creation and output). A
// DeepSeek model is reported as its estimated cost, since its cost is the estimate and not a count.

// The groupings a report can take.
const (
	ByRole  = "role"
	ByModel = "model"
	ByBead  = "bead"
	ByDay   = "day"
)

// ReportOptions is what one report covers.
type ReportOptions struct {
	// From and To bound the window. A record counts when its end is in [From, To], and an
	// unmatched session counts when its last write is.
	From, To time.Time
	// By is one of ByRole, ByModel, ByBead or ByDay.
	By string
	// Bead, when set, keeps only that bead's records.
	Bead string
	// Over, when above zero, keeps only the beads whose complete work record ran longer than it.
	// Every record of a kept bead counts.
	Over time.Duration
	// Zone is the zone a day is read in. A nil Zone reads the day in UTC.
	Zone *time.Location
}

// ReportRow is one group's numbers.
type ReportRow struct {
	Key   string `json:"key"`
	Calls int64  `json:"calls"`
	// ClaudeUncached is the uncached tokens of the models DeepSeek does not price.
	ClaudeUncached int64 `json:"claude_uncached"`
	// DeepSeekEstUSD is the estimated cost of the models DeepSeek prices. It is an estimate.
	DeepSeekEstUSD float64 `json:"deepseek_est_usd"`
	// SubShare is the subagents' uncached tokens over all the group's uncached tokens. It is nil
	// under ByModel, since a record's subagent tokens are not split by model.
	SubShare *float64 `json:"sub_share"`
}

// Report is the rows of one report and the counts that qualify them. The counts cover the whole
// window, whatever the bead and over filters keep.
type Report struct {
	By   string      `json:"by"`
	From time.Time   `json:"from"`
	To   time.Time   `json:"to"`
	Rows []ReportRow `json:"rows"`
	// Unmatched counts the sessions the scan matched to no bead, last written in the window.
	Unmatched int `json:"unmatched"`
	// Incomplete counts the records in the window that are marked incomplete. They are left out
	// of the rows, since a run still in flight would understate the window.
	Incomplete int `json:"incomplete"`
	// Skipped counts the records with no end time. No window holds one, so they are left out.
	Skipped int `json:"skipped"`
}

// recordsSubdir is the directory under the usage root that holds one record file per bead and kind.
const recordsSubdir = "beads"

// ReadRecords decodes every record file under usageDir. A file that cannot be read or decoded is an
// error that names it: a report that dropped it would understate the window, so no record is read as
// absent. A missing records directory is an error wrapping fs.ErrNotExist.
func ReadRecords(usageDir string) ([]dashboard.UsageRecord, error) {
	dir := filepath.Join(usageDir, recordsSubdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("usageattrib: reading the records directory: %w", err)
	}
	var recs []dashboard.UsageRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("usageattrib: reading %s: %w", path, err)
		}
		var rec dashboard.UsageRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("usageattrib: %s is not a usage record: %w", path, err)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// ReadUnmatched returns the last write of each session the scan matched to no bead, one per line of
// UnmatchedFile. A scan always writes that file, so a missing file is an error: a scan that did not
// finish, not a day with no unmatched session.
func ReadUnmatched(usageDir string) ([]time.Time, error) {
	path := filepath.Join(usageDir, UnmatchedFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("usageattrib: reading %s: %w", path, err)
	}
	var writes []time.Time
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var l unmatchedLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			return nil, fmt.Errorf("usageattrib: %s has a line that is not an unmatched session: %w", path, err)
		}
		writes = append(writes, l.LastWrite)
	}
	return writes, nil
}

// reportGroup is one row under construction. sub and uncached are the subagent and all uncached
// tokens behind SubShare.
type reportGroup struct {
	row      ReportRow
	sub      int64
	uncached int64
}

// BuildReport folds recs, and the last writes of the unmatched sessions, into the report o asks for.
func BuildReport(recs []dashboard.UsageRecord, unmatched []time.Time, o ReportOptions) (Report, error) {
	switch o.By {
	case ByRole, ByModel, ByBead, ByDay:
	default:
		return Report{}, fmt.Errorf("usageattrib: report: grouping %q is not role, model, bead or day", o.By)
	}
	zone := o.Zone
	if zone == nil {
		zone = time.UTC
	}
	rep := Report{By: o.By, From: o.From, To: o.To, Rows: []ReportRow{}}
	for _, t := range unmatched {
		if inWindow(t, o.From, o.To) {
			rep.Unmatched++
		}
	}
	var inWin []dashboard.UsageRecord
	for _, r := range recs {
		if r.End.IsZero() {
			rep.Skipped++
			continue
		}
		if !inWindow(r.End, o.From, o.To) {
			continue
		}
		if !r.Complete {
			rep.Incomplete++
			continue
		}
		inWin = append(inWin, r)
	}

	var over map[string]bool
	if o.Over > 0 {
		over = beadsOver(inWin, o.Over)
	}
	groups := map[string]*reportGroup{}
	for _, r := range inWin {
		if o.Bead != "" && r.Bead != o.Bead {
			continue
		}
		if o.Over > 0 && !over[r.Bead] {
			continue
		}
		if o.By == ByModel {
			for _, model := range sortedModels(r) {
				m := r.Models[model]
				g := groupOf(groups, model)
				g.row.Calls += m.Calls
				addModel(&g.row, model, m)
			}
			continue
		}
		g := groupOf(groups, keyOf(r, o.By, zone))
		for _, model := range sortedModels(r) {
			m := r.Models[model]
			g.row.Calls += m.Calls
			addModel(&g.row, model, m)
			g.uncached += uncachedOf(m)
		}
		g.sub += r.Subagents.Uncached
	}

	for key, g := range groups {
		row := g.row
		row.Key = key
		if o.By != ByModel {
			share := 0.0
			if g.uncached != 0 {
				share = float64(g.sub) / float64(g.uncached)
			}
			row.SubShare = &share
		}
		rep.Rows = append(rep.Rows, row)
	}
	sort.Slice(rep.Rows, func(i, j int) bool { return rep.Rows[i].Key < rep.Rows[j].Key })
	return rep, nil
}

// keyOf is the row a record falls in under the grouping by.
func keyOf(r dashboard.UsageRecord, by string, zone *time.Location) string {
	switch by {
	case ByBead:
		return r.Bead
	case ByDay:
		return r.End.In(zone).Format("2006-01-02")
	}
	return r.Class
}

// beadsOver is the beads whose complete work record ran longer than limit. A bead with no complete
// work record in the window has no known wall time, so it is not over.
func beadsOver(recs []dashboard.UsageRecord, limit time.Duration) map[string]bool {
	over := map[string]bool{}
	for _, r := range recs {
		if r.Kind == dashboard.UsageKindWork && time.Duration(r.WallS)*time.Second > limit {
			over[r.Bead] = true
		}
	}
	return over
}

func groupOf(groups map[string]*reportGroup, key string) *reportGroup {
	g, ok := groups[key]
	if !ok {
		g = &reportGroup{}
		groups[key] = g
	}
	return g
}

// addModel adds one model's share to a row: its estimated cost when DeepSeek prices the model, its
// uncached tokens otherwise.
func addModel(row *ReportRow, model string, m dashboard.UsageModel) {
	if _, priced := priceOf(model); priced {
		row.DeepSeekEstUSD += m.EstCostUSD
		return
	}
	row.ClaudeUncached += uncachedOf(m)
}

// uncachedOf is the tokens a model paid full price for: input, cache creation and output.
func uncachedOf(m dashboard.UsageModel) int64 {
	return m.Input + m.Output + m.CacheCreation
}

// sortedModels is a record's model names in order, so float sums add in the same order every run.
func sortedModels(r dashboard.UsageRecord) []string {
	names := make([]string, 0, len(r.Models))
	for name := range r.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// inWindow reports whether t falls in [from, to].
func inWindow(t, from, to time.Time) bool {
	return !t.Before(from) && !t.After(to)
}
