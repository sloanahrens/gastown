package townstate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/land"
)

// The day-one thresholds. Each is named so the table the check prints says what
// a row was held to, and a change to one is visible in review.
const (
	// GitSpeedRuns is how many times each git command is timed in each rig. The
	// median of the runs is the measurement.
	GitSpeedRuns = 3
	// GitSpeedMaxFactor fails a median more than this many times its baseline.
	GitSpeedMaxFactor = 2.0
	// GitSpeedMaxSeconds fails a median over this many seconds, whatever the
	// baseline says.
	GitSpeedMaxSeconds = 3.0
	// GitCommandBound is the most one timed git run may take before it is
	// abandoned and the row reports that it did not finish.
	GitCommandBound = 60 * time.Second
	// DaemonProbeBound is how long gt status --line may take to answer the
	// daemon's health probe, and the ceiling its run is abandoned at.
	DaemonProbeBound = 5 * time.Second
	// StatusLineBound is how long gt status --line may take to exit 0.
	StatusLineBound = 2 * time.Second
	// DaemonLogWindow is how long after the first daemon start since the
	// cut-over the daemon log is read for starts and panics.
	DaemonLogWindow = 5 * time.Minute
	// DaemonStartsInWindow is the most "Daemon starting" lines the window may
	// hold. One more than this is a restart loop.
	DaemonStartsInWindow = 1
)

// Paths under the town root the checks read and write. The daemon's state
// directory moves with the town-state volume, so the baseline the checks compare
// against is carried by the copy.
const (
	// CutoverRecordRel is the cut-over record `migrate copy --cutover` writes. The
	// migrate command writes it and the day-one checks read it, from this one
	// definition.
	CutoverRecordRel = "daemon/town-state-cutover.json"
	gitBaselineRel   = "daemon/town-state-git-baseline.json"
	daemonLogRel     = "daemon/daemon.log"
)

const (
	// daemonLogLayout is the timestamp the daemon's logger writes with
	// log.LstdFlags, in local time.
	daemonLogLayout = "2006/01/02 15:04:05"
	// daemonStartText is the line the daemon writes each time it starts.
	daemonStartText = "Daemon starting"
	// daemonPanicText is the prefix of a Go panic the daemon's log carries.
	daemonPanicText = "panic:"
)

// gitSpeedCommands are the two git commands timed in each rig. Both run in the
// rig's mayor/rig clone: git status needs a work tree, and the bare repository
// has none. Both are read-only, since install-gt fetches in mayor/rig and a
// concurrent fetch breaks its ff-only; ls-remote makes the fetch's round trip
// to the remote without writing.
var gitSpeedCommands = [][]string{{"status", "--short"}, {"ls-remote", "--heads", "origin"}}

// Env is what the day-one checks read and run through. Run is the only way a
// check reaches a process and Now the only clock it times with, so a test drives
// both with fakes. GT is the gt command the status probe runs.
type Env struct {
	Town string
	GT   string
	Run  func(ctx context.Context, dir, name string, args ...string) (out string, exit int, err error)
	Now  func() time.Time
}

// Row is one line of the day-one table: the check, what it measured, the
// threshold it was held to, and the result. A check that could not run has a
// Result of "not run: <reason>" and OK false, so it counts as a failure.
type Row struct {
	Check     string `json:"check"`
	Measured  string `json:"measured"`
	Threshold string `json:"threshold"`
	Result    string `json:"result"`
	OK        bool   `json:"ok"`
}

// passed is a row whose check held.
func passed(check, measured, threshold string) Row {
	return Row{Check: check, Measured: measured, Threshold: threshold, Result: "pass", OK: true}
}

// failed is a row whose check was measured and did not hold.
func failed(check, measured, threshold, reason string) Row {
	return Row{Check: check, Measured: measured, Threshold: threshold, Result: "fail: " + reason}
}

// notRun is a row whose check could not be measured. It counts as a failure.
func notRun(check, threshold string, err error) Row {
	return Row{Check: check, Measured: "-", Threshold: threshold, Result: "not run: " + err.Error()}
}

// DayOneFailures counts the rows that did not pass. A row that could not run is
// one of them.
func DayOneFailures(rows []Row) int {
	n := 0
	for _, r := range rows {
		if !r.OK {
			n++
		}
	}
	return n
}

// DayOneChecks runs the day-one checks in the order the table prints them: git
// speed, then the daemon and gt status rows, then the first landing since the
// cut-over.
func DayOneChecks(ctx context.Context, env Env) []Row {
	rows := CheckGitSpeed(ctx, env)
	rows = append(rows, CheckDaemonStart(ctx, env)...)
	return append(rows, CheckLandingSinceCutover(env))
}

// probe is one command's answer: its output, exit status, how long it took, and
// whether it was abandoned at its bound (or took longer than the bound).
type probe struct {
	out      string
	exit     int
	took     time.Duration
	timedOut bool
}

// runBounded runs one command under a deadline of bound. It reports the command
// as timed out when the deadline passed or the run took longer than bound, so a
// slow answer is never read as a timely one.
func runBounded(ctx context.Context, env Env, bound time.Duration, dir, name string, args ...string) (probe, error) {
	bctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	start := env.Now()
	out, exit, err := env.Run(bctx, dir, name, args...)
	took := env.Now().Sub(start)
	return probe{out: out, exit: exit, took: took, timedOut: bctx.Err() != nil || took > bound}, err
}

// CheckGitSpeed times each git command in each rig the landing worker reads and
// holds the median to the baseline the volume move was recorded against. A rig
// the baseline does not name fails; a missing baseline fails with the instruction
// to record it. A town with no rig to time fails too: no rows would read as a
// clean pass.
func CheckGitSpeed(ctx context.Context, env Env) []Row {
	rigs, unregistered, err := landingRigs(env.Town)
	if err != nil {
		return []Row{notRun("git speed", "rig list readable", err)}
	}
	rows := unregisteredRows(unregistered)
	if len(rigs) == 0 {
		return append(rows, failed("git speed", "-", "a rig to time", "git speed not measured: no rig to time; the rig registry names no rig, or the landing worker's rig list names none of them"))
	}
	gb, err := loadGitBaseline(env.Town)
	if errors.Is(err, fs.ErrNotExist) {
		return append(rows, failed("git speed baseline", "-", "a baseline recorded before the move", fmt.Sprintf("no baseline at %s; record it with gt town state check --record-baseline before the move", gitBaselineRel)))
	}
	if err != nil {
		return append(rows, notRun("git speed baseline", "a baseline recorded before the move", err))
	}
	for _, rig := range rigs {
		repo := constants.RigMayorPath(filepath.Join(env.Town, rig))
		for _, args := range gitSpeedCommands {
			cmdText := strings.Join(args, " ")
			check := fmt.Sprintf("git speed %s: %s", rig, cmdText)
			m, err := timeGit(ctx, env, repo, args)
			if err != nil {
				rows = append(rows, notRun(check, "median of runs within the baseline", err))
				continue
			}
			base, ok := gb.Rigs[rig][cmdText]
			threshold := fmt.Sprintf("<= %.1fx baseline and <= %.1fs", GitSpeedMaxFactor, GitSpeedMaxSeconds)
			measured := fmt.Sprintf("%.2fs median of %d", m, GitSpeedRuns)
			switch {
			case !ok:
				rows = append(rows, failed(check, measured, threshold, fmt.Sprintf("the baseline names no %s run for rig %s; record it with gt town state check --record-baseline before the move", cmdText, rig)))
			case m > GitSpeedMaxFactor*base:
				rows = append(rows, failed(check, measured, threshold, fmt.Sprintf("rig %s takes %.2fs, over %.1fx its baseline of %.2fs", rig, m, GitSpeedMaxFactor, base)))
			case m > GitSpeedMaxSeconds:
				rows = append(rows, failed(check, measured, threshold, fmt.Sprintf("rig %s takes %.2fs, over %.1fs", rig, m, GitSpeedMaxSeconds)))
			default:
				rows = append(rows, passed(check, measured, threshold))
			}
		}
	}
	return rows
}

// unregisteredRows fails the git speed check for each rig name the landing
// worker's list gives that the rig registry does not hold. The daemon skips such
// a name, so the day-one check says so instead.
func unregisteredRows(names []string) []Row {
	var rows []Row
	for _, name := range names {
		rows = append(rows, failed("landing worker rig list: "+name, "-", "every rig it names is registered", fmt.Sprintf("the landing worker's rig list (mayor/daemon.json patrols.landing_worker.rigs) names %q, which the rig registry does not hold; the landing worker skips it", name)))
	}
	return rows
}

// timeGit times args in dir GitSpeedRuns times and returns the median in
// seconds. A run that does not finish, cannot start or exits non-zero is an
// error naming the command and the directory. It never carries the command's
// output, which a repository can fill with anything.
func timeGit(ctx context.Context, env Env, dir string, args []string) (float64, error) {
	cmdText := "git " + strings.Join(args, " ")
	secs := make([]float64, 0, GitSpeedRuns)
	for range GitSpeedRuns {
		p, err := runBounded(ctx, env, GitCommandBound, dir, "git", args...)
		switch {
		case p.timedOut:
			return 0, fmt.Errorf("%s in %s did not finish within %s", cmdText, dir, GitCommandBound)
		case err != nil:
			return 0, fmt.Errorf("%s in %s: %w", cmdText, dir, err)
		case p.exit != 0:
			return 0, fmt.Errorf("%s in %s exited %d", cmdText, dir, p.exit)
		}
		secs = append(secs, p.took.Seconds())
	}
	return median(secs), nil
}

// median returns the middle of xs, or the mean of the two middle values when
// there is an even count. Callers pass at least one value.
func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// GitBaseline is the git speed the volume move is judged against: the median, in
// seconds, of GitSpeedRuns runs of each git command in each rig, taken from the
// bind mount before the move. Rigs maps rig name to command text to seconds.
type GitBaseline struct {
	RecordedAt time.Time                     `json:"recorded_at"`
	Rigs       map[string]map[string]float64 `json:"rigs"`
}

// RecordGitBaseline times each git command in each rig the landing worker reads,
// and writes the medians to the baseline file. It refuses to replace a baseline
// unless force is set: a baseline taken after the move would hide a regression.
// It returns the file's path.
func RecordGitBaseline(ctx context.Context, env Env, force bool) (string, error) {
	path := filepath.Join(env.Town, filepath.FromSlash(gitBaselineRel))
	if !force {
		_, err := os.Stat(path)
		switch {
		case err == nil:
			return path, fmt.Errorf("a git speed baseline is already recorded at %s; a baseline taken after the move would hide a regression, so replace it only with --force", gitBaselineRel)
		case !errors.Is(err, fs.ErrNotExist):
			return path, fmt.Errorf("checking %s: %w", gitBaselineRel, err)
		}
	}
	rigs, unregistered, err := landingRigs(env.Town)
	if err != nil {
		return path, fmt.Errorf("recording the git speed baseline: %w", err)
	}
	if len(unregistered) > 0 {
		return path, fmt.Errorf("recording the git speed baseline: the landing worker's rig list (mayor/daemon.json patrols.landing_worker.rigs) names %q, which the rig registry does not hold", unregistered)
	}
	if len(rigs) == 0 {
		return path, errors.New("recording the git speed baseline: the town has no rigs to time")
	}
	gb := GitBaseline{RecordedAt: env.Now().UTC(), Rigs: make(map[string]map[string]float64, len(rigs))}
	for _, rig := range rigs {
		repo := constants.RigMayorPath(filepath.Join(env.Town, rig))
		gb.Rigs[rig] = make(map[string]float64, len(gitSpeedCommands))
		for _, args := range gitSpeedCommands {
			m, err := timeGit(ctx, env, repo, args)
			if err != nil {
				return path, fmt.Errorf("recording the git speed baseline for rig %s: %w", rig, err)
			}
			gb.Rigs[rig][strings.Join(args, " ")] = m
		}
	}
	if err := writeGitBaseline(path, gb); err != nil {
		return path, err
	}
	return path, nil
}

// writeGitBaseline writes gb to path by a temporary sibling and a rename, so a
// reader never sees half a baseline.
func writeGitBaseline(path string, gb GitBaseline) error {
	data, err := json.MarshalIndent(gb, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", gitBaselineRel, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the daemon state directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", gitBaselineRel, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", gitBaselineRel, err), os.Remove(tmp))
	}
	return nil
}

// loadGitBaseline reads the baseline file. A missing file wraps fs.ErrNotExist.
func loadGitBaseline(town string) (GitBaseline, error) {
	var gb GitBaseline
	data, err := os.ReadFile(filepath.Join(town, filepath.FromSlash(gitBaselineRel)))
	if err != nil {
		return gb, fmt.Errorf("reading %s: %w", gitBaselineRel, err)
	}
	if err := json.Unmarshal(data, &gb); err != nil {
		return gb, fmt.Errorf("reading %s: %w", gitBaselineRel, err)
	}
	return gb, nil
}

// CheckDaemonStart asks the daemon's health probe for the town line once, and
// reads the daemon log from the cut-over. The probe is the first line of
// gt status --line, which the update cycle reads as the health probe: it must
// start with GREEN. The exit status and the time of the same run answer the
// gt status --line row.
func CheckDaemonStart(ctx context.Context, env Env) []Row {
	const (
		healthCheck = "daemon health probe (gt status --line)"
		statusCheck = "gt status --line"
	)
	healthThreshold := fmt.Sprintf("GREEN within %s", DaemonProbeBound)
	statusThreshold := fmt.Sprintf("exit 0 within %s", StatusLineBound)

	p, err := runBounded(ctx, env, DaemonProbeBound, "", env.GT, "status", "--line")
	var rows []Row
	switch {
	case p.timedOut:
		rows = append(rows,
			failed(healthCheck, "no answer", healthThreshold, fmt.Sprintf("no answer within %s", DaemonProbeBound)),
			failed(statusCheck, "no answer", statusThreshold, fmt.Sprintf("no answer within %s", DaemonProbeBound)))
	case err != nil:
		rows = append(rows,
			notRun(healthCheck, healthThreshold, err),
			notRun(statusCheck, statusThreshold, err))
	default:
		line, _, _ := strings.Cut(p.out, "\n")
		line = strings.TrimSpace(line)
		measured := fmt.Sprintf("%s in %.2fs", strconv.Quote(line), p.took.Seconds())
		if strings.HasPrefix(strings.ToLower(line), "green") {
			rows = append(rows, passed(healthCheck, measured, healthThreshold))
		} else {
			rows = append(rows, failed(healthCheck, measured, healthThreshold, fmt.Sprintf("the town line is not GREEN: %q", line)))
		}
		statusMeasured := fmt.Sprintf("exit %d in %.2fs", p.exit, p.took.Seconds())
		switch {
		case p.exit != 0:
			rows = append(rows, failed(statusCheck, statusMeasured, statusThreshold, fmt.Sprintf("exit %d", p.exit)))
		case p.took > StatusLineBound:
			rows = append(rows, failed(statusCheck, statusMeasured, statusThreshold, fmt.Sprintf("took %.2fs, over %s", p.took.Seconds(), StatusLineBound)))
		default:
			rows = append(rows, passed(statusCheck, statusMeasured, statusThreshold))
		}
	}
	return append(rows, daemonLogRows(env)...)
}

// daemonLogRows reads the daemon log from the first start at or after the
// cut-over: the starts in the following DaemonLogWindow, and any panic in it.
func daemonLogRows(env Env) []Row {
	const (
		startsCheck = "daemon log: starts in the window"
		panicCheck  = "daemon log: panics in the window"
	)
	startsThreshold := fmt.Sprintf("<= %d start in %s after the cut-over", DaemonStartsInWindow, DaemonLogWindow)
	panicThreshold := fmt.Sprintf("no panic in %s after the cut-over", DaemonLogWindow)

	cut, err := readCutover(env.Town)
	if err != nil {
		return []Row{notRun(startsCheck, startsThreshold, err), notRun(panicCheck, panicThreshold, err)}
	}
	lines, err := readDaemonLog(env.Town)
	if err != nil {
		return []Row{notRun(startsCheck, startsThreshold, err), notRun(panicCheck, panicThreshold, err)}
	}
	anchor, ok := firstStartAtOrAfter(lines, cut)
	if !ok {
		reason := fmt.Sprintf("no daemon start since the cut-over at %s", cut.Format(time.RFC3339))
		return []Row{failed(startsCheck, "0 starts", startsThreshold, reason), failed(panicCheck, "-", panicThreshold, reason)}
	}
	end := anchor.Add(DaemonLogWindow)
	starts, panics := 0, 0
	for _, l := range lines {
		if l.at.Before(anchor) || l.at.After(end) {
			continue
		}
		if strings.Contains(l.text, daemonStartText) {
			starts++
		}
		if strings.Contains(l.text, daemonPanicText) {
			panics++
		}
	}
	startsMeasured := fmt.Sprintf("%d start(s) from %s", starts, anchor.Format(time.RFC3339))
	panicMeasured := fmt.Sprintf("%d panic line(s)", panics)
	var startsRow, panicRow Row
	if starts > DaemonStartsInWindow {
		startsRow = failed(startsCheck, startsMeasured, startsThreshold, fmt.Sprintf("%d starts in %s: a restart loop", starts, DaemonLogWindow))
	} else {
		startsRow = passed(startsCheck, startsMeasured, startsThreshold)
	}
	if panics > 0 {
		panicRow = failed(panicCheck, panicMeasured, panicThreshold, fmt.Sprintf("%d panic line(s) in the window", panics))
	} else {
		panicRow = passed(panicCheck, panicMeasured, panicThreshold)
	}
	return []Row{startsRow, panicRow}
}

// logLine is one daemon log line with the time its record was written. A line
// the logger did not stamp (a continuation of a panic trace) takes the time of
// the line before it.
type logLine struct {
	at   time.Time
	text string
}

// readDaemonLog reads the daemon log. A missing log wraps fs.ErrNotExist.
func readDaemonLog(town string) ([]logLine, error) {
	f, err := os.Open(filepath.Join(town, filepath.FromSlash(daemonLogRel)))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", daemonLogRel, err)
	}
	defer func() { _ = f.Close() }()
	var lines []logLine
	var at time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		text := sc.Text()
		if len(text) >= len(daemonLogLayout) {
			if t, err := time.ParseInLocation(daemonLogLayout, text[:len(daemonLogLayout)], time.Local); err == nil {
				at = t
				text = strings.TrimPrefix(text[len(daemonLogLayout):], " ")
			}
		}
		if at.IsZero() {
			continue
		}
		lines = append(lines, logLine{at: at, text: text})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", daemonLogRel, err)
	}
	return lines, nil
}

// firstStartAtOrAfter is the time of the first daemon start line at or after cut.
func firstStartAtOrAfter(lines []logLine, cut time.Time) (time.Time, bool) {
	for _, l := range lines {
		if !l.at.Before(cut) && strings.Contains(l.text, daemonStartText) {
			return l.at, true
		}
	}
	return time.Time{}, false
}

// readCutover reads the cut-over time from the cut-over record. An absent
// record means the cut-over has not run, which is a reason, not a zero time.
func readCutover(town string) (time.Time, error) {
	data, err := os.ReadFile(filepath.Join(town, filepath.FromSlash(CutoverRecordRel)))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, fmt.Errorf("no cut-over record at %s, so the cut-over has not run", CutoverRecordRel)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading %s: %w", CutoverRecordRel, err)
	}
	var rec struct {
		CutoverAt string `json:"cutover_at"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return time.Time{}, fmt.Errorf("reading %s: %w", CutoverRecordRel, err)
	}
	at, err := time.Parse(time.RFC3339, rec.CutoverAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading %s: cutover_at is not RFC 3339: %w", CutoverRecordRel, err)
	}
	return at, nil
}

// CheckLandingSinceCutover reads the landing records through the reader the
// landing worker uses, and holds the newest landing since the cut-over to a
// recorded gate result and a merged commit.
func CheckLandingSinceCutover(env Env) Row {
	const (
		check     = "first landing since the cut-over"
		threshold = "a landing after the cut-over with gate_result and landed_commit"
	)
	cut, err := readCutover(env.Town)
	if err != nil {
		return notRun(check, threshold, err)
	}
	// The git speed check reports the rig names the registry does not hold, so
	// this check reads the registered rigs only.
	rigs, _, err := landingRigs(env.Town)
	if err != nil {
		return notRun(check, threshold, err)
	}
	var newest land.LandingRecord
	found := false
	for _, rig := range rigs {
		f, err := land.RigLandingsFile(env.Town, rig)
		if err != nil {
			return notRun(check, threshold, err)
		}
		recs, err := f.Since(cut)
		if err != nil {
			return notRun(check, threshold, fmt.Errorf("reading the landings of rig %s: %w", rig, err))
		}
		for _, rec := range recs {
			if !rec.LandedAt.After(cut) {
				continue
			}
			if !found || rec.LandedAt.After(newest.LandedAt) {
				newest, found = rec, true
			}
		}
	}
	if !found {
		return failed(check, "none since "+cut.Format(time.RFC3339), threshold, "no landing since the cut-over")
	}
	measured := fmt.Sprintf("bead %s landed %s", newest.BeadID, newest.LandedAt.Format(time.RFC3339))
	switch {
	case newest.GateResult == "":
		return failed(check, measured, threshold, "the newest landing since the cut-over records no gate result")
	case newest.LandedCommit == "":
		return failed(check, measured, threshold, "the newest landing since the cut-over records no merged commit")
	}
	return passed(check, measured, threshold)
}

// landingRigs names the rigs the landing worker reads, in name order, as the
// daemon's landingWorkerRigs does: every registered rig, narrowed to the
// landing worker's rig list when the settings name one. A name in that list the
// registry does not hold is not read; it comes back in unregistered, in the
// order the list gives it, so the checks can report it. A list that narrows to
// nothing returns no rigs. A registry or settings file that cannot be read is an
// error that names the file or key.
func landingRigs(town string) (rigs, unregistered []string, err error) {
	reg, err := config.LoadRigsConfig(filepath.Join(town, constants.DirMayor, constants.FileRigsJSON))
	if err != nil {
		return nil, nil, fmt.Errorf("reading the rig registry mayor/%s: %w", constants.FileRigsJSON, err)
	}
	known := slices.Sorted(maps.Keys(reg.Rigs))
	for _, rig := range known {
		if rig == "" || strings.ContainsAny(rig, `/\`) || rig == "." || rig == ".." {
			return nil, nil, fmt.Errorf("the rig registry mayor/%s names %q, which is not a rig directory name", constants.FileRigsJSON, rig)
		}
	}
	patrols, err := config.LoadDaemonPatrolConfig(config.DaemonPatrolConfigPath(town))
	if err != nil && !errors.Is(err, config.ErrNotFound) {
		return nil, nil, fmt.Errorf("reading the landing worker's rig list (mayor/daemon.json patrols.landing_worker.rigs): %w", err)
	}
	var want []string
	if patrols != nil && patrols.Patrols != nil && patrols.Patrols.LandingWorker != nil {
		want = patrols.Patrols.LandingWorker.Rigs
	}
	if len(want) == 0 {
		return known, nil, nil
	}
	for _, name := range want {
		if !slices.Contains(known, name) && !slices.Contains(unregistered, name) {
			unregistered = append(unregistered, name)
		}
	}
	return slices.DeleteFunc(known, func(r string) bool { return !slices.Contains(want, r) }), unregistered, nil
}
