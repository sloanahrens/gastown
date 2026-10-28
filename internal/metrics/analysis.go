package metrics

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The daily analysis: one run a day rebuilds the mango binary when its source
// moved, runs the extract, joins the landings to the snapshots and the peaks,
// runs the mango stage over the three targets and publishes the panel the
// dashboard reads. The stamp is written last, so a run that fails anywhere
// leaves the last good panel and stamp in place and the next run retries.

const (
	// DailyEvery is how often the analysis is due: a stamp younger than it
	// means the last run is still current.
	DailyEvery = 24 * time.Hour
	// JoinMaxAge is how old a snapshot may be and still be the one in force
	// for a landing: two hourly snapshots, so one skipped hour still joins.
	JoinMaxAge = 90 * time.Minute
)

// mangoModuleDir is the archive directory holding mango's go.mod: the module
// sits in go-api/, not at the archive root.
const mangoModuleDir = "go-api"

// mangoBuildTarget is the CLI's package, relative to the module directory.
const mangoBuildTarget = "./cmd/mango"

// The analysis's own files, under <town>/.runtime/metrics.
const (
	analysisDirName = "latest"
	panelName       = "panel.json"
	joinedName      = "landings.joined.csv"
	stampName       = "stamp"
	docsName        = "docs"
	mangoShaName    = "mango.sha"
	landingsName    = "landings.csv"
	sidecarName     = "landings.sidecar.json"
)

// Due reports whether the analysis is due: a zero stamp, a stamp at least every
// old, and a future stamp are all due. A future stamp is due rather than fresh
// because a clock set back would otherwise wedge the daily run until the clock
// caught up again.
func Due(stamp, now time.Time, every time.Duration) bool {
	if stamp.IsZero() || every <= 0 {
		return true
	}
	if stamp.After(now) {
		return true
	}
	return now.Sub(stamp) >= every
}

// NeedRebuild reports whether the mango binary has to be rebuilt for head:
// always when no build is recorded, and whenever the recorded head differs.
func NeedRebuild(built, head string) bool {
	return built != head
}

// AnalysisOptions is what the daily analysis reaches outside its Env: the
// extract, the mango repository, and where its binary goes.
type AnalysisOptions struct {
	// Repo is accepted and ignored: the extract runs in process, so no checkout
	// is needed to find it.
	Repo string
	// Home is the home directory the in-process extract joins its defaults and
	// its home-level bead listing to; empty is the process's own.
	Home string
	// Extract runs the extract over town, writing its docs into docsDir, and
	// returns the day directory it wrote. Nil is the in-process extract, whose
	// git and bd commands run through Env.RunDir.
	Extract func(ctx context.Context, town, docsDir string) (string, error)
	// MangoGitDir is the bare repository whose origin/main is built.
	MangoGitDir string
	// MangoBin is the binary the mango stage runs; <MangoBin>.sha records the
	// head it was built from.
	MangoBin string
	// BuildModuleDir is the archive directory holding mango's go.mod; empty is
	// mangoModuleDir.
	BuildModuleDir string
	// BuildTarget is the package go build compiles inside BuildModuleDir; empty
	// is mangoBuildTarget.
	BuildTarget string
	// Every is the stamp's interval; zero is DailyEvery.
	Every time.Duration
	// MaxAge is the oldest snapshot the join accepts; zero is JoinMaxAge.
	MaxAge time.Duration
	// Reportf takes the notes a run does not fail on: a file it skipped and the
	// panel's trend or events it could not draw. Nil writes them to the process
	// log.
	Reportf func(format string, args ...any)
}

// withDefaults fills the zero fields with the daily run's values.
func (o AnalysisOptions) withDefaults() AnalysisOptions {
	if o.BuildModuleDir == "" {
		o.BuildModuleDir = mangoModuleDir
	}
	if o.BuildTarget == "" {
		o.BuildTarget = mangoBuildTarget
	}
	if o.Every == 0 {
		o.Every = DailyEvery
	}
	if o.MaxAge == 0 {
		o.MaxAge = JoinMaxAge
	}
	if o.Reportf == nil {
		o.Reportf = log.Printf
	}
	return o
}

// AnalysisResult is one run of the daily analysis: whether it was due, where it
// published, how many landings it read, and the panel it published.
type AnalysisResult struct {
	Ran      bool
	Dir      string
	Landings int
	Panel    Panel
	// SnapshotsSkipped is how many snapshots had a ts that does not parse, so
	// the join could not place them.
	SnapshotsSkipped int
	// PeaksSkipped names the peak files the join could not read, so the receipt
	// names the loss rather than leaving it to the report log.
	PeaksSkipped []string
}

// RunAnalysis runs the daily analysis when its stamp is older than Every, and
// publishes panel.json, the joined landings and the stamp into
// <town>/.runtime/metrics/latest. A run that is not due returns Ran false and
// no error.
//
// A failed step returns its error with the old panel.json and the old stamp
// still in place, so the failure costs the next run's retry rather than a day
// of a stale panel with a fresh stamp beside it. The panel's trend and events
// are not steps: an input the run cannot use costs that list and goes on the
// report line, so the day's panel still publishes.
func RunAnalysis(ctx context.Context, env Env, opts AnalysisOptions) (AnalysisResult, error) {
	o := opts.withDefaults()
	dir := filepath.Join(env.Town, ".runtime", "metrics", analysisDirName)
	res := AnalysisResult{Dir: dir}
	switch {
	case o.MangoGitDir == "":
		return res, errors.New("analysis: AnalysisOptions.MangoGitDir is unset, so mango cannot be built")
	case o.MangoBin == "":
		return res, errors.New("analysis: AnalysisOptions.MangoBin is unset, so mango cannot be run")
	case env.Now == nil:
		return res, errors.New("analysis: Env.Now is unset")
	case env.ReadFile == nil:
		return res, errors.New("analysis: Env.ReadFile is unset")
	}
	now := env.Now().UTC()
	inProcess := o.Extract == nil
	if inProcess {
		o.Extract = inProcessExtract(now, o.Home, env.RunDir)
	}
	stamp, err := readStamp(env, filepath.Join(dir, stampName))
	if err != nil {
		return res, err
	}
	if !Due(stamp, now, o.Every) {
		return res, nil
	}
	// The command seams are checked only once a run is due: an hour that is not
	// a day's boundary must not fail for a seam it never reaches.
	switch {
	case env.Run == nil:
		return res, errors.New("analysis: Env.Run is unset")
	case env.RunEnv == nil:
		return res, errors.New("analysis: Env.RunEnv is unset, so mango's git and go commands cannot run")
	case inProcess && env.RunDir == nil:
		return res, errors.New("analysis: Env.RunDir is unset, so the in-process extract cannot run its git and bd commands")
	}

	if err := ensureMango(ctx, env, o); err != nil {
		return res, err
	}
	docs := filepath.Join(dir, docsName)
	if err := os.MkdirAll(docs, 0o755); err != nil {
		return res, fmt.Errorf("analysis: %w", err)
	}
	dayDir, err := o.Extract(ctx, env.Town, docs)
	if err != nil {
		return res, fmt.Errorf("extract: %w", err)
	}
	columns, rows, explainOnly, err := readExtract(env, dayDir)
	if err != nil {
		return res, err
	}
	snaps, err := readSnapshots(env)
	if err != nil {
		return res, err
	}
	peaks, peaksSkipped, err := readPeaks(env, o.Reportf)
	if err != nil {
		return res, err
	}

	joinHeader, joined, cov := Join(columns, rows, snaps, o.MaxAge)
	joinHeader, joined = JoinPeaks(joinHeader, joined, peaks)
	weekly := panelWeekly(joined, o.Reportf)
	targets, err := analyseTargets(ctx, env, o, joinHeader, joined, explainOnly)
	if err != nil {
		return res, err
	}
	panel := BuildPanel(targets, PanelMeta{
		Items:         len(joined),
		SnapshotHours: snapshotHours(snaps),
		Weekly:        weekly,
		Events:        panelEvents(env, o.Reportf),
	}, now)
	panelJSON, err := json.MarshalIndent(panel, "", "  ")
	if err != nil {
		return res, fmt.Errorf("panel: %w", err)
	}
	var joinedCSV bytes.Buffer
	if err := WriteLandings(&joinedCSV, joinHeader, joined); err != nil {
		return res, err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("analysis: %w", err)
	}
	if err := PublishAtomic(dir, panelName, append(panelJSON, '\n')); err != nil {
		return res, err
	}
	if err := PublishAtomic(dir, joinedName, joinedCSV.Bytes()); err != nil {
		return res, err
	}
	// The stamp goes last: everything the run publishes is in place by the time
	// it says the analysis is current.
	if err := PublishAtomic(dir, stampName, []byte(now.Format(time.RFC3339)+"\n")); err != nil {
		return res, err
	}
	res.Ran, res.Landings, res.Panel, res.SnapshotsSkipped = true, len(joined), panel, cov.Skipped
	res.PeaksSkipped = peaksSkipped
	return res, nil
}

// readStamp reads when the last analysis published. A missing or unparseable
// stamp is the zero time, which is due.
func readStamp(env Env, path string) (time.Time, error) {
	data, err := env.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("analysis stamp %s: %w", path, err)
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, nil
	}
	return at, nil
}

// inProcessExtract is the daily extract run in this process, the three calls
// the extract command makes: the town's sources read through RealEnv into the
// docs directory it is given, the extract written under the town's runtime for
// the local day, and the caches saved so the next run reads each bead and
// commit once. It returns the day directory the extract wrote. run is the
// runner every git and bd command goes through; home is the directory the
// extract's defaults and its home-level bead listing join to, empty for the
// process's own.
func inProcessExtract(now time.Time, home string, run RunnerDir) func(ctx context.Context, town, docsDir string) (string, error) {
	return func(ctx context.Context, town, docsDir string) (string, error) {
		real, err := RealEnv(ctx, RealEnvOptions{
			Today:   now.Local().Format("2006-01-02"),
			Town:    town,
			DocsDir: docsDir,
			Home:    home,
			Run:     run,
		})
		if err != nil {
			return "", err
		}
		if _, err := Extract(ctx, real.Extract); err != nil {
			return "", err
		}
		if err := SaveCaches(real.Extract, real.Town); err != nil {
			return "", err
		}
		return real.Extract.OutDir, nil
	}
}

// runEnv runs argv through the RunEnv seam. A seam that is unset is an error
// rather than a command that never ran.
func runEnv(ctx context.Context, env Env, argv []string) ([]byte, error) {
	if env.RunEnv == nil {
		return nil, fmt.Errorf("analysis: Env.RunEnv is unset, so %s cannot run", argv[0])
	}
	return env.RunEnv(ctx, argv, nil)
}

// extractDir is the directory the extract reports writing: its receipt line
// ends with "-> <dir>", and the analysis reads there, so a run that writes
// somewhere else is followed rather than assumed from the clock.
func extractDir(out []byte) (string, error) {
	const arrow = " -> "
	text := strings.TrimSpace(string(out))
	if i := strings.LastIndex(text, arrow); i >= 0 {
		if dir := strings.TrimSpace(text[i+len(arrow):]); dir != "" {
			return dir, nil
		}
	}
	return "", fmt.Errorf("extract: no output directory in its receipt %q", tailString(string(out), 400))
}

// readExtract reads the landings and the explain-only columns the extract wrote
// into dir, with the CSV header Join keeps as the joined table's own. A missing
// sidecar names no explain-only column: it is additive to the CSV, so its
// absence is not an analysis without landings.
func readExtract(env Env, dir string) (columns []string, rows []Row, explainOnly []string, err error) {
	data, err := env.ReadFile(filepath.Join(dir, landingsName))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", landingsName, err)
	}
	columns, rows, err = ReadLandings(bytes.NewReader(data))
	if err != nil {
		return nil, nil, nil, err
	}
	side, err := env.ReadFile(filepath.Join(dir, sidecarName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return columns, rows, nil, nil
	case err != nil:
		return nil, nil, nil, fmt.Errorf("%s: %w", sidecarName, err)
	}
	explainOnly, err = ReadSidecar(bytes.NewReader(side))
	if err != nil {
		return nil, nil, nil, err
	}
	return columns, rows, explainOnly, nil
}

// readSnapshots reads the town's snapshot log. No log at all is no snapshots:
// the landings still publish, with every snap_ cell empty.
func readSnapshots(env Env) ([]Record, error) {
	data, err := env.ReadFile(SnapshotPath(env.Town))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("snapshots: %w", err)
	}
	return ReadSnapshots(bytes.NewReader(data))
}

// snapshotHours is the hours of history the snapshots span, so the panel says
// how much the joined cells actually cover. A single snapshot is no span.
func snapshotHours(snaps []Record) int {
	var first, last time.Time
	for _, rec := range snaps {
		at, err := time.Parse(time.RFC3339, rec.TS)
		if err != nil {
			continue
		}
		if first.IsZero() || at.Before(first) {
			first = at
		}
		if last.IsZero() || at.After(last) {
			last = at
		}
	}
	if first.IsZero() {
		return 0
	}
	return int(last.Sub(first).Round(time.Hour) / time.Hour)
}

// panelWeekly is the panel's trend: the town's own weeks, the thin ones and the
// ones no rejected share could be read from left out. A builder error goes on
// the analysis's report line and leaves the series empty, the same policy as
// panelEvents' file, so a trend the panel cannot draw costs the trend rather
// than the day's panel. The builder sorts its rows oldest first, so BuildPanel
// keeps the newest of them.
func panelWeekly(landings []Row, reportf func(format string, args ...any)) []PanelWeek {
	rows, err := BuildWeekly(landings, nil, nil, nil, nil, nil)
	if err != nil {
		reportf("metrics: %v; the panel carries no weekly trend", err)
		return nil
	}
	series := make([]PanelWeek, 0, len(rows))
	for _, row := range rows {
		if row.Rig != "ALL" || row.Thin || row.RejectedShare == nil {
			continue
		}
		series = append(series, PanelWeek{Week: row.Week, Landings: row.Landings, RejectedShare: *row.RejectedShare})
	}
	return series
}

// panelEvents is the regime events the panel marks, as the reader returns them;
// BuildPanel establishes the order, validity and cap it writes. A file the
// reader cannot use costs the panel its marks rather than the run, so it goes
// on the analysis's own report line and the list stays empty.
func panelEvents(env Env, reportf func(format string, args ...any)) []PanelEvent {
	events, err := ReadRegimeEvents(env)
	if err != nil {
		reportf("metrics: %v; the panel carries no events", err)
		return nil
	}
	marked := make([]PanelEvent, 0, len(events))
	for _, event := range events {
		marked = append(marked, PanelEvent{Date: event.Date, What: event.What})
	}
	return marked
}

// ensureMango rebuilds the mango binary when origin/main moved since the build
// that made it, and records the new head in <MangoBin>.sha only after the build
// succeeds, so a failed build leaves the old head to be retried.
func ensureMango(ctx context.Context, env Env, o AnalysisOptions) error {
	out, err := runEnv(ctx, env, []string{"git", "--git-dir", o.MangoGitDir, "rev-parse", "origin/main"})
	if err != nil {
		return fmt.Errorf("mango head: %w", err)
	}
	head := strings.TrimSpace(string(out))
	if head == "" {
		return fmt.Errorf("mango head: %s named no commit", o.MangoGitDir)
	}
	shaPath := filepath.Join(filepath.Dir(o.MangoBin), mangoShaName)
	built := ""
	if data, err := env.ReadFile(shaPath); err == nil {
		built = strings.TrimSpace(string(data))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", shaPath, err)
	}
	if !NeedRebuild(built, head) {
		if _, err := os.Stat(o.MangoBin); err == nil {
			return nil
		}
		// A sha with no binary behind it is not a build: rebuild from the head
		// the sha records rather than leaving every mango call to fail.
	}
	archive, err := runEnv(ctx, env, []string{"git", "--git-dir", o.MangoGitDir, "archive", "--format=tar", "origin/main"})
	if err != nil {
		return fmt.Errorf("mango archive: %w", err)
	}
	src, err := os.MkdirTemp("", "gt-mango-build-*")
	if err != nil {
		return fmt.Errorf("mango build: %w", err)
	}
	defer func() { _ = os.RemoveAll(src) }()
	if err := extractTar(src, archive); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(o.MangoBin), 0o755); err != nil {
		return fmt.Errorf("mango build: %w", err)
	}
	moduleDir := filepath.Join(src, o.BuildModuleDir)
	if _, err := runEnv(ctx, env, []string{"go", "build", "-C", moduleDir, "-o", o.MangoBin, o.BuildTarget}); err != nil {
		return fmt.Errorf("mango build: %w", err)
	}
	return PublishAtomic(filepath.Dir(o.MangoBin), mangoShaName, []byte(head+"\n"))
}

// extractTar writes an archive's regular files below dir. Link entries are
// dropped: a source tree builds from its files, and a link is the one entry
// that could write outside dir.
func extractTar(dir string, data []byte) error {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("mango archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		path, err := archivePath(dir, hdr.Name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("mango archive: %w", err)
		}
		if err := writeFile(path, tr); err != nil {
			return fmt.Errorf("mango archive: %w", err)
		}
	}
}

// archivePath is the path an archive entry lands at, refused when the entry
// would land outside dir.
func archivePath(dir, name string) (string, error) {
	path := filepath.Join(dir, filepath.FromSlash(name))
	if path != dir && !strings.HasPrefix(path, dir+string(filepath.Separator)) {
		return "", fmt.Errorf("mango archive: %q lands outside the source directory", name)
	}
	return path, nil
}

// writeFile writes r to path, readable because go build reads it.
func writeFile(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// analyseTargets runs the mango stage for every target whose training split
// holds MangoMinSupport positives, and reports every target's split counts
// either way.
func analyseTargets(ctx context.Context, env Env, o AnalysisOptions, header []string, rows []Row, explainOnly []string) ([]TargetResult, error) {
	results := make([]TargetResult, 0, len(mangoTargets))
	for _, target := range mangoTargets {
		p := Prepare(header, rows, explainOnly, target)
		if p.TrainPositives < MangoMinSupport {
			results = append(results, skipTarget(target, p))
			continue
		}
		res, err := analyseTarget(ctx, env, o, p, target)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}

// skipTarget is a target's counts with no box: too few training positives is
// not evidence either way, so its panel line carries the counts and not-yet.
func skipTarget(target string, p Prepared) TargetResult {
	return TargetResult{
		Target:           target,
		TrainN:           len(p.Train),
		TrainPositives:   p.TrainPositives,
		HoldoutN:         len(p.Holdout),
		HoldoutPositives: p.HoldoutPositives,
		LastPositiveAt:   p.LastPositiveAt,
	}
}

// analyseTarget writes the target's split, runs the mango stage over it, and
// measures the boxes on the holdout.
func analyseTarget(ctx context.Context, env Env, o AnalysisOptions, p Prepared, target string) (TargetResult, error) {
	dir, err := os.MkdirTemp("", "gt-mango-*")
	if err != nil {
		return TargetResult{}, fmt.Errorf("mango %s: %w", target, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	open := func(path string) (io.WriteCloser, error) { return os.Create(path) }
	if err := WriteSplit(p, dir, open); err != nil {
		return TargetResult{}, fmt.Errorf("mango %s split: %w", target, err)
	}
	out, err := RunMango(ctx, env.Run, o.MangoBin, dir, target, MangoOptions{ReadFile: env.ReadFile})
	if err != nil {
		return TargetResult{}, fmt.Errorf("mango %s: %w", target, err)
	}
	return measureTarget(target, p, out.Boxes, MangoMinSupport), nil
}
