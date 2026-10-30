package metrics

import (
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

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemonlog"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The queue group: how much work the town holds, and what the spec dispatcher
// did with it in the last hour.
//
// Each bead column is one store read per registered rig, summed, except
// waiting_to_land, which a rig's landing worker's published count answers
// while that count is fresh, and whose rigs a store that holds several of them
// answers in one request (queueLandingColumn, beads.ListAcross). The rigs come
// from mayor/rigs.json, or from the registry section of mayor/town.json on the
// two-file layout (internal/config/layout.go). Each read goes through a store
// opened for the rig's own directory, which is the scoping `bd -C <town>/<rig>`
// gave, and the filters each column asks for are queueBeadColumns' own.
//
// A read that fails, or a ready page bd reports as cut, nils its column; the
// store's own argv and failure classification are the seam's business.
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

// queueBdTimeout is one store read's deadline. A column reads its rigs
// queueRigParallel at a time, so nine rigs cost at most three deadlines (15 s)
// for a column, and the group's own deadline (SourceTimeout) caps the whole
// group regardless. A bd hung on a dead Dolt server costs its column rather
// than the whole group.
const queueBdTimeout = 5 * time.Second

// queueRigParallel is how many rigs one column reads at once. The four columns
// run side by side, so at most four times this many store reads are alive.
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

// queueBeadColumn is one bead count and the store read in one rig that fills
// it. A column is read one of two ways: read runs its query in one rig's own
// store, and list with readAcross reads it as a plain list across every rig,
// which a store holding several rigs answers for all of them in one request.
// Exactly one of the two is set.
type queueBeadColumn struct {
	column string
	read   func(BeadStore) ([]*beads.Issue, error)
	// list is the column's query as a plain list (beads.ListAcross).
	list *beads.ListOptions
	// readAcross runs list in one rig's own store. It is the column's own
	// function so a bd read stays attributed to the column that asks for it:
	// internal/beads' census names the caller from the frame that started the
	// call, and its budgets are keyed by that name (census_budgets.go).
	readAcross beads.AcrossRead
	// published, when set, is where a count someone else read the same board
	// for answers this column: the landing worker's own list, which the sample
	// can report instead of repeating it (gt-hw6ih). It returns the counts
	// fresh as of now, and a rig it leaves out is read from the store.
	published func(env Env, now time.Time) map[string]int
	// skipEphemeral leaves out the rows bd marks ephemeral (infrastructure
	// wisps, not work). It is for a column read per rig: a list column counts
	// every row the list answers.
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
			read: func(s BeadStore) ([]*beads.Issue, error) {
				return s.ReadyFiltered(beads.ReadyOptions{Limit: 0})
			},
		},
		{
			column: "dispatchable",
			read: func(s BeadStore) ([]*beads.Issue, error) {
				return s.ReadyFiltered(beads.ReadyOptions{
					Unassigned:    true,
					ExcludeLabels: labels,
					ExcludeTypes:  specdispatch.NonWorkBeadTypes(),
					Limit:         0,
				})
			},
		},
		queueLandingColumn(),
		{
			column:        "blocked",
			read:          func(s BeadStore) ([]*beads.Issue, error) { return s.Blocked() },
			skipEphemeral: true,
		},
	}
}

// queueLandingColumn is the store read behind waiting_to_land, the beads
// submitted and waiting to land. The burst row counts the same board
// (BurstRow).
//
// The landing worker lists this board at the top of every pass anyway, so a
// sample reads the count that pass published (Env.LandingQueue) and lists the
// board only for a rig whose count is stale or was never published
// (gt-hw6ih).
//
// List adds no status filter of its own, so the label and bd's default list
// scope are the whole query, as they were for the pre-seam `bd list`; Limit 0
// asks for the whole board rather than bd's default page, which is what the
// pre-seam --limit 0 did.
//
// The rigs a tracker rig's store answers come back in one request for the
// whole town instead of one per rig; the option is the one readAcross reads,
// so both forms ask the same board.
func queueLandingColumn() queueBeadColumn {
	opts := beads.ListOptions{Label: "gt:ready-to-land", Limit: 0}
	return queueBeadColumn{
		column: "waiting_to_land",
		published: func(env Env, now time.Time) map[string]int {
			if env.LandingQueue == nil {
				return nil
			}
			return env.LandingQueue(now)
		},
		list:       &opts,
		readAcross: func(l beads.Lister) ([]*beads.Issue, error) { return l.List(opts) },
	}
}

func (queueSource) Group() string { return "queue" }

func (queueSource) Collect(ctx context.Context, env Env) (map[string]any, error) {
	rigs, err := queueRigs(env)
	if err != nil {
		return nil, err
	}
	if env.Beads == nil {
		return nil, fmt.Errorf("queue: the environment has no bead store")
	}
	cols := make(map[string]any, 8)
	queueAddLogColumns(env, cols)
	columns := queueBeadColumns()
	done := make(chan queueColumnResult, len(columns))
	for _, c := range columns {
		go func(c queueBeadColumn) {
			done <- queueColumnResult{column: c.column, value: queueSumBeads(ctx, env, rigs, c)}
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

// queueRig is one registered rig: the name its directory and the landing
// worker's published counts use, and the beads prefix its store files its
// issues under, which is how a store that holds several rigs (the tracker)
// names it.
type queueRig struct {
	name   string
	prefix string
}

// queueRigs lists the registered rigs, sorted by name. An unreadable registry
// is an error: summed over no rigs, every bead column would read as an empty
// town.
func queueRigs(env Env) ([]queueRig, error) {
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
		return queueRigList(tc.Registry), nil
	}
	var rc config.RigsConfig
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, fmt.Errorf("queue: parsing mayor/rigs.json: %w", err)
	}
	return queueRigList(&rc), nil
}

// queueRigList sorts the registry's rigs so a snapshot reads the same on every
// run.
func queueRigList(rc *config.RigsConfig) []queueRig {
	rigs := make([]queueRig, 0, len(rc.Rigs))
	for name, entry := range rc.Rigs {
		rigs = append(rigs, queueRig{name: name, prefix: queueRigPrefix(name, entry)})
	}
	sort.Slice(rigs, func(i, j int) bool { return rigs[i].name < rigs[j].name })
	return rigs
}

// queueRigPrefix is the prefix the rig's store files its issues under: the
// registry's beads prefix without its trailing dash. A rig the registry records
// no prefix for keeps its name, so a rig a store cannot name fails its own read
// instead of being summed in as an empty one.
func queueRigPrefix(name string, e config.RigEntry) string {
	if e.BeadsConfig != nil {
		if p := strings.TrimSuffix(e.BeadsConfig.Prefix, "-"); p != "" {
			return p
		}
	}
	return name
}

// queueSumBeads runs one column's read in every rig and sums the beads. Any
// rig that fails leaves the column nil: a partial sum would read as a queue
// that shrank.
func queueSumBeads(ctx context.Context, env Env, rigs []queueRig, col queueBeadColumn) any {
	n, ok := queueSumCount(ctx, env, rigs, col)
	if !ok {
		return nil
	}
	return n
}

// queueSumCount is queueSumBeads's reading as a number and a flag: ok is false
// when any rig failed, so a caller that needs the count cannot mistake a
// failed read for an empty queue. A rig the column's published count answers
// costs no store read; the rigs it does not answer are read as before.
func queueSumCount(ctx context.Context, env Env, rigs []queueRig, col queueBeadColumn) (int, bool) {
	var published map[string]int
	if col.published != nil {
		published = col.published(env, env.now())
	}
	sum := 0
	read := make([]queueRig, 0, len(rigs))
	for _, rig := range rigs {
		if n, ok := published[rig.name]; ok {
			sum += n
			continue
		}
		read = append(read, rig)
	}
	counts, ok := queueRigCounts(ctx, env, read, col)
	if !ok {
		return 0, false
	}
	for _, n := range counts {
		sum += n
	}
	return sum, true
}

// queueRigCounts reads one column in rigs and returns each rig's count keyed by
// name. ok is false when any rig failed.
func queueRigCounts(ctx context.Context, env Env, rigs []queueRig, col queueBeadColumn) (map[string]int, bool) {
	if col.list != nil {
		return queueListCounts(ctx, env, rigs, *col.list, col.readAcross)
	}
	return queueStoreCounts(ctx, env, rigs, col)
}

// queueStoreCounts runs one column's read in every rig, queueRigParallel at a
// time.
func queueStoreCounts(ctx context.Context, env Env, rigs []queueRig, col queueBeadColumn) (map[string]int, bool) {
	type result struct {
		rig string
		n   int
		err error
	}
	results := make([]result, len(rigs))
	sem := make(chan struct{}, queueRigParallel)
	var wg sync.WaitGroup
	for i, rig := range rigs {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, rig queueRig) {
			defer func() { <-sem; wg.Done() }()
			n, err := queueCount(ctx, env, filepath.Join(env.Town, rig.name), col)
			results[i] = result{rig: rig.name, n: n, err: err}
		}(i, rig)
	}
	wg.Wait()
	counts := make(map[string]int, len(rigs))
	for _, r := range results {
		if r.err != nil {
			return nil, false
		}
		counts[r.rig] = r.n
	}
	return counts, true
}

// queueListCounts reads a column as a plain list through beads.ListAcross: the
// rigs a store that holds several of them answers cost one request between
// them, and every other rig is read from its own store, queueRigParallel at a
// time. A rig left unanswered is a failed read. Each rig's store is opened with
// a deadline of its own (queueBdTimeout), so one hung store costs its rig and
// the rest still answer. That deadline is set when the batch opens, not when
// the rig's read starts: a read has to reuse the store it was opened with, and
// a second open per rig would double the store reads the census budgets. The
// group's own deadline still caps the whole.
func queueListCounts(ctx context.Context, env Env, rigs []queueRig, opts beads.ListOptions, read beads.AcrossRead) (map[string]int, bool) {
	if len(rigs) == 0 {
		return map[string]int{}, true
	}
	if env.Beads == nil {
		return nil, false
	}
	names := make(map[string]string, len(rigs)) // the rig's prefix to its name
	prefixes := make([]string, len(rigs))
	for i, rig := range rigs {
		if _, dup := names[rig.prefix]; dup {
			// One prefix for two rigs is a registry the town cannot route, and
			// each rig's answer would count the other's board too.
			return nil, false
		}
		names[rig.prefix] = rig.name
		prefixes[i] = rig.prefix
	}
	// Each rig's store is opened once, under a deadline of its own that is
	// set when ListAcross opens the batch (it opens every rig before it reads
	// one). The cancels run when the batch is answered.
	var cancels []context.CancelFunc
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	open := func(prefix string) (beads.Lister, error) {
		rigCtx, cancel := context.WithTimeout(ctx, queueBdTimeout)
		cancels = append(cancels, cancel)
		store := env.Beads(rigCtx, filepath.Join(env.Town, names[prefix]))
		if store == nil {
			return nil, fmt.Errorf("queue: no bead store for %s", names[prefix])
		}
		return store, nil
	}
	res, err := beads.ListAcross(prefixes, open, opts, read, queueRigParallel)
	if err != nil {
		return nil, false
	}
	counts := make(map[string]int, len(rigs))
	for _, rig := range rigs {
		if err := res.Errors[rig.prefix]; err != nil {
			return nil, false
		}
		counts[rig.name] = len(res.Rigs[rig.prefix])
	}
	return counts, true
}

// queueCount runs one column's read in one rig's store under its own deadline
// and returns how many beads it answered, leaving out the ephemeral ones when
// the column asks. Only a column read with read takes it: a list column reads
// through queueListCounts.
func queueCount(ctx context.Context, env Env, rigPath string, col queueBeadColumn) (int, error) {
	callCtx, cancel := context.WithTimeout(ctx, queueBdTimeout)
	defer cancel()
	store := env.Beads(callCtx, rigPath)
	if store == nil {
		return 0, fmt.Errorf("queue: no bead store for %s", rigPath)
	}
	issues, err := col.read(store)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, issue := range issues {
		if col.skipEphemeral && issue.Ephemeral {
			continue
		}
		n++
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
