// Package tokensrc turns the per-bead usage records into the month's token
// counts per workspace, the adapter internal/metering/export calls the usage
// records reader for plan rows M3 and M8. Its caller is the invoice command
// bead filed with this one.
//
// It reads no record of its own and keeps no clock: the tree comes in as an
// io/fs.FS and the month as an export.CalendarMonth, so the counts are a pure
// function of the files the tree holds.
package tokensrc

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/metering/export"
)

// usageDir is the directory under the usage tree that holds one record file per
// bead and kind.
const usageDir = "beads"

// Result is the month's token counts: one row per workspace, with what the read
// excluded kept beside them so a total that fell short says why.
type Result struct {
	// Workspaces is one row per workspace the month's records name, sorted by
	// workspace id.
	Workspaces []export.TokenCount
	// Unattributed is the token total of the records that name no workspace:
	// the scan wrote them before a record carried one, so no workspace may
	// claim their spend.
	Unattributed export.Tokens
	// OutOfMonth counts the records no month row holds: those whose end is
	// zero or falls outside the month, which contribute no tokens anywhere.
	OutOfMonth int
	// Unreadable names the files under the beads directory that could not be
	// read or decoded, so a short total names the files it is missing.
	Unreadable []string
}

// Tokens reads every *.json record under the usage tree's beads directory and
// totals the records whose end falls in month (UTC), one export.TokenCount per
// workspace, sorted by workspace id.
//
// A record with no workspace is excluded from the rows and totalled in
// Unattributed. A record whose end is zero or outside the month is counted in
// OutOfMonth before its workspace is read, since it belongs to no month row at
// all. A file that fails to read or decode is named in Unreadable and the read
// continues; a directory that cannot be listed, or a sum that overflows int64,
// returns an error.
func Tokens(fsys fs.FS, month export.CalendarMonth) (Result, error) {
	entries, err := fs.ReadDir(fsys, usageDir)
	if err != nil {
		return Result{}, fmt.Errorf("tokensrc: reading the usage records under %s: %w", usageDir, err)
	}
	start, end := month.Start(), month.End()
	sums := make(map[string]*export.Tokens)
	var res Result

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := entry.Name()
		data, err := fs.ReadFile(fsys, path.Join(usageDir, name))
		if err != nil {
			res.Unreadable = append(res.Unreadable, name)
			continue
		}
		var rec dashboard.UsageRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			res.Unreadable = append(res.Unreadable, name)
			continue
		}
		if rec.End.IsZero() || rec.End.Before(start) || !rec.End.Before(end) {
			res.OutOfMonth++
			continue
		}
		counts := export.Tokens{
			Input:         rec.Totals.Input,
			Output:        rec.Totals.Output,
			CacheCreation: rec.Totals.CacheCreation,
			CacheRead:     rec.Totals.CacheRead,
		}
		if rec.Workspace == "" {
			if err := addCounts(&res.Unattributed, counts, "the unattributed records"); err != nil {
				return Result{}, err
			}
			continue
		}
		sum := sums[rec.Workspace]
		if sum == nil {
			sum = &export.Tokens{}
			sums[rec.Workspace] = sum
		}
		if err := addCounts(sum, counts, fmt.Sprintf("workspace %q", rec.Workspace)); err != nil {
			return Result{}, err
		}
	}

	res.Workspaces = make([]export.TokenCount, 0, len(sums))
	for ws, sum := range sums {
		res.Workspaces = append(res.Workspaces, export.TokenCount{
			Workspace:     ws,
			Input:         sum.Input,
			Output:        sum.Output,
			CacheCreation: sum.CacheCreation,
			CacheRead:     sum.CacheRead,
		})
	}
	slices.SortFunc(res.Workspaces, func(a, b export.TokenCount) int {
		return strings.Compare(a.Workspace, b.Workspace)
	})
	return res, nil
}

// addCounts adds src into dst field by field, refusing a sum outside int64 so
// an absurd record fails the read rather than wrapping to a small bill.
func addCounts(dst *export.Tokens, src export.Tokens, where string) error {
	fields := []struct {
		name string
		dst  *int64
		val  int64
	}{
		{"input", &dst.Input, src.Input},
		{"output", &dst.Output, src.Output},
		{"cache creation", &dst.CacheCreation, src.CacheCreation},
		{"cache read", &dst.CacheRead, src.CacheRead},
	}
	for _, f := range fields {
		sum, ok := addInt64(*f.dst, f.val)
		if !ok {
			return fmt.Errorf("tokensrc: %s tokens for %s overflow int64", f.name, where)
		}
		*f.dst = sum
	}
	return nil
}

// addInt64 returns a+b and whether the sum fits in an int64.
func addInt64(a, b int64) (int64, bool) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, false
	}
	return s, true
}
