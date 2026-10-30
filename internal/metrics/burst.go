package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/daemonlog"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// BurstVersion is the schema version a burst row is written as (v).
const BurstVersion = 1

const (
	// BurstFastInterval is the gap between burst samples while the host is
	// pressed.
	BurstFastInterval = 15 * time.Second
	// BurstSlowInterval is the gap between burst samples otherwise.
	BurstSlowInterval = time.Minute
	// BurstIdlePct is the idle share under which the host counts as pressed.
	BurstIdlePct = 15.0
	// BurstExecTaxMS is the exec cost over which the host counts as pressed.
	BurstExecTaxMS = 50.0
)

// BurstInterval is the gap before the next burst sample: BurstFastInterval
// while the host is pressed and BurstSlowInterval otherwise.
//
// The host is pressed when the idle share is under BurstIdlePct or the exec tax
// is over BurstExecTaxMS. A reading that could not be taken is not a reading of
// a calm host, so it decides nothing by itself: an unread idle share beside a
// measured tax over the limit is pressure, and beside a measured tax under it
// is not. Only when neither reading exists is the host unknown rather than
// calm, and an unknown host takes the fast cadence: top is the reading a loaded
// host loses first, and the burst a slow cadence skips is the one this sampler
// exists to keep.
func BurstInterval(idlePct, execTaxMS *float64) time.Duration {
	if idlePct == nil && execTaxMS == nil {
		return BurstFastInterval
	}
	if idlePct != nil && *idlePct < BurstIdlePct {
		return BurstFastInterval
	}
	if execTaxMS != nil && *execTaxMS > BurstExecTaxMS {
		return BurstFastInterval
	}
	return BurstSlowInterval
}

// BurstRow is one burst sample of the town under host pressure, written by
// RunBurst as one JSON line.
//
// The hourly snapshot cannot explain a spike: by the time it is taken the
// burst is over. The reference burst is 17:07 on 2026-10-26 — 0 percent idle,
// 243 ms of exec tax per exec, the overrun guard's tick 4 minutes late — and
// the record holds none of it. A burst row is sampled often while the host is
// pressed (BurstInterval), and each reading it cannot take is null rather than
// zero, as the snapshot's groups leave theirs: a host whose top failed is not
// a host with no load. A row holding no reading at all is an error, and
// RunBurst writes nothing for it, so a sampler that lost every seam cannot
// pass for a quiet host.
//
// One sample costs the host_cpu group's four process-table reads and its
// one-second window, one top, one daemon-log tail and one bd read per rig for
// the landing queue: a price worth paying while the host is pressed, and what
// BurstInterval's slow cadence keeps from being paid all day.
//
// A daemon in a container cannot take the host readings with top and ps, so in
// container mode they come from the host agent's probe file instead: IdlePct,
// TopConsumers and ExecTaxMS hold the file's readings, HostAgeS says how far
// before TS the file was stamped, and a reading the file could not give is null
// with its reason in HostErrors. Outside container mode those two fields are
// absent and the three readings are taken from the host at TS.
type BurstRow struct {
	// V is the schema version, BurstVersion.
	V int `json:"v"`
	// TS is when the sample was taken, in RFC 3339 UTC.
	TS string `json:"ts"`
	// IntervalS is the gap the next sample should wait, from BurstInterval.
	IntervalS float64 `json:"interval_s"`
	// IdlePct is the host's idle share, nil when top could not be read.
	IdlePct *float64 `json:"idle_pct"`
	// ExecTaxMS is the daemon's last median exec cost, nil when the health file
	// holds none or one too old to be a reading of now.
	ExecTaxMS *float64 `json:"exec_tax_ms"`
	// ExecTaxAgeS is how old that measurement was, nil when its stamp could not
	// be read either.
	ExecTaxAgeS *float64 `json:"exec_tax_age_s"`
	// TopConsumers are the consumers that burned the most cores, empty when the
	// sample was taken and nothing burned any, and nil when it could not be
	// taken, or when a window it only partly measured ranked nobody.
	TopConsumers []BurstConsumer `json:"top_consumers"`
	// Polecats is the live session count the newest dispatcher roster names,
	// nil when the log holds no roster recent enough to answer for now.
	Polecats *int `json:"polecats"`
	// LandingQueue is the beads waiting to land, nil when any rig's queue could
	// not be read.
	LandingQueue *int `json:"landing_queue"`
	// Running is the test runs in progress, empty when the log was read and
	// nothing was running, and nil when it could not be read.
	Running []BurstRun `json:"running"`
	// HostErrors names, per host reading this row could not take from an
	// outside source (BurstHostReading.Errors), why it holds no number. It is
	// absent outside container mode, where the host's own commands answer for
	// a failed reading with the sampler's log line alone.
	HostErrors map[string]string `json:"host_errors,omitempty"`
	// HostAgeS is how many seconds before TS the host agent read the idle
	// share and the consumers this row holds (BurstHostReading.AgeS): the two
	// describe the host then, not at TS, and IntervalS is taken from the idle
	// share at that age. It is absent outside container mode, where both
	// readings are taken at TS, and on a row the file answered no reading for,
	// which HostErrors explains.
	HostAgeS *float64 `json:"host_age_s,omitempty"`
}

// BurstHostReading is a burst sample's host readings taken from outside this
// process: the three a sample otherwise takes from top, ps and the daemon's
// health file, each absent when it could not be read.
type BurstHostReading struct {
	// IdlePct is the host's idle share.
	IdlePct *float64
	// Consumers are the busiest processes in cores, empty when the sample was
	// taken and nothing burned a core and nil when it could not be taken.
	Consumers []BurstConsumer
	// ExecTaxMS is the exec tax measured and ExecTaxAgeS how old that
	// measurement was, both nil when the tax was not read.
	ExecTaxMS   *float64
	ExecTaxAgeS *float64
	// AgeS is how many seconds before the sample the source read IdlePct and
	// Consumers, and is written into the row's HostAgeS. nil when the source
	// read them at the sample's own instant.
	AgeS *float64
	// Errors names why a reading is absent, keyed by the row field it would
	// have filled, and is written into the row's HostErrors.
	Errors map[string]string
}

// BurstConsumer is one process name and the cores it burned over the sample's
// CPU window.
type BurstConsumer struct {
	Name  string  `json:"name"`
	Cores float64 `json:"cores"`
}

// The kinds of test run a burst row can name in Running.
const (
	burstSweep    = "sweep"
	burstPostLand = "post_land"
)

// BurstRun is one test run the daemon log shows in progress when the sample was
// taken.
type BurstRun struct {
	// Kind is burstSweep for a tier sweep and burstPostLand for a rig's
	// post-landing suite.
	Kind string `json:"kind"`
	// Rig is the rig whose tiers the run is testing.
	Rig string `json:"rig"`
	// Commit is the short commit the run is testing.
	Commit string `json:"commit"`
	// StartedS is how long before the sample the run's start line was written.
	StartedS float64 `json:"started_s"`
}

// BurstPath is the log RunBurst appends burst rows to under town.
func BurstPath(town string) string {
	return filepath.Join(town, ".runtime", "metrics", "bursts.jsonl")
}

// RunBurst takes one burst sample and appends it to the town's burst log,
// returning the row and the path it was written to. A sampler calls it once and
// then waits the row's IntervalS before the next call.
func RunBurst(ctx context.Context, env Env) (BurstRow, string, error) {
	path := BurstPath(env.Town)
	row, err := SampleBurst(ctx, env)
	if err != nil {
		return row, path, err
	}
	if err := battery.Append(path, row); err != nil {
		return row, path, fmt.Errorf("appending the burst row: %w", err)
	}
	return row, path, nil
}

// SampleBurst takes one burst sample through env's seams.
//
// An error means no reading at all could be taken, so the caller has nothing to
// record; a row beside that error holds only nils. Every other outcome is a row
// whose readings that failed are null, and a row that names a running test is a
// reading of its own: it ranks no consumer and still says what was testing.
func SampleBurst(ctx context.Context, env Env) (BurstRow, error) {
	now := env.now()
	row := BurstRow{
		V:  BurstVersion,
		TS: now.UTC().Format(time.RFC3339),
	}
	if env.BurstHost != nil {
		host := env.BurstHost(now)
		row.IdlePct = host.IdlePct
		row.ExecTaxMS, row.ExecTaxAgeS = host.ExecTaxMS, host.ExecTaxAgeS
		row.TopConsumers = burstRankedConsumers(host.Consumers, burstTopN)
		row.HostErrors = host.Errors
		row.HostAgeS = host.AgeS
	} else {
		row.IdlePct = burstIdle(ctx, env)
		row.ExecTaxMS, row.ExecTaxAgeS = burstExecTax(env, now)
		if consumers, ok := burstConsumers(ctx, env); ok {
			row.TopConsumers = consumers
		}
	}
	polecats, running, logRead := burstLogReadings(env, now)
	row.Polecats = polecats
	if logRead {
		row.Running = running
	}
	row.LandingQueue = burstLandingQueue(ctx, env)
	row.IntervalS = BurstInterval(row.IdlePct, row.ExecTaxMS).Seconds()
	if row.IdlePct == nil && row.ExecTaxMS == nil && row.TopConsumers == nil &&
		row.Polecats == nil && row.LandingQueue == nil && len(row.Running) == 0 {
		return row, errors.New("no reading of the host, its consumers or the town's queues could be taken")
	}
	return row, nil
}

// burstIdle reads the host's idle share with the probe the snapshot's host
// group reads it with, so the two cannot disagree on what idle means.
func burstIdle(ctx context.Context, env Env) *float64 {
	if env.Run == nil {
		return nil
	}
	value, ok := hostIdleProbe.read(ctx, env, hostCommandTimeout)
	if !ok {
		return nil
	}
	idle, ok := value.(float64)
	if !ok {
		return nil
	}
	return &idle
}

// burstExecTaxStaleAfter is how old the daemon's last exec-tax measurement may
// be and still count as the host's: the daemon writes the health file every
// tick, so a measurement this old means the ticks themselves stopped, and an
// old number is not a reading of now.
const burstExecTaxStaleAfter = 10 * time.Minute

// burstExecTax reads the exec tax the daemon last measured and its age. The tax
// is nil when the health file cannot be read, holds no measurement, or is too
// old to be current; the age is kept whenever the report's stamp was readable,
// so a null tax says how old the reason was.
func burstExecTax(env Env, now time.Time) (tax, age *float64) {
	if env.ReadFile == nil {
		return nil, nil
	}
	data, err := env.ReadFile(townhealth.Path(env.Town))
	if err != nil {
		return nil, nil
	}
	var report townhealth.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, nil
	}
	if report.At.IsZero() {
		return nil, nil
	}
	seconds := now.Sub(report.At).Seconds()
	if seconds < 0 {
		// The report is stamped after the sample: the town's clocks disagree,
		// and its measurement is current rather than newer than fresh.
		seconds = 0
	}
	age = &seconds
	if report.ExecTaxMS == nil || *report.ExecTaxMS < 0 || seconds > burstExecTaxStaleAfter.Seconds() {
		return nil, age
	}
	return report.ExecTaxMS, age
}

// burstTopN is how many consumers a row names: the three busiest explain a
// spike, and the snapshot's host_cpu group holds the whole table.
const burstTopN = 3

// burstConsumers samples the host's consumers with the snapshot's host_cpu
// source and ranks them. ok is false when the sample could not be taken, or
// when it was taken over a window the group only partly measured and ranked
// nobody: either is a gap in the row rather than a host with nothing running.
func burstConsumers(ctx context.Context, env Env) ([]BurstConsumer, bool) {
	if env.Run == nil || env.Wait == nil {
		return nil, false
	}
	cols, err := hostCPUSource{}.Collect(ctx, env)
	if err != nil {
		return nil, false
	}
	consumers := burstTopConsumers(cols, burstTopN)
	if len(consumers) == 0 && burstHostCPUTablePartial(cols) {
		return nil, false
	}
	return consumers, true
}

// burstHostCPUTablePartial reports whether the host_cpu group says its window
// was only partly read: unmeasured_pids are processes that exited between its
// two samples, whose burn after the first read is in no column, and
// regressed_pids are ids whose two samples are not one process, whose first
// process is in no column. A rank of nobody over such a window is not an idle
// host.
func burstHostCPUTablePartial(cols map[string]any) bool {
	unmeasured, okUnmeasured := cols["unmeasured_pids"].(int)
	regressed, okRegressed := cols["regressed_pids"].(int)
	if !okUnmeasured || !okRegressed {
		// The group's own account of its gaps could not be read, so the window
		// is not known to be whole.
		return true
	}
	return unmeasured > 0 || regressed > 0
}

// burstTopConsumers ranks a host_cpu group's columns into the busiest
// consumers: the named ones the group records, then the other names it ranked.
// A consumer with no reading or at zero cores is left out, so an idle host
// ranks none and a scanner that could not be read is not named at 0.
func burstTopConsumers(cols map[string]any, n int) []BurstConsumer {
	out := make([]BurstConsumer, 0, len(hostCPUConsumers)+hostCPUOtherN)
	for _, consumer := range hostCPUConsumers {
		if cores, ok := cols[consumer.column+"_cores"].(float64); ok && cores > 0 {
			out = append(out, BurstConsumer{Name: consumer.column, Cores: cores})
		}
	}
	for i := 1; i <= hostCPUOtherN; i++ {
		name, _ := cols[fmt.Sprintf("other_%d_name", i)].(string)
		cores, ok := cols[fmt.Sprintf("other_%d_cores", i)].(float64)
		if name == "" || !ok || cores <= 0 {
			continue
		}
		out = append(out, BurstConsumer{Name: name, Cores: cores})
	}
	return burstRankedConsumers(out, n)
}

// burstRankedConsumers orders consumers the way the row names them: the
// busiest first, the name breaking a tie, and at most n. The ranking is a copy,
// so a caller's list keeps its order. nil stays nil and an empty list stays
// empty: one is a list that was never taken, the other a host nothing burned.
func burstRankedConsumers(consumers []BurstConsumer, n int) []BurstConsumer {
	if consumers == nil {
		return nil
	}
	ranked := make([]BurstConsumer, len(consumers))
	copy(ranked, consumers)
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Cores != ranked[j].Cores {
			return ranked[i].Cores > ranked[j].Cores
		}
		return ranked[i].Name < ranked[j].Name
	})
	if len(ranked) > n {
		ranked = ranked[:n]
	}
	return ranked
}

// burstLogTailBytes is how much of the daemon log a row reads: its newest
// megabyte, which holds the roster the polecat count comes from and the start
// lines of the runs it can name. A run whose start line is older than the read
// cannot be told from one that finished outside it, so it is not named.
const burstLogTailBytes = 1024 * 1024

// burstRunMaxAge is how long a run may have been going and still count as the
// one in progress: a start line older than this is not evidence of a run now,
// because its end line may have scrolled out of the tail the row reads, and a
// start line alone says nothing about whether the run is still going.
const burstRunMaxAge = 30 * time.Minute

// burstRosterMaxAge bounds the roster the polecat count comes from: an older
// roster names the sessions of another time, and a count read against it would
// answer a question the row does not ask.
const burstRosterMaxAge = 10 * time.Minute

// The daemon log lines a burst row reads. Each is the line its writer logs
// today; a line whose shape changes drops out of the row rather than being read
// as something else.
var (
	// burstSweepStartRe matches the tier sweep's start line, which names the
	// rig, the commit and the tiers it will run.
	burstSweepStartRe = regexp.MustCompile(`^tier_sweep: (\S+): sweep started ([0-9a-f]{4,40}) \(`)
	// burstSweepEndRe matches the two lines that end a sweep.
	burstSweepEndRe = regexp.MustCompile(`^tier_sweep: (\S+): (?:swept|sweep aborted) ([0-9a-f]{4,40}) \(`)
	// burstPostStartRe matches the post-land runner's line for a run it
	// started.
	burstPostStartRe = regexp.MustCompile(`^landing_worker: (\S+): post-land: running ".*" at ([0-9a-f]{4,40}) \(`)
	// burstPostVerdictRe matches the post-land lines that carry a run's verdict:
	// green, red, a run that could not start. The start line matches none of
	// them.
	burstPostVerdictRe = regexp.MustCompile(`^landing_worker: (\S+): post-land: (?:green at |RED: .*? at |WARNING could not run .*? at )([0-9a-f]{4,40}) \(`)
	// burstPostSupersededRe matches the line for a run a daemon restart cut
	// short. "supersedes queued" is a different line: it names the new commit
	// first and a queued one second, and the queued run never started, so
	// neither is a run this read has to finish.
	burstPostSupersededRe = regexp.MustCompile(`^landing_worker: (\S+): post-land: ([0-9a-f]{4,40}) \([^)]*\) superseded:`)
)

// burstRunKey identifies one run. The same commit can be swept again after a
// fix, so a start line after an end line is a new run rather than the old one.
type burstRunKey struct {
	kind, rig, commit string
}

// burstRunState is one run the log named, in the order its lines came.
type burstRunState struct {
	key     burstRunKey
	started time.Time
	ended   bool
}

// burstLogReadings reads the daemon log's newest lines for the live polecat
// count and the test runs in progress.
//
// polecats is nil when the read failed, holds no roster, or holds none recent
// enough to name today's sessions. running is nil when the read failed, and
// empty when it succeeded and nothing was running.
func burstLogReadings(env Env, now time.Time) (polecats *int, running []BurstRun, read bool) {
	if env.Tail == nil {
		return nil, nil, false
	}
	data, err := env.Tail(daemonLogPath(env.Town), burstLogTailBytes)
	if err != nil {
		return nil, nil, false
	}
	p := &daemonlog.Parser{Loc: now.Location()}
	roster, rosterAt := "", time.Time{}
	runs := make(map[burstRunKey]*burstRunState)
	var order []*burstRunState
	for _, raw := range strings.Split(string(data), "\n") {
		at, text := p.Parse(raw)
		if at.IsZero() {
			continue // a line the read cut at the top carries no stamp
		}
		if _, seats, isTick := queueTickLine(strings.TrimPrefix(text, "spec_dispatch: ")); isTick && !at.Before(rosterAt) {
			roster, rosterAt = seats, at
			continue
		}
		if m := burstSweepStartRe.FindStringSubmatch(text); m != nil {
			order = append(order, startBurstRun(runs, burstSweep, m[1], m[2], at))
			continue
		}
		if m := burstSweepEndRe.FindStringSubmatch(text); m != nil {
			endBurstRun(runs, burstSweep, m[1], m[2])
			continue
		}
		if m := burstPostStartRe.FindStringSubmatch(text); m != nil {
			order = append(order, startBurstRun(runs, burstPostLand, m[1], m[2], at))
			continue
		}
		if m := burstPostVerdictRe.FindStringSubmatch(text); m != nil {
			endBurstRun(runs, burstPostLand, m[1], m[2])
			continue
		}
		if m := burstPostSupersededRe.FindStringSubmatch(text); m != nil {
			endBurstRun(runs, burstPostLand, m[1], m[2])
		}
	}
	return burstLivePolecats(roster, rosterAt, now), burstRunningRuns(runs, order, now), true
}

// startBurstRun records a run's start line and returns its state, which a start
// line for the same run replaces.
func startBurstRun(runs map[burstRunKey]*burstRunState, kind, rig, commit string, at time.Time) *burstRunState {
	state := &burstRunState{key: burstRunKey{kind: kind, rig: rig, commit: commit}, started: at}
	runs[state.key] = state
	return state
}

// endBurstRun marks a run finished. An end line for a run whose start line is
// not in the read names nothing to finish.
func endBurstRun(runs map[burstRunKey]*burstRunState, kind, rig, commit string) {
	if state, ok := runs[burstRunKey{kind: kind, rig: rig, commit: commit}]; ok {
		state.ended = true
	}
}

// burstRunningRuns lists the runs that are still going, oldest first. A start
// line a later one replaced, or one whose end line followed it, is over.
func burstRunningRuns(runs map[burstRunKey]*burstRunState, order []*burstRunState, now time.Time) []BurstRun {
	running := []BurstRun{}
	for _, state := range order {
		if runs[state.key] != state || state.ended {
			continue
		}
		age := now.Sub(state.started).Seconds()
		if age < 0 {
			age = 0 // a line stamped after the sample is the clocks' disagreement
		}
		if age > burstRunMaxAge.Seconds() {
			continue
		}
		running = append(running, BurstRun{
			Kind:     state.key.kind,
			Rig:      state.key.rig,
			Commit:   state.key.commit,
			StartedS: age,
		})
	}
	sort.Slice(running, func(i, j int) bool {
		a, b := running[i], running[j]
		if a.StartedS != b.StartedS {
			return a.StartedS > b.StartedS
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Rig != b.Rig {
			return a.Rig < b.Rig
		}
		return a.Commit < b.Commit
	})
	return running
}

// burstLivePolecats sums the live counts of the roster the dispatcher last
// wrote. nil is a roster that is missing, too old, or names no seat: a roster
// this code cannot read is not a town of no polecats.
func burstLivePolecats(roster string, at, now time.Time) *int {
	if roster == "" || at.IsZero() || now.Sub(at) > burstRosterMaxAge {
		return nil
	}
	live, counted := 0, false
	for name, value := range queueSeatColumns(roster) {
		if !strings.HasPrefix(name, "seat_live_") {
			continue
		}
		if n, ok := value.(int); ok {
			live += n
			counted = true
		}
	}
	if !counted {
		return nil
	}
	return &live
}

// burstLandingTimeout bounds the landing-queue read: one fast cadence, so a bd
// the burst itself has made slow costs the row its queue column and not the
// whole sample.
const burstLandingTimeout = BurstFastInterval

// burstLandingQueue counts the beads waiting to land, summed over the rigs the
// way the queue group's waiting_to_land column counts them: from the count
// each rig's landing worker published while it is fresh, and from the board
// itself for a rig whose count is stale or missing, because a sample this
// frequent must not re-list a board another pass read a moment ago
// (gt-hw6ih). nil is a registry or a rig that could not be read: a partial sum
// would read as a queue that shrank.
func burstLandingQueue(ctx context.Context, env Env) *int {
	if env.Beads == nil || env.ReadFile == nil {
		return nil
	}
	rigs, err := queueRigs(env)
	if err != nil || len(rigs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, burstLandingTimeout)
	defer cancel()
	n, ok := queueSumCount(ctx, env, rigs, queueLandingColumn())
	if !ok {
		return nil
	}
	return &n
}
