package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// The Tier sweeps panel reads the daemon log, the only place a sweep's history
// survives: each rig's .runtime/tier-sweep/<rig>.json keeps just its latest run
// (internal/daemon/tier_sweep.go). The log is scanned incrementally, the way the
// om panel scans it, so a poll reads only what it gained.

const (
	// tierSweepRows is how many sweeps the panel lists, newest first.
	tierSweepRows = 5
	// tierSweepRowsKept is the backlog the incremental scan holds, deep enough
	// that the panel's window never falls off the end.
	tierSweepRowsKept = 40
	// tierSweepTrendPerRig is how many sweeps each rig's wall trend keeps: a
	// month of hourly sweeps is enough to see devops test speed move.
	tierSweepTrendPerRig = 30
	// tierSweepStaleAfter is when a stage-finished line stops meaning a sweep is
	// in flight. A cycle's longest stage is the integration one, bounded by the
	// daemon's 90m budget: a rig whose last line is older ran through a daemon
	// that died mid-cycle.
	tierSweepStaleAfter = 90 * time.Minute
)

var (
	// tierSweepStageRe matches the daemon's line for a stage that finished:
	// "tier_sweep: gastown: shell finished (exit 0) in 1m5s; last lines:". The
	// duration is captured because the wall trend charts each tier's own time,
	// which the swept line that closes the cycle does not carry.
	tierSweepStageRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) tier_sweep: (\S+): (\S+) finished \(exit -?\d+\) in (\S+); last lines:$`)
	// tierSweepSweptRe matches the line closing a cycle, whose trailing " in
	// <dur>" is absent on a cycle that ran before gt-iqzr0:
	// "tier_sweep: gastown: swept a9be03e4 (shell GREEN, integration GREEN) in 9m36s".
	tierSweepSweptRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) tier_sweep: (\S+): swept (\S+) \(([^)]*)\)(?: in (\S+))?\s*$`)
	// tierSweepStartedRe matches the line opening a cycle, which names the
	// tiers the cycle will run (gt-rntre):
	// "tier_sweep: gastown: sweep started a9be03e4 (shell, integration, race)".
	tierSweepStartedRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) tier_sweep: (\S+): sweep started (\S+) \(([^)]*)\)\s*$`)
	// tierSweepAbortedRe matches the line closing a cycle that started and
	// ended with no swept line, because no stage ran or the record was not
	// written:
	// "tier_sweep: gastown: sweep aborted a9be03e4 (no stage ran)".
	tierSweepAbortedRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) tier_sweep: (\S+): sweep aborted (\S+) \(([^)]*)\)\s*$`)
)

// tierSweepEvent is the newest thing the log said about one rig: a stage that
// finished, or a whole cycle that swept. Which one it is decides whether a sweep
// is in flight for that rig.
type tierSweepEvent struct {
	at    time.Time
	stage string // the stage that finished, for a stage line
	swept bool   // the line closed a cycle
}

// tierSweepStarted is a cycle the log opened and no swept line has closed: what
// the "sweep started" line said, so the pane names the stages the cycle runs
// and times it from its true start rather than inferring it (gt-rntre).
type tierSweepStarted struct {
	at    time.Time
	sha   string
	tiers []string
}

// tierSweepReader keeps the daemon-log scan between polls.
type tierSweepReader struct {
	logPath  string
	stateDir string
	// rigs lists the town's known rig names. A rig the log names is used in a
	// path under stateDir only when this names it (gt-2czgm).
	rigs func() ([]string, error)

	mu     sync.Mutex
	offset int64
	readOK bool
	last   map[string]tierSweepEvent
	// start holds the cycle each rig opened and has not closed. A rig absent
	// from it is read the old way, off its shell stage's finished line.
	start map[string]tierSweepStarted
	// walls holds the tier wall times the stage lines have reported for a rig's
	// cycle in flight, until its swept line closes it.
	walls map[string]map[string]float64
	rows  []dashboard.TierSweepRow
	// byRig holds each rig's newest sweeps, oldest first, for the wall trend:
	// the panel's own rows are a town-wide window too shallow for one rig's
	// series.
	byRig map[string][]dashboard.TierSweepRow
}

func newTierSweepReader(townRoot string) *tierSweepReader {
	return &tierSweepReader{
		logPath:  filepath.Join(townRoot, "daemon", "daemon.log"),
		stateDir: filepath.Join(constants.TownRuntimePath(townRoot), "tier-sweep"),
		rigs:     func() ([]string, error) { return knownRigNames(townRoot) },
		last:     map[string]tierSweepEvent{},
		start:    map[string]tierSweepStarted{},
	}
}

// knownRigs is the set of rig names the town's registry holds. A registry that
// cannot be read names nothing, so no log-derived rig reaches a path.
func (r *tierSweepReader) knownRigs() map[string]bool {
	if r.rigs == nil {
		return nil
	}
	names, err := r.rigs()
	if err != nil {
		return nil
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// read is the panel's whole reading. A log that cannot be read at all says so;
// a log that holds no sweeps is an empty list, which is a different thing.
func (r *tierSweepReader) read(now time.Time) *dashboard.TierSweep {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.scanLog(); err != nil && !r.readOK {
		return &dashboard.TierSweep{Unavailable: true}
	}
	trends, trendsErr := r.trends()
	ts := &dashboard.TierSweep{Sweeps: r.recent(), Trends: trends, TrendsUnavailable: trendsErr != nil}
	if run := r.running(now); run != nil {
		ts.Running = run
	}
	return ts
}

// scanLog reads what the daemon log gained since the last scan. A log that
// shrank was rotated: it is read from the top, and the sweeps already read stay
// read.
func (r *tierSweepReader) scanLog() error {
	f, err := os.Open(r.logPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r.readOK = true
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < r.offset {
		r.offset = 0
	}
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			// A last line with no newline yet is the writer's, mid-line: the
			// next scan reads it whole.
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		r.offset += int64(len(line))
		if !strings.Contains(line, "tier_sweep: ") {
			continue
		}
		r.parseLogLine(strings.TrimRight(line, "\r\n"))
	}
}

func (r *tierSweepReader) parseLogLine(line string) {
	if m := tierSweepStartedRe.FindStringSubmatch(line); m != nil {
		at, err := time.ParseInLocation(omLogTimeLayout, m[1], time.Local)
		if err != nil {
			return
		}
		r.start[m[2]] = tierSweepStarted{at: at, sha: m[3], tiers: splitTierSweepTiers(m[4])}
		// A new cycle starts its walls from nothing: stage lines the previous
		// cycle left behind (an abort the log lost) must not be charted here.
		delete(r.walls, m[2])
		return
	}
	if m := tierSweepAbortedRe.FindStringSubmatch(line); m != nil {
		// The cycle is over and swept nothing: the rig is idle, not running
		// until tierSweepStaleAfter expires. Only the cycle the line names
		// closes: an aborted line for another sha belongs to a cycle already
		// replaced, and must not end the one that is running.
		if open, ok := r.start[m[2]]; ok && open.sha == m[3] {
			delete(r.start, m[2])
			delete(r.walls, m[2])
		}
		return
	}
	if m := tierSweepSweptRe.FindStringSubmatch(line); m != nil {
		at, err := time.ParseInLocation(omLogTimeLayout, m[1], time.Local)
		if err != nil {
			return
		}
		row := dashboard.TierSweepRow{At: at, Rig: m[2], SHA: m[3], Stages: parseTierSweepVerdicts(m[4]), Walls: copyWalls(r.walls[m[2]])}
		if m[5] != "" {
			if d, err := time.ParseDuration(m[5]); err == nil {
				s := d.Seconds()
				row.Secs = &s
			}
		}
		r.keep(row)
		r.last[m[2]] = tierSweepEvent{at: at, swept: true}
		delete(r.start, m[2])
		delete(r.walls, m[2])
		return
	}
	if m := tierSweepStageRe.FindStringSubmatch(line); m != nil {
		at, err := time.ParseInLocation(omLogTimeLayout, m[1], time.Local)
		if err != nil {
			return
		}
		r.last[m[2]] = tierSweepEvent{at: at, stage: m[3]}
		if d, err := time.ParseDuration(m[4]); err == nil {
			if r.walls == nil {
				r.walls = map[string]map[string]float64{}
			}
			w := r.walls[m[2]]
			if w == nil {
				w = map[string]float64{}
				r.walls[m[2]] = w
			}
			w[m[3]] = d.Seconds()
		}
	}
}

// keep files a finished sweep in the panel's town-wide window and in its rig's
// own, each capped to what it draws. The struct's maps are made here so a
// reader built as a bare literal — a test's — reads a fresh log without setup.
func (r *tierSweepReader) keep(row dashboard.TierSweepRow) {
	r.rows = append(r.rows, row)
	if len(r.rows) > tierSweepRowsKept {
		r.rows = append([]dashboard.TierSweepRow(nil), r.rows[len(r.rows)-tierSweepRowsKept:]...)
	}
	if r.byRig == nil {
		r.byRig = map[string][]dashboard.TierSweepRow{}
	}
	rig := r.byRig[row.Rig]
	rig = append(rig, row)
	if len(rig) > tierSweepTrendPerRig {
		rig = append([]dashboard.TierSweepRow(nil), rig[len(rig)-tierSweepTrendPerRig:]...)
	}
	r.byRig[row.Rig] = rig
}

// tierSweepTrendRead is the seam a rig's wall trend reads: the sweeps kept for
// one rig, oldest first.
type tierSweepTrendRead func(rig string) []dashboard.TierSweepRow

// buildTierSweepTrends folds the kept sweeps into one series per rig, oldest
// first and at most perRig points each. A rig in names with no sweeps keeps an
// empty series so the page can say so, and a name listed twice is read once.
func buildTierSweepTrends(names []string, read tierSweepTrendRead, perRig int) []dashboard.TierSweepTrendRig {
	if perRig <= 0 || read == nil {
		return nil
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	seen := make(map[string]bool, len(sorted))
	var out []dashboard.TierSweepTrendRig
	for _, rig := range sorted {
		if rig == "" || seen[rig] {
			continue
		}
		seen[rig] = true
		rows := read(rig)
		if len(rows) > perRig {
			rows = rows[len(rows)-perRig:]
		}
		tr := dashboard.TierSweepTrendRig{Rig: rig, Sweeps: make([]dashboard.TierSweepTrendPoint, 0, len(rows))}
		for _, row := range rows {
			tr.Sweeps = append(tr.Sweeps, dashboard.TierSweepTrendPoint{At: row.At, SHA: row.SHA, Walls: row.Walls, TotalSecs: row.Secs})
		}
		out = append(out, tr)
	}
	return out
}

// trends is one wall trend per rig over the sweeps the scan has kept. The town's
// registry names the rigs, so one that has not swept yet still gets an empty
// trend; a rig the log named is kept even if the registry lost it, because its
// sweeps were what the log recorded. A registry that cannot be read is returned
// as an error, not as a shorter list: the caller must say the list is partial.
func (r *tierSweepReader) trends() ([]dashboard.TierSweepTrendRig, error) {
	var names []string
	var regErr error
	if r.rigs != nil {
		got, err := r.rigs()
		if err != nil {
			regErr = err
		}
		names = append(names, got...)
	}
	for rig := range r.byRig {
		names = append(names, rig)
	}
	return buildTierSweepTrends(names, func(rig string) []dashboard.TierSweepRow { return r.byRig[rig] }, tierSweepTrendPerRig), regErr
}

// copyWalls returns a copy of one sweep's tier walls. The row keeps its own map,
// so no later write to the scan's map can change a sweep already kept.
func copyWalls(w map[string]float64) map[string]float64 {
	if w == nil {
		return nil
	}
	out := make(map[string]float64, len(w))
	for k, v := range w {
		out[k] = v
	}
	return out
}

// splitTierSweepTiers reads the "(shell, integration, race)" part of a "sweep
// started" line into the tiers it names, in the order the cycle runs them.
func splitTierSweepTiers(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// tierSweepUnderLoadWord opens the " under load" annotation the daemon appends
// to a stage it started beside a busy CI runner (tierSweepVerdictSummary).
const tierSweepUnderLoadWord = "under"

// parseTierSweepVerdicts reads the "(shell GREEN, integration GREEN)" part of a
// swept line. An entry the line does not name is left out, which is how the
// panel tells a tier that did not run from one that passed. Only the field
// after the tier name is the verdict: the daemon annotates a stage it started
// beside a busy CI runner, "integration GREEN under load" (gt-5ejux), and the
// panel still has to read that as GREEN. A tier marked loaded whose run did not
// happen carries the annotation and no verdict ("integration  under load"), so
// the field after its name is the annotation's own "under": no verdict, and the
// entry is left out like any other tier the sweep never ran.
func parseTierSweepVerdicts(s string) []dashboard.TierSweepStage {
	var out []dashboard.TierSweepStage
	for _, part := range strings.Split(s, ",") {
		fields := strings.Fields(part)
		if len(fields) < 2 || fields[1] == tierSweepUnderLoadWord {
			continue
		}
		out = append(out, dashboard.TierSweepStage{Tier: fields[0], Verdict: fields[1]})
	}
	return out
}

// running is the sweep at least one rig is in flight in now. A cycle the log
// opened with a "sweep started" line and no swept line has closed is running by
// definition: the line names the tiers the cycle will run, and its timestamp is
// the cycle's start (gt-rntre). A rig with no open start is read the old way,
// for a log from a daemon that logs none: the shell stage's finished line is
// the sign, a cycle whose integration stage (which carries race) is running in
// the silence that follows.
func (r *tierSweepReader) running(now time.Time) *dashboard.TierSweepRun {
	var best *dashboard.TierSweepRun
	consider := func(run *dashboard.TierSweepRun) {
		if best == nil || run.Since.After(best.Since) {
			best = run
		}
	}
	live := func(at time.Time) (int64, bool) {
		d := now.Sub(at)
		if d < 0 || d > tierSweepStaleAfter {
			return 0, false
		}
		return int64(d / time.Second), true
	}

	for rig, s := range r.start {
		elapsed, ok := live(s.at)
		if !ok {
			continue
		}
		run := &dashboard.TierSweepRun{Rig: rig, Tiers: s.tiers, Since: s.at, ElapsedSec: elapsed}
		if len(s.tiers) > 0 {
			run.Tier = s.tiers[0]
		}
		consider(run)
	}
	for rig, ev := range r.last {
		if ev.swept || ev.stage != "shell" {
			continue
		}
		if _, open := r.start[rig]; open {
			// The cycle's own start line says better than this inference does.
			continue
		}
		elapsed, ok := live(ev.at)
		if !ok {
			continue
		}
		consider(&dashboard.TierSweepRun{Rig: rig, Tier: "integration", Since: ev.at, ElapsedSec: elapsed})
	}
	return best
}

// recent is the newest sweeps, newest first, each RED tier given the failing
// units its rig's record still holds. The record keeps only the rig's last run,
// so its names are used only for the row whose sha it carries. A tier a cycle
// carried forward from an earlier tip is in the record but not in the row: the
// swept line names a tier that did not run with no verdict and the parser drops
// it, so no stage can pick up a red nobody re-ran (gt-euxf1).
func (r *tierSweepReader) recent() []dashboard.TierSweepRow {
	rows := append([]dashboard.TierSweepRow(nil), r.rows...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.After(rows[j].At) })
	if len(rows) > tierSweepRows {
		rows = rows[:tierSweepRows]
	}
	known := r.knownRigs()
	for i := range rows {
		names := r.recordFailed(rows[i].Rig, rows[i].SHA, known)
		for j := range rows[i].Stages {
			if rows[i].Stages[j].Verdict == "RED" {
				rows[i].Stages[j].Failed = names[rows[i].Stages[j].Tier]
			}
		}
	}
	return rows
}

// tierSweepRecord mirrors the fields the daemon writes to
// .runtime/tier-sweep/<rig>.json that name a failing unit.
type tierSweepRecord struct {
	LastSHA string                   `json:"last_sha"`
	Tiers   map[string]tierSweepTier `json:"tiers"`
}

type tierSweepTier struct {
	FailedNames []string `json:"failed_names"`
}

// recordFailed is one rig's failing units by tier. A record that cannot be
// read, or whose sha is another sweep's, says nothing: the verdict is the
// log's, and the names are only ever a bonus. The rig comes from that same log
// line, so it becomes a path only when the registry names it: a name the
// registry does not hold — including one carrying a path separator — never
// reaches outside the state directory (gt-2czgm).
func (r *tierSweepReader) recordFailed(rig, sha string, known map[string]bool) map[string][]string {
	if rig == "" || sha == "" || !known[rig] {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(r.stateDir, rig+".json"))
	if err != nil {
		return nil
	}
	var rec tierSweepRecord
	if json.Unmarshal(b, &rec) != nil || rec.LastSHA == "" {
		return nil
	}
	if !tierSweepSameSHA(rec.LastSHA, sha) {
		return nil
	}
	out := map[string][]string{}
	for tier, t := range rec.Tiers {
		if len(t.FailedNames) == 0 {
			continue
		}
		out[tier] = t.FailedNames
	}
	return out
}

// tierSweepSameSHA reports whether two shas name the same commit, allowing
// either to carry the other's prefix: the log line and the record each hold the
// abbreviated form.
func tierSweepSameSHA(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}
