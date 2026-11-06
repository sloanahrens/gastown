package cmd

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

// The flag's default is the documented command, word for word.
func TestDashboardJevSpendCmdFlagDefault(t *testing.T) {
	t.Parallel()

	f := dashboardCmd.Flags().Lookup("jev-spend-cmd")
	if f == nil {
		t.Fatal("gt dashboard has no --jev-spend-cmd flag")
	}
	const want = "gt jev spend --json --hours 24"
	if f.DefValue != want {
		t.Errorf("--jev-spend-cmd defaults to %q, want %q", f.DefValue, want)
	}
	if !strings.Contains(dashboardCmd.Long, want) {
		t.Errorf("the help does not document the default command %q", want)
	}
}

// The price is its own flag, not a number inside the command string.
func TestDashboardJevPriceFlagDefault(t *testing.T) {
	t.Parallel()

	f := dashboardCmd.Flags().Lookup("jev-usd-per-billion-tokens")
	if f == nil {
		t.Fatal("gt dashboard has no --jev-usd-per-billion-tokens flag")
	}
	if f.DefValue != "42" {
		t.Errorf("--jev-usd-per-billion-tokens defaults to %q, want 42", f.DefValue)
	}
	if strings.Contains(defaultJevSpendCmd, "usd-per-billion-tokens") {
		t.Errorf("the default command %q bakes in a price", defaultJevSpendCmd)
	}
}

func TestJevSpendArgv(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		cmd   string
		price float64
		want  []string
	}{
		{"default command takes the flag's price", defaultJevSpendCmd, 42,
			[]string{"gt", "jev", "spend", "--json", "--hours", "24", "--usd-per-billion-tokens", "42"}},
		{"a fractional price keeps its digits", "gt jev spend --json", 0.5,
			[]string{"gt", "jev", "spend", "--json", "--usd-per-billion-tokens", "0.5"}},
		{"a quoted path with a space is one word", `"/opt/my tools/jev spend" --json`, 3,
			[]string{"/opt/my tools/jev spend", "--json", "--usd-per-billion-tokens", "3"}},
		{"single quotes group too", `'/opt/my tools/jev' --json`, 3,
			[]string{"/opt/my tools/jev", "--json", "--usd-per-billion-tokens", "3"}},
		{"a price in the command is kept as written", "gt jev spend --usd-per-billion-tokens 7", 42,
			[]string{"gt", "jev", "spend", "--usd-per-billion-tokens", "7"}},
		{"the = spelling counts as a price", "gt jev spend --usd-per-billion-tokens=7", 42,
			[]string{"gt", "jev", "spend", "--usd-per-billion-tokens=7"}},
		{"an empty command is no argv", "", 42, nil},
		{"a blank command is no argv", "   ", 42, nil},
	}
	for _, tc := range cases {
		got, err := jevSpendArgv(tc.cmd, tc.price)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") || (got == nil) != (tc.want == nil) {
			t.Errorf("%s: argv = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A command or price that could only read as an unavailable row is refused when
// the dashboard starts.
func TestJevSpendArgvRefusesWhatCannotRun(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		cmd   string
		price float64
	}{
		"unbalanced quote": {`"gt jev spend`, 42},
		"zero price":       {defaultJevSpendCmd, 0},
		"negative price":   {defaultJevSpendCmd, -1},
		"infinite price":   {defaultJevSpendCmd, math.Inf(1)},
		"NaN price":        {defaultJevSpendCmd, math.NaN()},
	}
	for name, tc := range cases {
		if got, err := jevSpendArgv(tc.cmd, tc.price); err == nil {
			t.Errorf("%s: argv = %q, want an error", name, got)
		}
	}
}

// An empty command needs no price, so a zero price beside it is not an error.
func TestJevSpendArgvEmptyCommandIgnoresPrice(t *testing.T) {
	t.Parallel()

	if got, err := jevSpendArgv("", 0); err != nil || got != nil {
		t.Errorf("argv = %q, err = %v, want neither", got, err)
	}
}

// fakeJevRun stands in for the process: it records the argv it was asked to
// run and answers with out and err.
func fakeJevRun(out string, err error, got *[]string) func(context.Context, []string) ([]byte, error) {
	return func(_ context.Context, argv []string) ([]byte, error) {
		if got != nil {
			*got = argv
		}
		return []byte(out), err
	}
}

func TestDashboardJevSpendFoldsTheCommandsJSON(t *testing.T) {
	t.Parallel()

	const report = `{"calls":14,"input_tokens":900,"output_tokens":100,"usd":1.75,"lines_skipped":0,
 "by_caller":{"deacon":{"calls":4,"usd":0.25},"overseer":{"calls":10,"usd":1.5}}}`
	argv, err := jevSpendArgv(defaultJevSpendCmd, 42)
	if err != nil {
		t.Fatal(err)
	}
	const wantRan = "gt jev spend --json --hours 24 --usd-per-billion-tokens 42"
	var ran []string
	read := dashboardJevSpendWith(argv, fakeJevRun(report, nil, &ran))
	if read == nil {
		t.Fatal("a configured command has no reader")
	}
	j := read()
	if j == nil {
		t.Fatal("the command's report read as a failure")
	}
	if j.Calls != 14 || j.USD != 1.75 || j.TopCaller != "overseer" || j.TopCallerCalls != 10 || j.TopCallerUSD != 1.5 || j.Window != "24h" {
		t.Errorf("row = %+v", *j)
	}
	if strings.Join(ran, " ") != wantRan {
		t.Errorf("ran %q, want the default command with its price, %q", ran, wantRan)
	}
}

// A command that fails or prints something else reads as nil, the failed read
// the hub shows as unavailable.
func TestDashboardJevSpendFailureReadsAsNil(t *testing.T) {
	t.Parallel()

	cases := map[string]func(context.Context, []string) ([]byte, error){
		"exits non-zero": fakeJevRun(`{"calls":1,"usd":1}`, errors.New("exit status 3"), nil),
		"prints nothing": fakeJevRun("", nil, nil),
		"prints text":    fakeJevRun("gt: unknown command", nil, nil),
		"prints no usd":  fakeJevRun(`{"calls":1}`, nil, nil),
		"times out":      fakeJevRun("", context.DeadlineExceeded, nil),
	}
	for name, run := range cases {
		if got := dashboardJevSpendWith([]string{"gt", "jev", "spend"}, run)(); got != nil {
			t.Errorf("%s: read %+v, want a failed read", name, *got)
		}
	}
}

// A command that is not on the machine is a failed read, not a panic: the real
// runner returns the start error.
func TestDashboardJevSpendMissingCommandReadsAsNil(t *testing.T) {
	t.Parallel()

	if got := dashboardJevSpend([]string{"gt-no-such-jev-command-xyz", "jev", "spend"})(); got != nil {
		t.Errorf("a missing command read %+v, want a failed read", *got)
	}
}

// An empty command leaves the Jev row out, like an unset DeepSeek command.
func TestDashboardJevSpendEmptyCommandIsNoReader(t *testing.T) {
	t.Parallel()

	if dashboardJevSpend(strings.Fields("  ")) != nil {
		t.Error("an empty --jev-spend-cmd still has a reader")
	}
}

// writeJevCache writes a cache document to the town's cache path through the
// production writer.
func writeJevCache(t *testing.T, town string, doc scorecard.CacheDoc) {
	t.Helper()
	if err := (scorecard.Cache{}).Write(scorecard.CachePath(town), doc); err != nil {
		t.Fatalf("writing the scorecard cache: %v", err)
	}
}

// The panel reads the cache the refresh wrote: each block becomes its line,
// with the report's counts and questions, its scored_at, and whether the
// refresh failed and kept a previous block. It starts no process: the reader
// takes a town root, not a command.
func TestDashboardJevScorecardReadsTheCachedScorecard(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	scored := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)
	base, precision := 0.25, 0.625
	doc := scorecard.CacheDoc{ScoredAt: scored.Add(5 * time.Hour), Since: "7d", Batteries: []scorecard.BatteryBlock{
		{Name: "finding", ScoredAt: scored, Report: &scorecard.Report{
			Battery: "finding", Items: 212, Labeled: 140, LinesSkipped: 3,
			Questions: []scorecard.QuestionReport{
				{ID: "fix_worthy", Verdict: "earning", Labeled: 140, BaseRate: &base, Precision: &precision},
				{ID: "any_duplicate", Verdict: "not-yet", Labeled: 40},
			},
		}},
		{Name: "testsel", ScoredAt: scored, Report: &scorecard.Report{
			Battery: "testsel", Items: 39858, Labeled: 30000,
			Questions: []scorecard.QuestionReport{{ID: "affects_tests", Verdict: "earning"}},
		}},
	}}
	writeJevCache(t, town, doc)

	sc := dashboardJevScorecard(town)()
	if sc == nil {
		t.Fatal("the cache read as a failure")
	}
	if sc.NoScorecard || sc.Window != "7d" || len(sc.Batteries) != 2 {
		t.Fatalf("scorecard = %+v", *sc)
	}
	b := sc.Batteries[0]
	if b.Name != "finding" || b.Items != 212 || b.Labeled != 140 || b.Unanswered != 3 ||
		b.Failed || b.Kept || b.ScoredAt == nil || !b.ScoredAt.Equal(scored) {
		t.Errorf("finding line = %+v", b)
	}
	if len(b.Questions) != 2 || b.Questions[0].ID != "fix_worthy" || b.Questions[0].Verdict != "earning" ||
		b.Questions[0].Labeled != 140 || b.Questions[0].BaseRate == nil || *b.Questions[0].BaseRate != base ||
		b.Questions[0].Precision == nil || *b.Questions[0].Precision != precision || b.Questions[1].BaseRate != nil {
		t.Errorf("finding questions = %+v", b.Questions)
	}
	if tsel := sc.Batteries[1]; tsel.Unit != "packages" || tsel.Items != 39858 {
		t.Errorf("testsel line = %+v, want a package count", tsel)
	}
	if want := (dashboard.JevTotals{Batteries: 1, Items: 212, Labeled: 140, Earning: 2, NotYet: 1}); sc.Totals != want {
		t.Errorf("Totals = %+v, want %+v", sc.Totals, want)
	}
}

// The reader's only input is the town's cache file: it reads that one path, so
// there is no command to start.
func TestDashboardJevScorecardReadsOnlyTheCachePath(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	scored := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)
	writeJevCache(t, town, scorecard.CacheDoc{ScoredAt: scored, Since: "7d", Batteries: []scorecard.BatteryBlock{
		{Name: "finding", ScoredAt: scored, Report: &scorecard.Report{Battery: "finding", Items: 2}},
	}})
	var read []string
	cache := scorecard.Cache{ReadFile: func(path string) ([]byte, error) {
		read = append(read, path)
		return os.ReadFile(path)
	}}
	if sc := dashboardJevScorecardWith(town, cache)(); sc == nil {
		t.Fatal("the cache read as a failure")
	}
	if want := []string{scorecard.CachePath(town)}; len(read) != 1 || read[0] != want[0] {
		t.Errorf("read %q, want only %q", read, want)
	}
}

// No refresh has run yet: the cache is missing, and the panel says how to make
// one rather than reading as a failed read.
func TestDashboardJevScorecardMissingCacheSaysNoScorecard(t *testing.T) {
	t.Parallel()

	sc := dashboardJevScorecard(t.TempDir())()
	if sc == nil || !sc.NoScorecard || len(sc.Batteries) != 0 {
		t.Fatalf("an empty town read %+v, want the no-scorecard value", sc)
	}
}

// A cache that cannot be read, or does not parse, is a failed read: the hub
// keeps the last good panel over it.
func TestDashboardJevScorecardUnreadableCacheIsAFailedRead(t *testing.T) {
	t.Parallel()

	boom := errors.New("permission denied")
	cache := scorecard.Cache{ReadFile: func(string) ([]byte, error) { return nil, boom }}
	if got := dashboardJevScorecardWith(t.TempDir(), cache)(); got != nil {
		t.Errorf("an unreadable cache read %+v, want a failed read", *got)
	}

	town := t.TempDir()
	path := scorecard.CachePath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("making the cache directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("not a scorecard"), 0o644); err != nil {
		t.Fatalf("writing the corrupt cache: %v", err)
	}
	if got := dashboardJevScorecard(town)(); got != nil {
		t.Errorf("a corrupt cache read %+v, want a failed read", *got)
	}
}

// A block a failed refresh kept draws its report, marked kept, with the time it
// was scored. A battery that never scored has a placeholder with no age.
func TestDashboardJevScorecardKeepsTheKeptBlockAndLeavesTheRest(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	good := time.Date(2026, 11, 5, 4, 30, 0, 0, time.UTC)
	keptSince := good
	doc := scorecard.CacheDoc{ScoredAt: good.Add(24 * time.Hour), Since: "7d", Batteries: []scorecard.BatteryBlock{
		{Name: "finding", ScoredAt: good, Failed: true, Kept: true, KeptSince: &keptSince, Error: "bd: refused",
			Report: &scorecard.Report{Battery: "finding", Items: 5, Labeled: 4}},
		{Name: "landing", ScoredAt: good.Add(24 * time.Hour), Failed: true, Error: "bd: refused"},
	}}
	writeJevCache(t, town, doc)

	sc := dashboardJevScorecard(town)()
	if sc == nil {
		t.Fatal("the cache read as a failure")
	}
	kept := sc.Batteries[0]
	if !kept.Failed || !kept.Kept || kept.Items != 5 || kept.Labeled != 4 || kept.ScoredAt == nil || !kept.ScoredAt.Equal(good) {
		t.Errorf("finding = %+v, want its kept block with the last good score time", kept)
	}
	lost := sc.Batteries[1]
	if !lost.Failed || lost.Kept || lost.Items != 0 || lost.ScoredAt != nil {
		t.Errorf("landing = %+v, want a placeholder with no score", lost)
	}
	if want := []string{"finding", "landing"}; strings.Join(sc.Unavailable, ",") != strings.Join(want, ",") {
		t.Errorf("Unavailable = %q, want %q", sc.Unavailable, want)
	}
	if want := (dashboard.JevTotals{Batteries: 1, Items: 5, Labeled: 4}); sc.Totals != want {
		t.Errorf("Totals = %+v, want the kept line counted", sc.Totals)
	}
}

// sourceFunc returns the source of the named function, from its func line to
// the next top-level one.
func sourceFunc(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "func "+name+"(")
	if start < 0 {
		t.Fatalf("the source has no func %s", name)
	}
	rest := src[start:]
	if end := strings.Index(rest[1:], "\nfunc "); end >= 0 {
		return rest[:end+1]
	}
	return rest
}

// The scorecard comes from the cache, and nothing that builds the panel starts
// a process: the dashboard package has no exec at all, and the reader here has
// none. The flag and argv seams that ran gt jev scorecard per battery are gone.
func TestDashboardJevScorecardWiringStartsNoProcess(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	self, err := os.ReadFile(filepath.Join(dir, "dashboard.go"))
	if err != nil {
		t.Fatalf("reading dashboard.go: %v", err)
	}
	reader := sourceFunc(t, string(self), "dashboardJevScorecardWith")
	if strings.Contains(reader, "exec.") {
		t.Errorf("dashboardJevScorecardWith starts a process:\n%s", reader)
	}
	if !strings.Contains(reader, "scorecard.CachePath(") || !strings.Contains(reader, "cache.Read(") {
		t.Error("the scorecard reader does not read the cache file")
	}
	for _, banned := range []string{"--jev-scorecard-cmd", "jevScorecardArgv", "readJevBattery", "dashboardJevScorecardTimeout", `"gt jev scorecard --json`} {
		if strings.Contains(string(self), banned) {
			t.Errorf("dashboard.go still carries %q, the per-battery run the cache replaced", banned)
		}
	}

	files, err := filepath.Glob(filepath.Join(dir, "..", "dashboard", "*.go"))
	if err != nil {
		t.Fatalf("listing the dashboard package: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("found no dashboard package files")
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		if strings.Contains(string(src), "os/exec") {
			t.Errorf("%s imports os/exec: the dashboard package starts no process", f)
		}
	}
}

// The dashboard leaves the packages battery out of the JEV title line by name,
// so its name must be the one the scorecard registry gives the test-selection
// battery.
func TestDashboardJevPackagesBatteryIsTheTestselBattery(t *testing.T) {
	t.Parallel()

	if dashboard.JevPackagesBattery != scorecard.TestselBattery {
		t.Errorf("dashboard.JevPackagesBattery = %q, want the scorecard's %q", dashboard.JevPackagesBattery, scorecard.TestselBattery)
	}
}
