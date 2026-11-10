package usageattrib

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// SessionUsage is one session's usage as the caller read it from its transcripts.
type SessionUsage struct {
	ID string
	// Main is the session's own transcript. Subagent holds the messages of every
	// subagents/agent-*.jsonl file the session started.
	Main     []Message
	Subagent []Message
	// Complete is false when a transcript was missing or unreadable, or had a
	// line that did not parse: its totals may understate the run.
	Complete bool
}

// Facts is what the caller knows about a run: which bead it served and when it ran.
type Facts struct {
	Bead      string
	Kind      string // dashboard.UsageKindWork or dashboard.UsageKindOM
	Class     string
	Rig       string
	Seat      *string
	Workspace string
	Start     time.Time
	End       time.Time
	// Complete is true when the run finished: its done event, or the review's return.
	Complete bool
}

// storedRecord is a record file as Write keeps it: the usage record plus the ids of the
// sessions already counted in it. The dashboard and tokensrc decoders ignore the extra field.
type storedRecord struct {
	dashboard.UsageRecord
	Sessions []string `json:"sessions"`
}

// Build sums sessions into the usage record for one run. A session counts once however many
// times it is passed; the record is complete only when the run finished, it had at least one
// session, and every session was read whole.
func Build(f Facts, sessions []SessionUsage) dashboard.UsageRecord {
	rec := dashboard.UsageRecord{
		Bead:      f.Bead,
		Kind:      f.Kind,
		Class:     f.Class,
		Rig:       f.Rig,
		Workspace: f.Workspace,
		Seat:      f.Seat,
		Start:     f.Start,
		End:       f.End,
		Models:    map[string]dashboard.UsageModel{},
	}
	complete := f.Complete && len(sessions) > 0
	seen := map[string]bool{}
	var subCalls, subUncached int64
	for _, s := range sessions {
		if s.ID == "" {
			complete = false
			continue
		}
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		if !s.Complete {
			complete = false
		}
		var stamps []time.Time
		add := func(m Message, sub bool) {
			slot := rec.Models[m.Model]
			slot.Calls++
			slot.Input += m.Input
			slot.CacheCreation += m.CacheCreation
			slot.CacheRead += m.CacheRead
			slot.Output += m.Output
			rec.Models[m.Model] = slot
			if sub {
				subCalls++
				subUncached += m.Input + m.CacheCreation + m.Output
			}
			// A timestamp that does not parse makes the record incomplete and its cost unknown,
			// but the tokens were still read, so they stay counted.
			t, err := ParseTS(m.TS)
			if err != nil {
				complete = false
				return
			}
			stamps = append(stamps, t)
			cost, priced, err := EstCost(m.Model, m.Input, m.CacheCreation, m.CacheRead, m.Output, m.TS)
			if err != nil {
				complete = false
				return
			}
			if priced {
				slot = rec.Models[m.Model]
				slot.EstCostUSD += cost
				rec.Models[m.Model] = slot
				rec.EstCostUSD += cost
			}
		}
		for _, m := range s.Main {
			add(m, false)
		}
		for _, m := range s.Subagent {
			add(m, true)
		}
		// Wall time is the span of each session's own timestamps, summed over sessions, so
		// a second session adds its own minutes rather than the gap between the two.
		if len(stamps) > 0 {
			lo, hi := stamps[0], stamps[0]
			for _, t := range stamps[1:] {
				if t.Before(lo) {
					lo = t
				}
				if t.After(hi) {
					hi = t
				}
			}
			rec.WallS += int64(math.RoundToEven(hi.Sub(lo).Seconds()))
		}
	}
	rec.Totals = totalsOf(rec.Models)
	rec.Subagents = dashboard.UsageSubagents{
		Calls:    subCalls,
		Uncached: subUncached,
		Share:    subagentShare(subUncached, rec.Totals.Uncached),
	}
	rec.Complete = complete
	return rec
}

// totalsOf sums the per-model rows. Uncached is input, cache creation and output, the tokens
// the run paid full price for.
func totalsOf(models map[string]dashboard.UsageModel) dashboard.UsageTotals {
	var t dashboard.UsageTotals
	for _, m := range models {
		t.Calls += m.Calls
		t.Input += m.Input
		t.Output += m.Output
		t.CacheCreation += m.CacheCreation
		t.CacheRead += m.CacheRead
	}
	t.Uncached = t.Input + t.CacheCreation + t.Output
	return t
}

// subagentShare is the fraction of the run's uncached tokens spent in subagents, zero for a
// run that spent none.
func subagentShare(sub, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(sub) / float64(total)
}

// unsafeFileChars matches the characters usage-attrib's safe() replaces in a bead name (gt-6qc3nw).
var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// recordPath is <usageDir>/beads/<bead>.<kind>.json, with the bead cleaned so no bead name
// can reach outside the beads directory.
func recordPath(usageDir, bead, kind string) string {
	return filepath.Join(usageDir, "beads", unsafeFileChars.ReplaceAllString(bead, "_")+"."+kind+".json")
}

// writeMu serialises Write within the process, the only writer of these files.
var writeMu sync.Mutex

// Write merges rec into its bead's record file under usageDir and adds sessionIDs to the
// file's counted sessions. rec must be built from exactly sessionIDs. Sessions already in
// the file make the write a no-op, so writing the same session twice leaves the file as it
// was. A record that mixes counted and new sessions is refused, since its totals cannot be
// split.
func Write(usageDir string, rec dashboard.UsageRecord, sessionIDs []string) error {
	if rec.Bead == "" {
		return errors.New("usageattrib: record has no bead")
	}
	// A record with no workspace would total as unattributed spend, which no workspace may claim.
	if rec.Workspace == "" {
		return fmt.Errorf("usageattrib: %s: record has no workspace", rec.Bead)
	}
	if rec.Kind != dashboard.UsageKindWork && rec.Kind != dashboard.UsageKindOM {
		return fmt.Errorf("usageattrib: %s: kind %q is neither %q nor %q", rec.Bead, rec.Kind, dashboard.UsageKindWork, dashboard.UsageKindOM)
	}
	var ids []string
	for _, id := range sessionIDs {
		if id == "" {
			return fmt.Errorf("usageattrib: %s: a session has no id", rec.Bead)
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return fmt.Errorf("usageattrib: %s: no sessions to count", rec.Bead)
	}

	// Read, merge and rename must not interleave: two runs for one bead ending together would
	// otherwise each merge into the old file and one run's sessions would be lost.
	writeMu.Lock()
	defer writeMu.Unlock()

	path := recordPath(usageDir, rec.Bead, rec.Kind)
	merged, counted := rec, ids
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The first write for this bead and kind.
	case err != nil:
		return fmt.Errorf("usageattrib: reading %s: %w", path, err)
	default:
		var cur storedRecord
		if err := json.Unmarshal(data, &cur); err != nil {
			return fmt.Errorf("usageattrib: %s is not a record this package wrote, left as it is: %w", path, err)
		}
		if cur.Sessions == nil {
			return fmt.Errorf("usageattrib: %s has no sessions list, so its sessions cannot be told apart: left as it is", path)
		}
		if err := sameRun(cur.UsageRecord, rec); err != nil {
			return err
		}
		var fresh []string
		for _, id := range ids {
			if !slices.Contains(cur.Sessions, id) {
				fresh = append(fresh, id)
			}
		}
		if len(fresh) == 0 {
			return nil
		}
		if len(fresh) != len(ids) {
			return fmt.Errorf("usageattrib: %s: %d of %d sessions are already counted; build the record from the new sessions only",
				rec.Bead, len(ids)-len(fresh), len(ids))
		}
		merged = mergeRecords(cur.UsageRecord, rec)
		counted = append(slices.Clone(cur.Sessions), ids...)
	}

	out, err := json.MarshalIndent(storedRecord{UsageRecord: merged, Sessions: counted}, "", " ")
	if err != nil {
		return fmt.Errorf("usageattrib: encoding the record for %s: %w", rec.Bead, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("usageattrib: creating the beads directory: %w", err)
	}
	return writeFileAtomic(path, append(out, '\n'))
}

// sameRun refuses a merge into a record of a different run. Every field here is set by the
// caller, so a mismatch means the caller passed the wrong bead, kind or workspace.
func sameRun(cur, rec dashboard.UsageRecord) error {
	fields := []struct{ name, have, want string }{
		{"bead", cur.Bead, rec.Bead},
		{"kind", cur.Kind, rec.Kind},
		{"class", cur.Class, rec.Class},
		{"rig", cur.Rig, rec.Rig},
		{"workspace", cur.Workspace, rec.Workspace},
	}
	for _, f := range fields {
		if f.have != f.want {
			return fmt.Errorf("usageattrib: %s: the record has %s %q, the new sessions have %q", rec.Bead, f.name, f.have, f.want)
		}
	}
	return nil
}

// mergeRecords adds the totals of b into a. Seat is kept only when both runs had the same one,
// so a record that spans several seats names none.
func mergeRecords(a, b dashboard.UsageRecord) dashboard.UsageRecord {
	m := a
	m.Complete = a.Complete && b.Complete
	m.Start = earliest(a.Start, b.Start)
	m.End = latest(a.End, b.End)
	m.WallS += b.WallS
	m.EstCostUSD += b.EstCostUSD
	m.Models = maps.Clone(a.Models)
	for name, row := range b.Models {
		s := m.Models[name]
		s.Calls += row.Calls
		s.Input += row.Input
		s.Output += row.Output
		s.CacheCreation += row.CacheCreation
		s.CacheRead += row.CacheRead
		s.EstCostUSD += row.EstCostUSD
		m.Models[name] = s
	}
	m.Totals = totalsOf(m.Models)
	m.Subagents = dashboard.UsageSubagents{
		Calls:    a.Subagents.Calls + b.Subagents.Calls,
		Uncached: a.Subagents.Uncached + b.Subagents.Uncached,
	}
	m.Subagents.Share = subagentShare(m.Subagents.Uncached, m.Totals.Uncached)
	m.Activity = dashboard.UsageActivity{
		ToolCalls:      a.Activity.ToolCalls + b.Activity.ToolCalls,
		Edits:          a.Activity.Edits + b.Activity.Edits,
		TestRuns:       a.Activity.TestRuns + b.Activity.TestRuns,
		FailedTestRuns: a.Activity.FailedTestRuns + b.Activity.FailedTestRuns,
	}
	if !sameSeat(a.Seat, b.Seat) {
		m.Seat = nil
	}
	return m
}

func sameSeat(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// earliest and latest treat a zero time as unset, so a record with no bound takes the other's.
func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case b.Before(a):
		return b
	}
	return a
}

func latest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case b.After(a):
		return b
	}
	return a
}

// writeFileAtomic replaces path with data through a temporary file in the same directory, so
// a reader sees the old record or the new one, never a partial file.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("usageattrib: writing %s: %w", path, err)
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		// CreateTemp makes the file 0600, which a reader running as another user could not open.
		err = f.Chmod(0o644)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		err = errors.Join(err, os.Remove(tmp))
		return fmt.Errorf("usageattrib: writing %s: %w", path, err)
	}
	return nil
}

// WorkspaceOf is the workspace a bead belongs to: the rig prefix (the text before its first
// hyphen) as settings/workspaces.json under townRoot maps it, else the file's default, else
// customer-zero when the file is absent. A malformed file is an error.
func WorkspaceOf(townRoot, bead string) (string, error) {
	m, err := workspaceid.Load(townRoot)
	if err != nil {
		return "", fmt.Errorf("usageattrib: workspace of %s: %w", bead, err)
	}
	prefix, _, _ := strings.Cut(bead, "-")
	ws, err := m.For(prefix)
	if err != nil {
		return "", fmt.Errorf("usageattrib: workspace of %s: %w", bead, err)
	}
	return ws, nil
}
