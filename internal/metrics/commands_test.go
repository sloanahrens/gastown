package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// The cases below drive the command layer through Main and Run, with the town
// wiring, the mango lookup and the filesystem answered from fixtures: no case
// reads ~/gt, starts a process, or writes outside its temp directory.

const commandToday = "2026-10-23"

// commandExtract is the fixture town's extract: two gastown landings in one
// week, one of them by hand, a shaped bead whose notes hold a rejection, a
// usage file and an overrun stop. Every landing's diff is cached, so the
// extract's size columns come from the fixture and no git runs.
func commandExtract(t *testing.T, outDir string) ExtractEnv {
	t.Helper()
	return ExtractEnv{
		LedgerLines: map[string][]string{"gastown": {
			extractLedgerLine(t, "gt-a", "gastown", "daemon", "2026-10-14T00:00:00Z"),
			extractLedgerLine(t, "gt-b", "gastown", "overseer-manual", "2026-10-14T00:00:00Z"),
		}},
		// No review lines: the fixture's rejection history comes from the
		// bead's notes.
		ReviewLines: []string{},
		Diffs:       commandDiffs(),
		RunBd:       extractBd(&[]bdCall{}),
		BeadDirs:    []string{"/g"},
		PrefixToRig: map[string]string{"gt": "gastown"},
		UsageFiles: map[string]UsageFile{"gt-a": {
			Models:   map[string]UsageModel{"m": {Calls: 1}},
			Sessions: []json.RawMessage{json.RawMessage(`{}`)},
		}},
		OverrunStates: map[string]OverrunState{
			"gt-a": {},
			"gt-b": {StopBeganAt: "2026-10-14T00:00:00Z"},
		},
		OMCommits: map[string][]OMCommit{"gastown": {
			{At: "2026-09-01T00:00:00Z", Text: `{"threshold": 0.7}`},
		}},
		OutDir: outDir,
		Today:  commandToday,
	}
}

// commandDiffs is the fixture's diff source: each landing's commit is cached,
// so Stats answers without a runner and the cache has something to save.
func commandDiffs() *GitDiffSource {
	cache := DiffCache{}
	for _, bead := range []string{"gt-a", "gt-b"} {
		cache[DiffKey{Rig: "gastown", Commit: "c-" + bead}] = DiffStat{LinesAdded: 5, LinesDeleted: 5, FilesTouched: 1}
	}
	return &GitDiffSource{Cache: cache}
}

// commandTown is one fixture run: the wired town, the file seams the steps
// after the extract read through, and the buffer the receipts go to.
type commandTown struct {
	real      RealTown
	env       Env
	out       *bytes.Buffer
	wireCalls int
}

// newCommandTown wires the fixture: a temp town and docs directory, with the
// extract writing under the town's runtime as the real wiring has it.
func newCommandTown(t *testing.T) *commandTown {
	t.Helper()
	town, docs := t.TempDir(), t.TempDir()
	return &commandTown{
		real: RealTown{
			Town:    town,
			DocsDir: docs,
			Extract: commandExtract(t, filepath.Join(town, ".runtime", "metrics", commandToday)),
		},
		env: Env{Town: town, ReadFile: os.ReadFile, ReadDir: listDir},
		out: &bytes.Buffer{},
	}
}

// outDir is the extract's output directory.
func (f *commandTown) outDir() string { return f.real.Extract.OutDir }

// commands is the fixture as one invocation: every step wired to it and no
// mango binary, which is a host without mango.
func (f *commandTown) commands() Commands {
	return Commands{
		Today: commandToday,
		Wire: func(context.Context) (RealTown, error) {
			f.wireCalls++
			return f.real, nil
		},
		Env:   f.env,
		Mango: func(string) string { return "" },
		Out:   f.out,
	}
}

// run is Main over the fixture's invocation.
func (f *commandTown) run(t *testing.T, argv ...string) (int, error) {
	t.Helper()
	return Main(context.Background(), argv, f.commands())
}

// readFile reads path and fails the case when it cannot.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// requireFile fails the case unless path exists.
func requireFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// TestMetricsMainUsage holds Main's answer to an argv that is not one command:
// the usage line, rc 2, and no town read.
func TestMetricsMainUsage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		argv []string
	}{
		{"no arguments", nil},
		{"an unknown command", []string{"frobnicate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			wired := false
			code, err := Main(context.Background(), tc.argv, Commands{
				Wire: func(context.Context) (RealTown, error) {
					wired = true
					return RealTown{}, errors.New("the wiring must not run")
				},
				Out: &out,
			})
			if err != nil {
				t.Fatalf("Main: %v", err)
			}
			if code != 2 {
				t.Errorf("code = %d, want 2", code)
			}
			if got, want := out.String(), usageText+"\n"; got != want {
				t.Errorf("out = %q, want %q", got, want)
			}
			if wired {
				t.Error("the town was wired for an argv that names no command")
			}
		})
	}
}

// TestMetricsRunRefusesAnUnknownCommand holds Run to refusing a name Main would
// never pass it, rather than running nothing and reporting success.
func TestMetricsRunRefusesAnUnknownCommand(t *testing.T) {
	t.Parallel()
	wired := false
	err := Run(context.Background(), "frobnicate", Commands{
		Wire: func(context.Context) (RealTown, error) {
			wired = true
			return RealTown{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("err = %v, want a refusal naming the command", err)
	}
	if wired {
		t.Error("the town was wired for a name that is not a subcommand")
	}
}

// TestMetricsWireReadsTheTownTheWiringBuilt holds the steps after the extract to
// the town the wiring returned, even when the caller's Env names another, so a
// caller that supplies only file seams still reads the town the extract read.
func TestMetricsWireReadsTheTownTheWiringBuilt(t *testing.T) {
	t.Parallel()
	readFile := func(string) ([]byte, error) { return nil, errors.New("not read") }
	c := Commands{
		Wire: func(context.Context) (RealTown, error) { return RealTown{Town: "/wired"}, nil },
		Env:  Env{Town: "/stale", ReadFile: readFile},
	}
	real, env, err := c.wire(context.Background())
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	if real.Town != "/wired" || env.Town != "/wired" {
		t.Errorf("towns = %q, %q, want the wired %q", real.Town, env.Town, "/wired")
	}
	if env.ReadFile == nil {
		t.Error("wire dropped the caller's file seam")
	}
}

// TestMetricsExtract drives the extract command: the four tables and the footer
// are written into the wired town, both caches are saved, and the receipt
// counts the landings and the weekly rows.
func TestMetricsExtract(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	code, err := town.run(t, CommandExtract)
	if err != nil {
		t.Fatalf("Main: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	want := fmt.Sprintf("extract: 2 landings, 2 weekly rows -> %s\n", town.outDir())
	if got := town.out.String(); got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
	for _, name := range []string{"landings.csv", "landings.sidecar.json", "weekly.csv", "beads.csv", "footer.json"} {
		requireFile(t, filepath.Join(town.outDir(), name))
	}
	for _, name := range []string{"handoff_bytes.json", "diffstats.json"} {
		requireFile(t, filepath.Join(town.real.Town, ".runtime", "metrics", "cache", name))
	}
}

// TestMetricsReport drives the report command over an earlier extract's output:
// the digest is written under the docs directory and its path is the receipt.
func TestMetricsReport(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	if _, err := town.run(t, CommandExtract); err != nil {
		t.Fatalf("extract: %v", err)
	}
	town.out.Reset()
	code, err := town.run(t, CommandReport)
	if err != nil {
		t.Fatalf("Main: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	path := filepath.Join(town.real.DocsDir, commandToday+"-town-metrics.md")
	if got, want := town.out.String(), "report: "+path+"\n"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
	text := readFile(t, path)
	if !strings.HasPrefix(text, "# Town metrics "+commandToday+"\n") {
		t.Errorf("digest starts %q, want its dated heading", firstLine(text))
	}
}

// TestMetricsBacktest drives the backtest command: the section is written into
// the extract's output directory, carries the unconfirmed regime event, and the
// report embeds it.
func TestMetricsBacktest(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	if _, err := town.run(t, CommandExtract); err != nil {
		t.Fatalf("extract: %v", err)
	}
	writeTownFile(t, filepath.Join(town.real.DocsDir, "regime-events.json"),
		`[{"date": "2026-10-14", "what": "gate raised", "confirmed": false}]`)
	town.out.Reset()
	if _, err := town.run(t, CommandBacktest); err != nil {
		t.Fatalf("Main: %v", err)
	}
	if got, want := town.out.String(), "backtest: section written\n"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
	section := readFile(t, filepath.Join(town.outDir(), reportBacktestName))
	for _, wantText := range []string{"| rework depth |", "gate raised [unconfirmed]"} {
		if !strings.Contains(section, wantText) {
			t.Errorf("section = %q, want it to hold %q", section, wantText)
		}
	}
	town.out.Reset()
	if _, err := town.run(t, CommandReport); err != nil {
		t.Fatalf("report: %v", err)
	}
	digest := readFile(t, filepath.Join(town.real.DocsDir, commandToday+"-town-metrics.md"))
	if !strings.Contains(digest, "## Usefulness backtest\n\nEvents (from") {
		t.Error("digest does not embed the backtest section")
	}
}

// TestMetricsMango drives the mango command over a town whose review log and
// merging split the section's tables read: the section is written beside the
// tables and the receipt says it was.
func TestMetricsMango(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	writeTownFile(t, land.TownReviewsFile(town.real.Town).Path, firstPassLine(t, firstPassVerdict("gt-a", 1, land.VerdictApprove)))
	writeTownFile(t, commandLandingLog(t, town.real.Town), firstPassLine(t, firstPassLanded("gt-a", firstPassAt)))
	mango, _ := mangoSectionRun(t, mangoSectionResult)
	cmds := town.commands()
	cmds.Mango = func(string) string { return "/opt/mango" }
	cmds.Env.Run = rejectionDigestRunner(t, firstPassBd(t, map[string]string{"gt-a": "polecat/alfa/gt-a"}, nil), mango)
	if _, err := town.run(t, CommandExtract); err != nil {
		t.Fatalf("extract: %v", err)
	}
	town.out.Reset()
	if err := Run(context.Background(), CommandMango, cmds); err != nil {
		t.Fatalf("Main: %v", err)
	}
	if got, want := town.out.String(), "mango: section written\n"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
	text := readFile(t, filepath.Join(town.outDir(), reportMangoName))
	if !strings.HasPrefix(text, "### Weekly table profile\n") {
		t.Errorf("section starts %q, want the weekly table profile", firstLine(text))
	}
	for _, name := range []string{"mango-summary.json"} {
		requireFile(t, filepath.Join(town.outDir(), name))
	}
}

// TestMetricsMangoWithoutABinary drives the host that has no mango: the section
// still says where the tables are, and the receipt is that first line rather
// than "section written".
func TestMetricsMangoWithoutABinary(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	if _, err := town.run(t, CommandExtract); err != nil {
		t.Fatalf("extract: %v", err)
	}
	town.out.Reset()
	if _, err := town.run(t, CommandMango); err != nil {
		t.Fatalf("Main: %v", err)
	}
	want := fmt.Sprintf("mango: mango binary not found; CSVs written to %s, mango step skipped.\n", town.outDir())
	if got := town.out.String(); got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
	if text := readFile(t, filepath.Join(town.outDir(), reportMangoName)); !strings.HasPrefix(text, "mango binary not found;") {
		t.Errorf("section starts %q, want the missing-binary line", firstLine(text))
	}
}

// TestMetricsAll drives all: the four steps run in order over one wiring, and
// the digest it writes embeds both sections.
func TestMetricsAll(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	code, err := town.run(t, CommandAll)
	if err != nil {
		t.Fatalf("Main: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimSuffix(town.out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("receipts = %q, want four lines", lines)
	}
	wants := []string{
		fmt.Sprintf("extract: 2 landings, 2 weekly rows -> %s", town.outDir()),
		fmt.Sprintf("mango: mango binary not found; CSVs written to %s, mango step skipped.", town.outDir()),
		"backtest: section written",
		"report: " + filepath.Join(town.real.DocsDir, commandToday+"-town-metrics.md"),
	}
	for i, want := range wants {
		if lines[i] != want {
			t.Errorf("receipt %d = %q, want %q", i, lines[i], want)
		}
	}
	if town.wireCalls != 1 {
		t.Errorf("the town was wired %d times, want once", town.wireCalls)
	}
	digest := readFile(t, filepath.Join(town.real.DocsDir, commandToday+"-town-metrics.md"))
	if !strings.Contains(digest, "## Mango findings\n\nmango binary not found;") {
		t.Error("digest does not embed the mango section")
	}
	if !strings.Contains(digest, "## Usefulness backtest\n\n") {
		t.Error("digest does not embed the backtest section")
	}
}

// TestMetricsStepsNeedTheDaysExtract holds the three steps after the extract to
// failing on an output directory no extract wrote, rather than reporting a
// section written from nothing.
func TestMetricsStepsNeedTheDaysExtract(t *testing.T) {
	t.Parallel()
	for _, command := range []string{CommandMango, CommandBacktest, CommandReport} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			town := newCommandTown(t)
			cmds := town.commands()
			cmds.Mango = func(string) string { return "/opt/mango" }
			err := Run(context.Background(), command, cmds)
			if err == nil {
				t.Fatalf("%s over an empty output directory succeeded", command)
			}
			if !strings.HasPrefix(err.Error(), command+": ") {
				t.Errorf("err = %v, want it to name %s", err, command)
			}
			if town.out.Len() != 0 {
				t.Errorf("receipts = %q, want none from a step that failed", town.out.String())
			}
		})
	}
}

// TestMetricsExtractNamesTheStepThatFailed drives each way the extract command
// stops: a wiring that failed, an extract with a source it cannot read, and
// caches that cannot be saved.
func TestMetricsExtractNamesTheStepThatFailed(t *testing.T) {
	t.Parallel()
	wireErr := errors.New("the town is not reachable")
	fileAsTown := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(fileAsTown, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		overrides commandOverrides
		want      string
	}{
		{"wiring", commandOverrides{wireErr: wireErr}, "the town is not reachable"},
		{"extract", commandOverrides{noDiffs: true}, "Diffs is unset"},
		{"save caches", commandOverrides{town: fileAsTown}, "save caches"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			code, err := Main(context.Background(), []string{CommandExtract}, tc.overrides.invocation(t, &buf))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to hold %q", err, tc.want)
			}
			if code != 1 {
				t.Errorf("code = %d, want 1 with the error returned", code)
			}
			if buf.Len() != 0 {
				t.Errorf("receipts = %q, want none from a run that failed", buf.String())
			}
		})
	}
}

// commandOverrides is the fixture overrides a failing extract case runs with.
type commandOverrides struct {
	wireErr error
	noDiffs bool
	town    string
}

// invocation is the fixture's invocation with the case's overrides applied.
func (s commandOverrides) invocation(t *testing.T, out *bytes.Buffer) Commands {
	t.Helper()
	town := newCommandTown(t)
	real := town.real
	if s.noDiffs {
		real.Extract.Diffs = nil
	}
	if s.town != "" {
		real.Town = s.town
	}
	return Commands{
		Today: commandToday,
		Wire: func(context.Context) (RealTown, error) {
			if s.wireErr != nil {
				return RealTown{}, s.wireErr
			}
			return real, nil
		},
		Env: town.env,
		Out: out,
	}
}

// TestFindMango holds the lookup's order: the override as given, then the
// town's own build, then PATH, and nothing when none of them is there.
func TestFindMango(t *testing.T) {
	t.Parallel()
	const town = "/town"
	built := filepath.Join(town, ".runtime", "metrics", "bin", "mango")
	cases := []struct {
		name   string
		lookup func(t *testing.T) MangoLookup
		want   string
	}{
		{
			"MANGO_BIN is taken as given",
			func(t *testing.T) MangoLookup {
				return MangoLookup{
					Getenv:   func(string) string { return "/opt/mango" },
					Exists:   unaskedExists(t),
					LookPath: unaskedLookPath(t),
				}
			},
			"/opt/mango",
		},
		{
			"the town's built binary is next",
			func(t *testing.T) MangoLookup {
				return MangoLookup{
					Getenv:   func(string) string { return "" },
					Exists:   func(path string) bool { return path == built },
					LookPath: unaskedLookPath(t),
				}
			},
			built,
		},
		{
			"PATH is last",
			func(*testing.T) MangoLookup {
				return MangoLookup{
					Getenv:   func(string) string { return "" },
					Exists:   func(string) bool { return false },
					LookPath: func(name string) (string, error) { return "/usr/local/bin/" + name, nil },
				}
			},
			"/usr/local/bin/mango",
		},
		{
			"no binary at all",
			func(*testing.T) MangoLookup {
				return MangoLookup{
					Getenv:   func(string) string { return "" },
					Exists:   func(string) bool { return false },
					LookPath: func(string) (string, error) { return "", errors.New("not found") },
				}
			},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FindMango(town, tc.lookup(t)); got != tc.want {
				t.Errorf("FindMango = %q, want %q", got, tc.want)
			}
		})
	}
}

// unaskedExists is an Exists seam that fails the case it is called in.
func unaskedExists(t *testing.T) func(string) bool {
	t.Helper()
	return func(string) bool {
		t.Fatal("Exists was asked after a step had already answered")
		return false
	}
}

// unaskedLookPath is a LookPath seam that fails the case it is called in.
func unaskedLookPath(t *testing.T) func(string) (string, error) {
	t.Helper()
	return func(string) (string, error) {
		t.Fatal("LookPath was asked after a step had already answered")
		return "", nil
	}
}

// TestBacktestRowsCarryTheWeeksFiguresAndCounts holds the typed weekly rows to
// the backtest's rows: a figure and a count the week has are carried, a figure
// it lacks is absent, and the labels are not values.
func TestBacktestRowsCarryTheWeeksFiguresAndCounts(t *testing.T) {
	t.Parallel()
	size := 12.5
	rows := backtestRows([]WeeklyMetrics{{
		Week: "2026-10-19", Rig: "ALL", Landings: 7, SizeMedian: &size,
	}})
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one", rows)
	}
	row := rows[0]
	if row.Week != "2026-10-19" || row.Rig != "ALL" {
		t.Errorf("row = %+v, want the week and rig carried", row)
	}
	if got := row.Values["landings"]; got != 7 {
		t.Errorf("landings = %v, want the week's count", got)
	}
	if got := row.Values["size_median"]; got != size {
		t.Errorf("size_median = %v, want %v", got, size)
	}
	if _, ok := row.Values["size_p90"]; ok {
		t.Error("size_p90 is carried for a week that has none, want it absent")
	}
	if _, ok := row.Values["dominant_model"]; ok {
		t.Error("dominant_model is carried as a value, want the labels left out")
	}
}

// failingWriter is an Out whose writes fail, as a closed pipe's do.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

// TestMetricsReportsAFailedReceipt holds a run to failing when its receipt
// could not be written, rather than counting a run nobody saw as a success.
func TestMetricsReportsAFailedReceipt(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	cmds := town.commands()
	cmds.Out = failingWriter{}
	code, err := Main(context.Background(), []string{CommandExtract}, cmds)
	if err == nil || !strings.Contains(err.Error(), "closed pipe") {
		t.Fatalf("err = %v, want the failed receipt's error", err)
	}
	if code != 1 {
		t.Errorf("code = %d, want 1 with the error returned", code)
	}
}

// TestMetricsSectionWriteNamesTheFileItCannotWrite drives a section that cannot
// be written: the error names the file rather than leaving the step silent.
func TestMetricsSectionWriteNamesTheFileItCannotWrite(t *testing.T) {
	t.Parallel()
	notADirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADirectory, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeText(notADirectory, reportBacktestName, "section")
	if err == nil || !strings.Contains(err.Error(), reportBacktestName+": ") {
		t.Fatalf("err = %v, want it to open with %s's name", err, reportBacktestName)
	}
}

// TestWriteBacktestTreatsAMissingOrCorruptSummaryAsNoTargets holds the backtest
// to reading a summary that is absent or half-written as naming no target, so
// the box column stays unanswered rather than reading a file's partial contents
// as a failed replication.
func TestWriteBacktestTreatsAMissingOrCorruptSummaryAsNoTargets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, summary string
	}{
		{"missing", ""},
		// The first entry decodes before the second one's type fails, so a
		// reader that kept what it decoded would name a target here.
		{"corrupt", `{"rejected_before_landing": {"boxes": 2, "holdout_positives": 3}, "bad": "not a target"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := newCommandTown(t)
			if _, err := town.run(t, CommandExtract); err != nil {
				t.Fatalf("extract: %v", err)
			}
			if tc.summary != "" {
				writeTownFile(t, filepath.Join(town.outDir(), mangoSummaryName), tc.summary)
			}
			text, err := WriteBacktest(town.env, town.outDir(), town.real.DocsDir)
			if err != nil {
				t.Fatalf("WriteBacktest: %v", err)
			}
			cells := reworkDepthCells(t, text)
			if cells[0] != "n/a" {
				t.Errorf("mango box replicates = %q, want n/a for a summary that names no target", cells[0])
			}
			if cells[1] != BacktestUndecided {
				t.Errorf("verdict = %q, want %q", cells[1], BacktestUndecided)
			}
		})
	}
}

// reworkDepthCells is the mango box column and the verdict of the backtest
// table's rework-depth row.
func reworkDepthCells(t *testing.T, section string) []string {
	t.Helper()
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| rework depth |") {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 7 {
			t.Fatalf("row %q has %d cells", line, len(fields))
		}
		return []string{strings.TrimSpace(fields[4]), strings.TrimSpace(fields[6])}
	}
	t.Fatalf("the section has no rework depth row:\n%s", section)
	return nil
}

// TestWriteMangoSectionReportsASectionItCannotWrite drives the write the mango
// command makes after the section renders: a section that cannot be written is
// an error naming the file, not a receipt for a file nobody wrote.
func TestWriteMangoSectionReportsASectionItCannotWrite(t *testing.T) {
	t.Parallel()
	outDir := t.TempDir()
	// A non-empty directory under the section's name: the atomic write's
	// rename cannot replace it, whatever the directory's mode.
	if err := os.MkdirAll(filepath.Join(outDir, reportMangoName, "kept"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No binary means the section needs no file of the extract's to render, so
	// the write is the step that fails.
	_, err := WriteMangoSection(context.Background(), Env{}, outDir, "", MangoSectionOptions{})
	if err == nil || !strings.Contains(err.Error(), reportMangoName+": ") {
		t.Fatalf("err = %v, want it to open with %s's name", err, reportMangoName)
	}
}

// TestWriteBacktestReportsEachFailure drives the backtest command's three ways
// of stopping: a seam it was not given, a weekly row it cannot date, and a
// section it cannot write.
func TestWriteBacktestReportsEachFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  Env
		// prepare writes the case's output directory; it is given the extract
		// the fixture wrote.
		prepare func(t *testing.T, town *commandTown)
		want    string
	}{
		{
			"no file seam",
			Env{},
			nil,
			"write backtest: Env.ReadFile is unset",
		},
		{
			"a weekly row it cannot date",
			Env{ReadFile: os.ReadFile},
			func(t *testing.T, town *commandTown) {
				writeTownFile(t, filepath.Join(town.outDir(), weeklyName), "week,rig,thin,dominant_model,size_median\n"+
					"2026-10-19,ALL,False,,12.5\nnot-a-date,ALL,False,,13\n")
			},
			"not-a-date",
		},
		{
			"a section it cannot write",
			Env{ReadFile: os.ReadFile},
			func(t *testing.T, town *commandTown) {
				if err := os.MkdirAll(filepath.Join(town.outDir(), reportBacktestName, "kept"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			reportBacktestName + ": ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := newCommandTown(t)
			if _, err := town.run(t, CommandExtract); err != nil {
				t.Fatalf("extract: %v", err)
			}
			if tc.prepare != nil {
				tc.prepare(t, town)
			}
			_, err := WriteBacktest(tc.env, town.outDir(), town.real.DocsDir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to hold %q", err, tc.want)
			}
		})
	}
}

// firstPassLine is rec as one log line, which is what the review and landing
// logs carry.
func firstPassLine(t *testing.T, rec any) string {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal %T: %v", rec, err)
	}
	return string(data) + "\n"
}

// commandLandingLog is the path of rig gastown's landing log in town.
func commandLandingLog(t *testing.T, town string) string {
	t.Helper()
	file, err := land.RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatalf("landing log: %v", err)
	}
	return file.Path
}
