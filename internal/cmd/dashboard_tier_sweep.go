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
)

// tierSweepEvent is the newest thing the log said about one rig: a stage that
// finished, or a whole cycle that swept. Which one it is decides whether a sweep
// is in flight for that rig.
type tierSweepEvent struct {
	at    time.Time
	stage string // the stage that finished, for a stage line
	swept bool   // the line closed a cycle
}

// tierSweepReader keeps the daemon-log scan between polls.
type tierSweepReader struct {
	logPath  string
	stateDir string

	mu     sync.Mutex
	offset int64
	readOK bool
	last   map[string]tierSweepEvent
	rows   []dashboard.TierSweepRow
}

func newTierSweepReader(townRoot string) *tierSweepReader {
	return &tierSweepReader{
		logPath:  filepath.Join(townRoot, "daemon", "daemon.log"),
		stateDir: filepath.Join(constants.TownRuntimePath(townRoot), "tier-sweep"),
		last:     map[string]tierSweepEvent{},
	}
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

// parseTierSweepVerdicts reads the "(shell GREEN, integration GREEN)" part of a
// swept line. An entry the line does not name is left out, which is how the
// panel tells a tier that did not run from one that passed.
func parseTierSweepVerdicts(s string) []dashboard.TierSweepStage {
	var out []dashboard.TierSweepStage
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tier, verdict, ok := strings.Cut(part, " ")
		if !ok || tier == "" || verdict == "" {
			continue
		}
		out = append(out, dashboard.TierSweepStage{Tier: tier, Verdict: verdict})
	}
	return out
}

// running is the stage a sweep is at now. The daemon logs no cycle start, so a
// sweep reads as in flight when a rig's newest line is the shell stage
// finishing: the script's second stage, integration (which carries race), is
// running in the silence that follows and has not yet been closed by a swept
// line. A finished integration stage means the cycle is over.
func (r *tierSweepReader) running(now time.Time) *dashboard.TierSweepRun {
	var best *dashboard.TierSweepRun
	for rig, ev := range r.last {
		if ev.swept || ev.stage != "shell" {
			continue
		}
		d := now.Sub(ev.at)
		if d < 0 || d > tierSweepStaleAfter {
			continue
		}
		run := &dashboard.TierSweepRun{Rig: rig, Tier: "integration", Since: ev.at, ElapsedSec: int64(d / time.Second)}
		if best == nil || run.Since.After(best.Since) {
			best = run
		}
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
	for i := range rows {
		names := r.recordFailed(rows[i].Rig, rows[i].SHA)
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
// log's, and the names are only ever a bonus.
func (r *tierSweepReader) recordFailed(rig, sha string) map[string][]string {
	if rig == "" || sha == "" {
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
