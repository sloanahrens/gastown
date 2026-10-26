package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemonlog"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The queue group: how much work the town holds, and what the spec dispatcher
// did with it in the last hour.
//
// Each bead column is one bd read per registered rig, summed. The rigs come
// from mayor/rigs.json, or from the registry section of mayor/town.json on the
// two-file layout (internal/config/layout.go); the flags are the ones
// internal/cmd/spec.go's specReadyOptions (through
// beads.Client.ReadyFiltered) and internal/beads' listIssues send, checked
// against `bd ready --help` and `bd list --help`:
//
//	ready            bd -C <town>/<rig> ready --json --limit 0
//	dispatchable     bd -C <town>/<rig> ready --json --unassigned
//	                   --exclude-label <constants.NonDispatchableBeadLabels,
//	                                    specdispatch.ReadyQueryLabels()>
//	                   --exclude-type <specdispatch.NonWorkBeadTypes()> --limit 0
//	waiting_to_land  bd -C <town>/<rig> list --json --label gt:ready-to-land --limit 0
//	blocked          bd -C <town>/<rig> blocked --json
//
// blocked counts the beads waiting on an open dependency, as bd blocked lists
// them, and skips the ephemeral rows (the work molecules' wisp steps, which are
// blocked by design and come and go with every dispatch). It does not count the
// beads whose status field says "blocked": the town never sets that status, so
// that count was always zero (gt-5jve7).
//
// dispatchable is the board the dispatcher itself reads, so it counts the
// beads its per-bead lint will judge, not only the ones it will sling: a bead
// the shape lint or a hold refuses is still on the board.
//
// The dispatcher columns are read from the tail of <town>/daemon/daemon.log
// (daemonlog.FileName), the last queueLogMaxLines lines of a queueLogTailBytes
// read through Env.Tail:
//
//	dispatch_ticks_1h  "spec_dispatch: tick: ..." lines stamped in the last hour
//	dispatched_1h      those lines' "N dispatched", summed
//	last_tick_secs     the newest "spec_dispatch: tick took <d>" line stamped in
//	                   the last hour, in seconds (nil when there is none)
//	seat_live_<agent>  live sessions on that agent's seat, from the roster of the
//	seat_cap_<agent>   newest tick line stamped in the last hour (specdispatch's
//	                   Budget.Picture); left out when no tick is that recent
//	dispatch_newest_tick_age_s  the age of the newest tick line in the tail
//	                   whatever its age, so a dispatcher that went quiet reads
//	                   as a number and not as current values
//	dispatch_unparsed_lines  tick lines the tail holds with no timestamp the
//	                   parser could place (no stamped line before them): they
//	                   are not counted in any column above, and counted here
//
// A read that failed leaves its columns nil rather than zero: a queue that
// could not be read is not an empty queue, and one rig failing nil only the
// columns it was read for keeps the rest of the group. The seats' town total
// ("total L/C", which Budget.Picture appends only under a configured cap) is
// not a column: most towns do not configure one, so it would read null for
// everyone but the towns that do.
type queueSource struct{}

// queueBdTimeout is one bd read's deadline. A column reads its rigs
// queueRigParallel at a time, so nine rigs cost at most three deadlines (15 s)
// for a column, and the group's own deadline (SourceTimeout) caps the whole
// group regardless. A bd hung on a dead Dolt server costs its column rather
// than the whole group.
const queueBdTimeout = 5 * time.Second

// queueRigParallel is how many rigs one column reads at once. The four columns
// run side by side, so at most four times this many bd processes are alive.
const queueRigParallel = 3

// queueLogMaxLines is how many of the log's last lines the parse keeps.
const queueLogMaxLines = 2000

// queueLogTailBytes is the most Env.Tail reads: an hour of the daemon log,
// which holds the last queueLogMaxLines lines of its usual line length (a tick
// line naming every skipped bead passes a kilobyte). Env.Tail takes bytes, not
// lines, so the line bound is applied to what was read.
const queueLogTailBytes = daemonLogHourBytes

// queueTickRe matches the dispatcher's tick line (internal/daemon's
// specDispatchTickLine) after its "spec_dispatch: " prefix is trimmed. The
// roster can hold commas and parenthesised reasons, so it runs non-greedy to
// the first "N dispatched".
var queueTickRe = regexp.MustCompile(`^tick: \d+ candidate\(s\), roster (.+?), (\d+) dispatched,`)

// queueTookRe matches the dispatcher's tick duration line.
var queueTookRe = regexp.MustCompile(`^tick took (\S+) \(`)

// queueSeatRe matches one seat at the start of a roster segment:
// "agent live/cap", followed by any parenthesised reason.
var queueSeatRe = regexp.MustCompile(`^([^\s]+) (\d+)/(\d+)`)

// queueBeadColumn is one bead count and the bd read in one rig that fills it.
type queueBeadColumn struct {
	column string
	args   []string
	// skipEphemeral leaves out the rows bd marks ephemeral (infrastructure
	// wisps, not work).
	skipEphemeral bool
}

// queueBeadColumns is the group's ordering of bead reads. The exclusion lists
// are the dispatcher's own, so a change to what it considers dispatchable
// moves this column with it.
func queueBeadColumns() []queueBeadColumn {
	labels := append(append([]string(nil), constants.NonDispatchableBeadLabels...), specdispatch.ReadyQueryLabels()...)
	return []queueBeadColumn{
		{
			column: "ready",
			args:   []string{"ready", "--json", "--limit", "0"},
		},
		{
			column: "dispatchable",
			args: []string{
				"ready", "--json", "--unassigned",
				"--exclude-label", strings.Join(labels, ","),
				"--exclude-type", strings.Join(specdispatch.NonWorkBeadTypes(), ","),
				"--limit", "0",
			},
		},
		{
			column: "waiting_to_land",
			args:   []string{"list", "--json", "--label", "gt:ready-to-land", "--limit", "0"},
		},
		{
			column:        "blocked",
			args:          []string{"blocked", "--json"},
			skipEphemeral: true,
		},
	}
}

func (queueSource) Group() string { return "queue" }

func (queueSource) Collect(ctx context.Context, env Env) (map[string]any, error) {
	rigs, err := queueRigs(env)
	if err != nil {
		return nil, err
	}
	cols := make(map[string]any, 8)
	queueAddLogColumns(env, cols)
	columns := queueBeadColumns()
	done := make(chan queueColumnResult, len(columns))
	for _, c := range columns {
		go func(c queueBeadColumn) {
			done <- queueColumnResult{column: c.column, value: queueSumBeads(ctx, env, rigs, c.args, c.skipEphemeral)}
		}(c)
	}
	for range columns {
		r := <-done
		cols[r.column] = r.value
	}
	return cols, nil
}

// queueColumnResult is one bead column's sum, nil when a rig could not be read.
type queueColumnResult struct {
	column string
	value  any
}

// queueRigs lists the registered rig names, sorted. An unreadable registry is
// an error: summed over no rigs, every bead column would read as an empty town.
func queueRigs(env Env) ([]string, error) {
	data, err := env.ReadFile(filepath.Join(env.Town, "mayor", "rigs.json"))
	if err != nil {
		town, townErr := env.ReadFile(filepath.Join(env.Town, config.MachineConfigFile))
		if townErr != nil {
			return nil, fmt.Errorf("queue: reading the rig registry: %w", err)
		}
		var tc config.TownConfig
		if err := json.Unmarshal(town, &tc); err != nil {
			return nil, fmt.Errorf("queue: parsing %s: %w", config.MachineConfigFile, err)
		}
		if tc.Registry == nil {
			return nil, fmt.Errorf("queue: %s carries no rig registry", config.MachineConfigFile)
		}
		return queueRigNames(tc.Registry), nil
	}
	var rc config.RigsConfig
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, fmt.Errorf("queue: parsing mayor/rigs.json: %w", err)
	}
	return queueRigNames(&rc), nil
}

// queueRigNames sorts the registry's names so a snapshot reads the same on
// every run.
func queueRigNames(rc *config.RigsConfig) []string {
	names := make([]string, 0, len(rc.Rigs))
	for name := range rc.Rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// queueSumBeads runs one column's read in every rig, queueRigParallel at a
// time, and sums the rows. Any rig that fails leaves the column nil: a partial
// sum would read as a queue that shrank.
func queueSumBeads(ctx context.Context, env Env, rigs []string, args []string, skipEphemeral bool) any {
	type result struct {
		n   int
		err error
	}
	results := make([]result, len(rigs))
	sem := make(chan struct{}, queueRigParallel)
	var wg sync.WaitGroup
	for i, rig := range rigs {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, rig string) {
			defer func() { <-sem; wg.Done() }()
			n, err := queueCount(ctx, env, filepath.Join(env.Town, rig), args, skipEphemeral)
			results[i] = result{n: n, err: err}
		}(i, rig)
	}
	wg.Wait()
	sum := 0
	for _, r := range results {
		if r.err != nil {
			return nil
		}
		sum += r.n
	}
	return sum
}

// queueCount runs one bd read in one rig's store under its own deadline and
// returns how many rows it printed.
func queueCount(ctx context.Context, env Env, rigPath string, args []string, skipEphemeral bool) (int, error) {
	callCtx, cancel := context.WithTimeout(ctx, queueBdTimeout)
	defer cancel()
	argv := append([]string{"bd", "-C", rigPath}, args...)
	out, err := env.Run(callCtx, argv)
	if err != nil {
		return 0, err
	}
	return queueCountIssues(out, skipEphemeral)
}

// queueCountIssues counts the rows of a bd --json answer, leaving out the rows
// bd marks ephemeral when skipEphemeral is set. bd prints a bare array; the
// envelope shapes are accepted too so a bd that grows one does not nil the
// column.
func queueCountIssues(out []byte, skipEphemeral bool) (int, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return 0, nil
	}
	if trimmed[0] == '[' {
		var rows []json.RawMessage
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return 0, fmt.Errorf("parsing bd rows: %w", err)
		}
		return queueCountRows(rows, skipEphemeral)
	}
	var envelope struct {
		Issues     []json.RawMessage `json:"issues"`
		Data       []json.RawMessage `json:"data"`
		Pagination *struct {
			Truncated bool `json:"truncated"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return 0, fmt.Errorf("parsing the bd envelope: %w", err)
	}
	if envelope.Pagination != nil && envelope.Pagination.Truncated {
		return 0, fmt.Errorf("bd returned a truncated page")
	}
	if envelope.Issues != nil {
		return queueCountRows(envelope.Issues, skipEphemeral)
	}
	return queueCountRows(envelope.Data, skipEphemeral)
}

// queueCountRows counts the rows, or only the non-ephemeral ones.
func queueCountRows(rows []json.RawMessage, skipEphemeral bool) (int, error) {
	if !skipEphemeral {
		return len(rows), nil
	}
	n := 0
	for _, raw := range rows {
		var row struct {
			Ephemeral bool `json:"ephemeral"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return 0, fmt.Errorf("parsing a bd row: %w", err)
		}
		if !row.Ephemeral {
			n++
		}
	}
	return n, nil
}

// queueAddLogColumns fills the dispatcher columns from the daemon log tail.
// The columns are set to nil before the read, so a log this run could not read
// is a stated gap rather than a key that vanished from the record.
func queueAddLogColumns(env Env, cols map[string]any) {
	cols["dispatch_ticks_1h"] = nil
	cols["dispatched_1h"] = nil
	cols["last_tick_secs"] = nil
	cols["dispatch_newest_tick_age_s"] = nil
	cols["dispatch_unparsed_lines"] = nil
	data, err := env.Tail(daemonLogPath(env.Town), queueLogTailBytes)
	if err != nil {
		return
	}
	now := env.Now()
	p := &daemonlog.Parser{Loc: now.Location()}
	cutoff := now.Add(-time.Hour)
	ticks, dispatched, unparsed := 0, 0, 0
	var newestTick, tookAt, rosterAt time.Time
	var lastTookSecs *float64
	roster, haveRoster := "", false
	for _, raw := range queueTailLines(data, queueLogMaxLines, int64(len(data)) >= queueLogTailBytes) {
		at, text := p.Parse(raw)
		text = strings.TrimPrefix(text, "spec_dispatch: ")
		if secs, ok := queueTookSecs(text); ok {
			if at.IsZero() {
				unparsed++
			} else if !at.Before(cutoff) && !at.Before(tookAt) {
				lastTookSecs, tookAt = &secs, at
			}
			continue
		}
		n, r, ok := queueTickLine(text)
		if !ok {
			continue
		}
		if at.IsZero() {
			unparsed++
			continue
		}
		if at.After(newestTick) {
			newestTick = at
		}
		if at.Before(cutoff) {
			continue
		}
		ticks++
		dispatched += n
		if !at.Before(rosterAt) {
			roster, rosterAt, haveRoster = r, at, true
		}
	}
	cols["dispatch_ticks_1h"] = ticks
	cols["dispatched_1h"] = dispatched
	cols["dispatch_unparsed_lines"] = unparsed
	if lastTookSecs != nil {
		cols["last_tick_secs"] = *lastTookSecs
	}
	if !newestTick.IsZero() {
		cols["dispatch_newest_tick_age_s"] = now.Sub(newestTick).Seconds()
	}
	if haveRoster {
		for name, value := range queueSeatColumns(roster) {
			cols[name] = value
		}
	}
}

// queueTailLines splits the tail into lines and keeps the last max. When the
// read hit its byte bound (truncated) the first line may be cut in the middle,
// and a cut line can still look like a tick, so it is dropped.
func queueTailLines(data []byte, max int, truncated bool) []string {
	lines := strings.Split(string(data), "\n")
	if truncated && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return lines
}

// queueTookSecs reads the duration from a "tick took" line.
func queueTookSecs(text string) (float64, bool) {
	m := queueTookRe.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	d, err := time.ParseDuration(m[1])
	if err != nil {
		return 0, false
	}
	return d.Seconds(), true
}

// queueTickLine reads a tick line's dispatched count and roster.
func queueTickLine(text string) (dispatched int, roster string, ok bool) {
	m := queueTickRe.FindStringSubmatch(text)
	if m == nil {
		return 0, "", false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return 0, "", false
	}
	return n, m[1], true
}

// queueSeatColumns turns a roster into seat_live_<agent> and seat_cap_<agent>
// columns. The last "total L/C" segment is Budget.Picture's town total, not a
// seat, so it is dropped; a segment the seat pattern does not open (a closed
// seat's reason split at its comma, "no seats") contributes nothing.
func queueSeatColumns(roster string) map[string]any {
	parts := strings.Split(strings.TrimSpace(roster), ", ")
	if n := len(parts); n > 0 && strings.HasPrefix(strings.TrimSpace(parts[n-1]), "total ") {
		parts = parts[:n-1]
	}
	out := make(map[string]any, len(parts)*2)
	for _, part := range parts {
		m := queueSeatRe.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			continue
		}
		live, liveErr := strconv.Atoi(m[2])
		capacity, capErr := strconv.Atoi(m[3])
		if liveErr != nil || capErr != nil {
			continue
		}
		out["seat_live_"+m[1]] = live
		out["seat_cap_"+m[1]] = capacity
	}
	return out
}
