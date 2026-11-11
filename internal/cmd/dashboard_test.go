package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	gtevents "github.com/steveyegge/gastown/internal/events"
)

func TestDashboardClassName(t *testing.T) {
	t.Parallel()
	for c, want := range map[tailClass]string{
		tailClassFailure: "failure", tailClassWarning: "warning", tailClassSuccess: "success",
		tailClassLanding: "landing", tailClassDispatch: "dispatch", tailClassRestart: "restart", tailClassPlain: "plain",
	} {
		if got := dashboardClassName(c); got != want {
			t.Errorf("class %d = %q, want %q", c, got, want)
		}
	}
}

// spendFake is a spend runner that records each argv it is called with and
// prints out. No test in this package runs a real process.
func spendFake(calls *[][]string, out string) func(context.Context, []string) ([]byte, error) {
	return func(_ context.Context, argv []string) ([]byte, error) {
		*calls = append(*calls, argv)
		return []byte(out), nil
	}
}

// spendUnused is the runner a test passes where no command may run: calling it
// is the failure.
func spendUnused(_ context.Context, argv []string) ([]byte, error) {
	return nil, fmt.Errorf("the spend command %v was run, want none", argv)
}

// writeSpendLedger writes the two-row ledger the in-process report reads under home.
func writeSpendLedger(t *testing.T, home string) {
	t.Helper()
	dir := spendDirIn(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := "2026-11-09T01:10:00 balance=51.50 granted=15.00 topped=36.50\n" +
		"2026-11-09T01:12:00 balance=51.00 granted=15.00 topped=36.00\n"
	if err := os.WriteFile(filepath.Join(dir, spendLedgerName), []byte(ledger), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSpendPrefersFlag(t *testing.T) {
	t.Parallel()
	var calls [][]string
	home := t.TempDir()
	homeFn := func() (string, error) { return home, nil }
	spend, reason := resolveSpendIn("/usr/bin/spend-report --json", homeFn, os.Stat, spendFake(&calls, `{"from":"flag"}`), nil)
	if spend == nil || reason != "" {
		t.Fatalf("flag not honored: spend nil %v, reason %q", spend == nil, reason)
	}
	if got := string(spend()); got != `{"from":"flag"}` {
		t.Errorf("flag's command output = %q, want the runner's JSON", got)
	}
	if want := [][]string{{"/usr/bin/spend-report", "--json"}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("runner calls = %v, want %v", calls, want)
	}
}

// The command runs through the injected runner. With --spend-cmd set, or the tool
// found, the runner is called with that command and its JSON is served. With
// neither, the in-process ledger report is served and the runner is not called.
func TestDashboardSpendRunnerSeam(t *testing.T) {
	t.Parallel()
	const printed = `{"from":"runner"}`

	withTool := t.TempDir()
	writeSpendLedger(t, withTool)
	tool := filepath.Join(withTool, ".claude", "tools", "deepseek-spend.py")
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ledgerOnly := t.TempDir()
	writeSpendLedger(t, ledgerOnly)
	withToolFn := func() (string, error) { return withTool, nil }
	ledgerOnlyFn := func() (string, error) { return ledgerOnly, nil }

	var calls [][]string
	spend, _ := resolveSpendIn("/usr/bin/spend-report --json", withToolFn, os.Stat, spendFake(&calls, printed), nil)
	if got := string(spend()); got != printed {
		t.Errorf("--spend-cmd: served %q, want the runner's JSON", got)
	}
	if want := [][]string{{"/usr/bin/spend-report", "--json"}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("--spend-cmd: runner calls = %v, want %v", calls, want)
	}

	calls = nil
	spend, _ = resolveSpendIn("", withToolFn, os.Stat, spendFake(&calls, printed), nil)
	if got := string(spend()); got != printed {
		t.Errorf("tool present: served %q, want the runner's JSON", got)
	}
	if want := [][]string{{tool, "--json"}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("tool present: runner calls = %v, want %v", calls, want)
	}

	calls = nil
	spend, _ = resolveSpendIn("", ledgerOnlyFn, os.Stat, spendFake(&calls, printed), nil)
	out := spend()
	var report struct {
		Samples int `json:"samples"`
	}
	if err := json.Unmarshal(out, &report); err != nil || report.Samples != 2 {
		t.Errorf("neither: served %s (err %v), want the in-process report of 2 rows", out, err)
	}
	if len(calls) != 0 {
		t.Errorf("neither: runner was called with %v, want no call", calls)
	}
}

// A command that fails, or prints something that is not JSON, gives no report.
func TestDashboardSpendRefusesAFailedCommand(t *testing.T) {
	t.Parallel()
	failing := func(context.Context, []string) ([]byte, error) {
		return []byte(`{"ok":true}`), errors.New("exit status 1")
	}
	notJSON := func(context.Context, []string) ([]byte, error) { return []byte("balance: 51.50"), nil }
	for name, run := range map[string]func(context.Context, []string) ([]byte, error){"exits non-zero": failing, "prints no JSON": notJSON} {
		if out := dashboardSpend([]string{"/usr/bin/spend-report"}, run)(); out != nil {
			t.Errorf("%s: served %s, want no report", name, out)
		}
	}
}

// A missing ledger is no spend source. An empty ledger is a zero report with no
// balance. A ledger or events log that cannot be read is a failed read: no report,
// and the error's first line goes to the log without the path.
func TestDashboardSpendLedgerStates(t *testing.T) {
	t.Parallel()
	noLedger := t.TempDir()
	homeFn := func() (string, error) { return noLedger, nil }
	if spend, reason := resolveSpendIn("", homeFn, os.Stat, spendUnused, nil); spend != nil || reason != dashboard.NotInClusterReason {
		t.Errorf("missing ledger: spend nil %v, reason %q; want no source and %q", spend == nil, reason, dashboard.NotInClusterReason)
	}

	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, spendLedgerName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var zero struct {
		Samples int      `json:"samples"`
		Balance *float64 `json:"balance"`
		Live    struct {
			OK bool `json:"ok"`
		} `json:"live"`
	}
	out := dashboardSpendLedger(empty, nil)()
	if err := json.Unmarshal(out, &zero); err != nil {
		t.Fatalf("empty ledger: report %q is not JSON: %v", out, err)
	}
	if zero.Samples != 0 || zero.Balance != nil || zero.Live.OK {
		t.Errorf("empty ledger: report %s, want a zero report with no balance", out)
	}

	failed := t.TempDir()
	if err := os.Mkdir(filepath.Join(failed, spendLedgerName), 0o755); err != nil {
		t.Fatal(err)
	}
	var logged []string
	whyLog := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	if out := dashboardSpendLedger(failed, whyLog)(); out != nil {
		t.Errorf("failed read: got report %s, want none", out)
	}
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "spend read failed: ") {
		t.Fatalf("failed read logged %q, want one spend read failure", logged)
	}
	if why := strings.TrimPrefix(logged[0], "spend read failed: "); strings.Contains(why, "\n") || strings.Contains(why, failed) || !strings.Contains(why, "is a directory") {
		t.Errorf("failed read reason = %q, want the error's first line with no path", why)
	}
}

// A ledger or events log that cannot be read, or a report that cannot be encoded,
// is no report: the dashboard shows the read failed, not a report built from part
// of the files.
func TestDashboardSpendLedgerRefusesUnreadableFiles(t *testing.T) {
	t.Parallel()
	ledgerIsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(ledgerIsDir, spendLedgerName), 0o755); err != nil {
		t.Fatal(err)
	}
	eventsIsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(eventsIsDir, spendLedgerName), []byte("2026-11-09T01:12:00 balance=51.50\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(eventsIsDir, spendEventsName), 0o755); err != nil {
		t.Fatal(err)
	}
	notEncodable := t.TempDir()
	if err := os.WriteFile(filepath.Join(notEncodable, spendLedgerName), []byte("2026-11-09T01:12:00 balance=NaN\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"ledger unreadable":    ledgerIsDir,
		"events unreadable":    eventsIsDir,
		"report not encodable": notEncodable,
	}
	for name, dir := range cases {
		if out := dashboardSpendLedger(dir, nil)(); out != nil {
			t.Errorf("%s: got report %s, want none", name, out)
		}
	}
}

// A town run where the spend tool and the balance ledger exist only on the Mac
// says so, as the reason, and a source that is there but does not stat is still
// run, so its failure is a failed read rather than an absent pane.
func TestDashboardSpendAbsentSaysNotInCluster(t *testing.T) {
	t.Parallel()
	noHome := func() (string, error) { return "", errors.New("no home") }
	home := func() (string, error) { return t.TempDir(), nil }
	absent := func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	present := func(string) (fs.FileInfo, error) { return nil, nil }
	denied := func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }
	noTool := func(p string) (fs.FileInfo, error) {
		if strings.HasSuffix(p, "deepseek-spend.py") {
			return nil, fs.ErrNotExist
		}
		return nil, nil
	}
	noLedger := func(p string) (fs.FileInfo, error) {
		if strings.HasSuffix(p, spendLedgerName) {
			return nil, fs.ErrNotExist
		}
		return nil, nil
	}

	if spend, reason := resolveSpendIn("", noHome, present, spendUnused, nil); spend != nil || reason != dashboard.NotInClusterReason {
		t.Errorf("no home: spend nil %v, reason %q; want no source and %q", spend == nil, reason, dashboard.NotInClusterReason)
	}
	if spend, reason := resolveSpendIn("", home, absent, spendUnused, nil); spend != nil || reason != dashboard.NotInClusterReason {
		t.Errorf("no ledger or tool: spend nil %v, reason %q; want no source and %q", spend == nil, reason, dashboard.NotInClusterReason)
	}
	if spend, reason := resolveSpendIn("", home, noLedger, spendUnused, nil); spend == nil || reason != "" {
		t.Errorf("tool only: spend nil %v, reason %q; want the tool and no reason", spend == nil, reason)
	}
	if spend, reason := resolveSpendIn("", home, noTool, spendUnused, nil); spend == nil || reason != "" {
		t.Errorf("ledger only: spend nil %v, reason %q; want the ledger and no reason", spend == nil, reason)
	}
	if spend, reason := resolveSpendIn("", home, present, spendUnused, nil); spend == nil || reason != "" {
		t.Errorf("tool and ledger: spend nil %v, reason %q; want a source and no reason", spend == nil, reason)
	}
	if spend, reason := resolveSpendIn("", home, denied, spendUnused, nil); spend == nil || reason != "" {
		t.Errorf("tool unreadable: spend nil %v, reason %q; want a source and no reason", spend == nil, reason)
	}

	if read := dashboardSpendHoursIn(noHome)(); read.Reason != dashboard.NotInClusterReason || read.Hours != nil {
		t.Errorf("no home ledger: %+v; want the reason and no hours", read)
	}
	missing := filepath.Join(t.TempDir(), "balance.log")
	if read := dashboardSpendHoursFrom(missing)(); read.Reason != dashboard.NotInClusterReason || read.Hours != nil {
		t.Errorf("no ledger: %+v; want the reason and no hours", read)
	}
	ledger := filepath.Join(t.TempDir(), "balance.log")
	if err := os.WriteFile(ledger, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if read := dashboardSpendHoursFrom(ledger)(); read.Reason != "" || len(read.Hours) == 0 {
		t.Errorf("ledger present: %+v; want hours and no reason", read)
	}
	if h := dashboard.NewHub(dashboard.Config{SpendReason: dashboard.NotInClusterReason}); h.State().SpendReason != dashboard.NotInClusterReason {
		t.Errorf("hub spend reason = %q, want %q", h.State().SpendReason, dashboard.NotInClusterReason)
	}
}

func TestDashboardEntryDropsClockAndKeepsTitle(t *testing.T) {
	t.Parallel()
	v := tailView{Trim: true}
	e := dashboardEntry(v, tailLine{Rig: "gastown", Kind: tailKindDaemon, Text: "landed gt-x on main", Title: "Fix the thing"})
	if e.Text != "landed gt-x on main · Fix the thing" || e.Class != "success" || e.Rig != "gastown" {
		t.Errorf("entry = %+v", e)
	}
}

// dashWhy is the reason the page is sent: the error's first line, every path
// cut to its file name, and held to dashWhyMax runes (gt-6qc3nw.2).
func TestDashWhyNamesTheFirstLineWithNoPath(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"nil error":            {nil, ""},
		"first line only":      {errors.New("bd list failed\n  stack line"), "bd list failed"},
		"path cut to its file": {errors.New("open /sandbox/gt/town/.beads/routes.jsonl: no such file"), "open routes.jsonl: no such file"},
		"held to the limit":    {errors.New(strings.Repeat("é", dashWhyMax+50)), strings.Repeat("é", dashWhyMax)},
		"no first line text":   {errors.New("\n  stack line"), dashWhyNoText},
		"empty error":          {errors.New(""), dashWhyNoText},
	} {
		if got := dashWhy(c.err); got != c.want {
			t.Errorf("%s: dashWhy = %q, want %q", name, got, c.want)
		}
	}
}

// A reason is logged and drawn as plain text: control characters and newlines
// are gone, a secret the scrubber knows is its mask, and the reason stops at
// dashWhyMax characters, in the log line and in the page's data alike
// (gt-6qc3nw.2).
func TestWhyTextIsSafe(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ in, want string }{
		"control characters go": {"bd \x1b[2J list\x07 failed\x7f", "bd [2J list failed"},
		"newlines are spaces":   {"bd list\r\nfailed", "bd list failed"},
		"token is masked":       {"login rejected token=hunter2", "login rejected token=[REDACTED:assignment]"},
		"url credentials go":    {"clone https://bob:s3cret@git.example.com/repo.git failed", "clone https://[REDACTED:url-userinfo]@git.example.com/repo.git failed"},
		"path is its file":      {"open /sandbox/gt/town/routes.jsonl: no such file", "open routes.jsonl: no such file"},
		"held to the limit":     {strings.Repeat("é", dashWhyMax+50), strings.Repeat("é", dashWhyMax)},
	} {
		if got := dashWhyText(c.in); got != c.want {
			t.Errorf("%s: dashWhyText(%q) = %q, want %q", name, c.in, got, c.want)
		}
	}

	var logged []string
	why := newDashWhyLog(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	town := t.TempDir()
	hostile := errors.New("bd list \x1b[2J failed\ntoken=hunter2 " + strings.Repeat("x", 300))
	var s dashboard.Summary
	dashboardPolecatsWith(dashPolecatReaders{
		kept: func(string) (map[string]time.Time, error) { return nil, nil },
		seats: func(_ string, _ []string, _ bool, _ *dashSeatCache, _ *gtevents.DispatchReader, _ sessionLister, _ map[string]time.Time) ([]dashSeat, error) {
			return nil, hostile
		},
	}, &s, town, nil, nil, true, nil, newDashLandings(town), newDashSeatCache(), gtevents.NewDispatchReader(town), fakeSessionLister{})
	why.summary(&s)

	if got := len([]rune(s.SeatsWhy)); got > dashWhyMax {
		t.Errorf("page reason is %d characters, want at most %d", got, dashWhyMax)
	}
	for _, text := range []string{s.SeatsWhy, logged[0]} {
		if strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			t.Errorf("reason %q carries a control character", text)
		}
		if strings.Contains(text, "hunter2") {
			t.Errorf("reason %q carries the secret", text)
		}
	}
	if want := dashboard.SourceSeats + " read failed: " + s.SeatsWhy; len(logged) != 1 || logged[0] != want {
		t.Errorf("logged %q, want one line %q", logged, want)
	}
}

// A failed read's reason is logged once per change: the same failure on every
// poll is one line, a different failure logs again, and a read that comes back
// logs again. The questions reader logs through the same log (gt-6qc3nw.2).
func TestDashboardWhyLoggedOncePerChange(t *testing.T) {
	t.Parallel()
	var lines []string
	why := newDashWhyLog(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	town := t.TempDir()
	poll := func(err error) {
		var s dashboard.Summary
		dashboardPolecatsWith(dashPolecatReaders{
			kept: func(string) (map[string]time.Time, error) { return nil, nil },
			seats: func(_ string, _ []string, _ bool, _ *dashSeatCache, _ *gtevents.DispatchReader, _ sessionLister, _ map[string]time.Time) ([]dashSeat, error) {
				return nil, err
			},
		}, &s, town, nil, nil, true, nil, newDashLandings(town), newDashSeatCache(), gtevents.NewDispatchReader(town), fakeSessionLister{})
		why.summary(&s)
	}
	breaker := errors.New("dolt circuit breaker is open: server appears down, failing fast")
	for range 3 {
		poll(breaker)
	}
	if len(lines) != 1 {
		t.Fatalf("three polls of one failure logged %d lines %q, want 1", len(lines), lines)
	}
	poll(errors.New("bd list: connection refused"))
	if len(lines) != 2 {
		t.Fatalf("a different failure logged %d lines total %q, want 2", len(lines), lines)
	}
	poll(nil)
	if len(lines) != 3 {
		t.Fatalf("a read that came back logged %d lines total %q, want 3", len(lines), lines)
	}

	questions := &dashQuestionsReader{
		list: func() ([]*beads.Issue, error) { return nil, breaker },
		why:  why,
	}
	for range 3 {
		questions.read(questionsNow)
	}
	if len(lines) != 4 || !strings.Contains(lines[3], "questions") {
		t.Errorf("three questions polls of one failure logged %q, want one questions line after the seats lines", lines)
	}
}
