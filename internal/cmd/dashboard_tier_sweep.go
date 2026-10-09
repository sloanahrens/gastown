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
	// tierSweepStaleAfter is when a stage-finished line stops meaning a sweep is
	// in flight. A cycle's longest stage is the integration one, bounded by the
	// daemon's 90m budget: a rig whose last line is older ran through a daemon
	// that died mid-cycle.
	tierSweepStaleAfter = 90 * time.Minute
)

var (
	// tierSweepStageRe matches the daemon's line for a stage that finished:
	// "tier_sweep: gastown: shell finished (exit 0) in 1m5s; last lines:".
	tierSweepStageRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) tier_sweep: (\S+): (\S+) finished \(exit -?\d+\) in \S+; last lines:$`)
	// tierSweepSweptRe matches the line closing a cycle, whose trailing " in
	// <dur>" is absent on a cycle that ran before gt-iqzr0:
	// "tier_sweep: gastown: swept a9be03e4 (shell GREEN, integration GREEN) in 9m36s".
	tierSweepSweptRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) tier_sweep: (\S+): swept (\S+) \(([^)]*)\)(?: in (\S+))?\s*$`)
	// tierSweepStartedRe matches the line opening a cycle, which names the
	// tiers the cycle will run (gt-rntre):
	// "tier_sweep: gastown: sweep started a9be03e4 (shell, integration, race)".
	tierSweepStartedRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) tier_sweep: (\S+): sweep started (\S+) \(([^)]*)\)\s*$`)
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
	rows  []dashboard.TierSweepRow
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
	ts := &dashboard.TierSweep{Sweeps: r.recent()}
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
		r.start[m[2]] = tierSweepStarted{at: at, tiers: splitTierSweepTiers(m[4])}
		return
	}
	if m := tierSweepSweptRe.FindStringSubmatch(line); m != nil {
		at, err := time.ParseInLocation(omLogTimeLayout, m[1], time.Local)
		if err != nil {
			return
		}
		row := dashboard.TierSweepRow{At: at, Rig: m[2], SHA: m[3], Stages: parseTierSweepVerdicts(m[4])}
		if m[5] != "" {
			if d, err := time.ParseDuration(m[5]); err == nil {
				s := d.Seconds()
				row.Secs = &s
			}
		}
		r.rows = append(r.rows, row)
		if len(r.rows) > tierSweepRowsKept {
			r.rows = append([]dashboard.TierSweepRow(nil), r.rows[len(r.rows)-tierSweepRowsKept:]...)
		}
		r.last[m[2]] = tierSweepEvent{at: at, swept: true}
		delete(r.start, m[2])
		return
	}
	if m := tierSweepStageRe.FindStringSubmatch(line); m != nil {
		at, err := time.ParseInLocation(omLogTimeLayout, m[1], time.Local)
		if err != nil {
			return
		}
		r.last[m[2]] = tierSweepEvent{at: at, stage: m[3]}
	}
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

// parseTierSweepVerdicts reads the "(shell GREEN, integration GREEN)" part of a
// swept line. An entry the line does not name is left out, which is how the
// panel tells a tier that did not run from one that passed. Only the field
// after the tier name is the verdict: the daemon annotates a stage it started
// beside a busy CI runner, "integration GREEN under load" (gt-5ejux), and the
// panel still has to read that as GREEN.
func parseTierSweepVerdicts(s string) []dashboard.TierSweepStage {
	var out []dashboard.TierSweepStage
	for _, part := range strings.Split(s, ",") {
		fields := strings.Fields(part)
		if len(fields) < 2 {
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
// so its names are used only for the row whose sha it carries.
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
	if !strings.HasPrefix(rec.LastSHA, sha) && !strings.HasPrefix(sha, rec.LastSHA) {
		return nil
	}
	out := map[string][]string{}
	for tier, t := range rec.Tiers {
		if len(t.FailedNames) > 0 {
			out[tier] = t.FailedNames
		}
	}
	return out
}
