package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The line's clock: every fixture is written against this instant.
var appNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// appFixture writes the markers and the ledgers, keyed by app name, under two
// temporary directories and returns a reader over them. A nil map leaves its
// directory out altogether.
func appFixture(t *testing.T, markers, ledgers map[string]string) *AppPowerReader {
	t.Helper()
	root := t.TempDir()
	write := func(dirName, suffix string, files map[string]string) string {
		dir := filepath.Join(root, dirName)
		if files == nil {
			return dir
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for app, body := range files {
			if err := os.WriteFile(filepath.Join(dir, app+suffix), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	return NewAppPowerReader(write("off", appOffSuffix, markers), write("releases", appLedgerSuffix, ledgers))
}

func appRow(t *testing.T, p *AppPower, app string) AppPowerRow {
	t.Helper()
	if p == nil {
		t.Fatal("the line is nil")
	}
	for _, r := range p.Apps {
		if r.App == app {
			return r
		}
	}
	t.Fatalf("no row for %q in %+v", app, p.Apps)
	return AppPowerRow{}
}

const markerSince = "2026-10-14T09:30:00Z"

// An app with no marker is on, an app with one is off in the marker's
// environment, and a marker without env= is the legacy form that means all. Each
// carries the marker's since, by and reason.
func TestAppPowerReadsOnAndTheThreeOffEnvironments(t *testing.T) {
	t.Parallel()

	p := appFixture(t, map[string]string{
		"stg":    "since=" + markerSince + " by=sloan env=staging reason=staging drill\n",
		"prd":    "since=" + markerSince + " by=sloan env=prod reason=cost freeze\n",
		"all":    "since=" + markerSince + " by=sloan env=all reason=parked\n",
		"legacy": "since=" + markerSince + " by=sloan reason=old marker with spaces\n",
	}, map[string]string{
		"live": `{"time":"2026-10-14T11:00:00Z","app":"live","action":"skip","reason":"no_green_commit"}` + "\n",
	}).Read(appNow)

	for _, tc := range []struct {
		app, state, env, reason string
	}{
		{"live", AppOn, "", ""},
		{"stg", AppOff, AppEnvStaging, "staging drill"},
		{"prd", AppOff, AppEnvProd, "cost freeze"},
		{"all", AppOff, AppEnvAll, "parked"},
		{"legacy", AppOff, AppEnvAll, "old marker with spaces"},
	} {
		r := appRow(t, p, tc.app)
		if r.State != tc.state || r.Env != tc.env || r.Reason != tc.reason {
			t.Errorf("%s: state %q env %q reason %q, want %q %q %q", tc.app, r.State, r.Env, r.Reason, tc.state, tc.env, tc.reason)
		}
		if tc.state == AppOff {
			if !r.Since.Equal(time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)) || r.By != "sloan" {
				t.Errorf("%s: since %v by %q, want the marker's", tc.app, r.Since, r.By)
			}
		}
	}
	if len(p.Apps) != 5 {
		t.Errorf("%d rows, want one per app in either directory", len(p.Apps))
	}
	for i := 1; i < len(p.Apps); i++ {
		if p.Apps[i-1].App > p.Apps[i].App {
			t.Errorf("rows are not sorted by app: %q before %q", p.Apps[i-1].App, p.Apps[i].App)
		}
	}
}

// The release bot's latest ledger line rides the app's row: its time, action
// and reason, taken from the last line only.
func TestAppPowerCarriesTheLastLedgerLine(t *testing.T) {
	t.Parallel()

	ledger := `{"time":"2026-10-14T08:00:00Z","app":"abc","action":"deploy","reason":"ok"}` + "\n" +
		`{"time":"2026-10-14T11:45:00Z","app":"abc","action":"skip","reason":"app_off"}` + "\n\n"
	p := appFixture(t, map[string]string{}, map[string]string{"abc": ledger}).Read(appNow)

	r := appRow(t, p, "abc")
	if r.State != AppOn {
		t.Fatalf("state %q, want on", r.State)
	}
	if r.Release == nil || r.Release.Action != "skip" || r.Release.Reason != "app_off" ||
		!r.Release.At.Equal(time.Date(2026, 10, 14, 11, 45, 0, 0, time.UTC)) {
		t.Errorf("release %+v, want the last line", r.Release)
	}
}

// An empty ledger, and an app with a marker but no ledger at all, have no
// release line and say nothing about it.
func TestAppPowerEmptyAndMissingLedgers(t *testing.T) {
	t.Parallel()

	p := appFixture(t, map[string]string{
		"noledger": "since=" + markerSince + " by=sloan env=prod reason=x\n",
	}, map[string]string{"empty": "", "blank": "\n \n"}).Read(appNow)

	for _, app := range []string{"empty", "blank", "noledger"} {
		r := appRow(t, p, app)
		if r.Release != nil || r.ReleaseNote != "" {
			t.Errorf("%s: release %+v note %q, want none", app, r.Release, r.ReleaseNote)
		}
	}
	if r := appRow(t, p, "noledger"); r.State != AppOff {
		t.Errorf("a marker without a ledger reads %q, want off", r.State)
	}
}

// A marker that does not parse is still off, never on: the file is there to say
// the app is stopped. The row says the marker was unreadable and invents no
// environment.
func TestAppPowerUnparsableMarkerIsOffNeverOn(t *testing.T) {
	t.Parallel()

	p := appFixture(t, map[string]string{
		"empty":    "",
		"garbage":  "not a marker\n",
		"badsince": "since=yesterday by=sloan env=prod reason=x\n",
		"badenv":   "since=" + markerSince + " by=sloan env=dev reason=x\n",
	}, map[string]string{}).Read(appNow)

	for _, app := range []string{"empty", "garbage", "badsince", "badenv"} {
		r := appRow(t, p, app)
		if r.State != AppOff || r.Note != appMarkerUnreadable || r.Env != "" {
			t.Errorf("%s: state %q note %q env %q, want off (unreadable marker)", app, r.State, r.Note, r.Env)
		}
	}
}

// Fields the reader does not know are skipped, and the known ones are read
// wherever they sit before the reason.
func TestAppPowerSkipsUnknownMarkerFields(t *testing.T) {
	t.Parallel()

	p := appFixture(t, map[string]string{
		"a": "host=mini env=staging since=" + markerSince + " ticket=GT-1 by=sloan reason=drill, then back\n",
	}, map[string]string{}).Read(appNow)

	r := appRow(t, p, "a")
	if r.State != AppOff || r.Env != AppEnvStaging || r.By != "sloan" || r.Reason != "drill, then back" {
		t.Errorf("row %+v, want the known fields read around the unknown ones", r)
	}
}

// A directory the reader cannot look in is never read as every app being on:
// with the markers directory unreadable, each app the ledgers name is unknown,
// and with nothing to name one the line says so itself. The reader reports
// nothing at all only when neither directory is there.
func TestAppPowerUnreadableDirectoryIsUnknownNeverOn(t *testing.T) {
	t.Parallel()

	// A markers path that is a file, not a directory: it exists and cannot be listed.
	root := t.TempDir()
	notDir := filepath.Join(root, "off")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(root, "releases")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "abc.jsonl"), []byte(`{"time":"2026-10-14T11:00:00Z","action":"skip","reason":"app_off"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewAppPowerReader(notDir, rel).Read(appNow)
	r := appRow(t, p, "abc")
	if r.State != AppUnknown || r.Note != appOffDirUnreadable {
		t.Errorf("state %q note %q, want unknown (off directory unreadable)", r.State, r.Note)
	}
	if r.Release == nil || r.Release.Reason != "app_off" {
		t.Errorf("the ledger line is lost with the markers: %+v", r.Release)
	}

	// Nothing names an app: the line carries the reason itself.
	p = NewAppPowerReader(notDir, filepath.Join(root, "gone")).Read(appNow)
	if p == nil || len(p.Apps) != 0 || p.Note != appOffDirUnreadable {
		t.Errorf("line %+v, want no rows and the note", p)
	}

	// Neither directory there: no cloud setup, nothing to draw.
	if p := NewAppPowerReader(filepath.Join(root, "x"), filepath.Join(root, "y")).Read(appNow); p != nil {
		t.Errorf("two missing directories read as %+v, want nothing", p)
	}
}

// A missing markers directory with ledgers beside it is unreadable markers, not
// a fleet that is on.
func TestAppPowerMissingMarkersDirectoryWithLedgersIsUnknown(t *testing.T) {
	t.Parallel()

	p := appFixture(t, nil, map[string]string{"abc": `{"time":"2026-10-14T11:00:00Z","action":"deploy"}` + "\n"}).Read(appNow)
	if r := appRow(t, p, "abc"); r.State != AppUnknown {
		t.Errorf("state %q, want unknown", r.State)
	}
}

// A marker the reader cannot open is unknown, not off with a guessed reason and
// not on.
func TestAppPowerUnreadableMarkerFileIsUnknown(t *testing.T) {
	t.Parallel()

	r := appFixture(t, map[string]string{"locked": "since=" + markerSince + " by=s env=all reason=x\n"}, map[string]string{})
	// A dangling symlink is listed as a file but cannot be opened, for root too.
	path := filepath.Join(r.offDir, "locked.off")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(r.offDir, "missing-target"), path); err != nil {
		t.Fatal(err)
	}
	row := appRow(t, r.Read(appNow), "locked")
	if row.State != AppUnknown || row.Note != appMarkerReadFailed {
		t.Errorf("state %q note %q, want unknown (marker unreadable)", row.State, row.Note)
	}
}

// A FIFO named x.off must not be opened: an open would block until a writer
// appears and stall the Cloud poll for good. The read runs in a goroutine so a
// missing fix fails the test by timeout instead of hanging the suite.
func TestReadSkipsNonRegularMarkerWithoutBlocking(t *testing.T) {
	t.Parallel()

	r := appFixture(t, map[string]string{}, map[string]string{"y": "\n"})
	if err := syscall.Mkfifo(filepath.Join(r.offDir, "x.off"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A FIFO in the ledgers directory is skipped the same way.
	if err := syscall.Mkfifo(filepath.Join(r.releasesDir, "z.jsonl"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan *AppPower, 1)
	go func() { done <- r.Read(appNow) }()
	select {
	case p := <-done:
		x := appRow(t, p, "x")
		if x.State != AppUnknown || x.Note != appMarkerNotAFile {
			t.Errorf("x row state %q note %q, want unknown (%s)", x.State, x.Note, appMarkerNotAFile)
		}
		if z := appRow(t, p, "z"); z.ReleaseNote != appLedgerReadFailed {
			t.Errorf("z row release note %q, want %q", z.ReleaseNote, appLedgerReadFailed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read blocked on a FIFO marker")
	}
}

// A directory named x.off is an entry the reader cannot make sense of, so the
// app is not on.
func TestReadDirectoryMarkerIsNotOn(t *testing.T) {
	t.Parallel()

	r := appFixture(t, map[string]string{}, map[string]string{})
	if err := os.Mkdir(filepath.Join(r.offDir, "x.off"), 0o755); err != nil {
		t.Fatal(err)
	}
	row := appRow(t, r.Read(appNow), "x")
	if row.State == AppOn {
		t.Errorf("x row state %q, want not on", row.State)
	}
	if row.State != AppUnknown || row.Note != appMarkerNotAFile {
		t.Errorf("x row state %q note %q, want unknown (%s)", row.State, row.Note, appMarkerNotAFile)
	}
}

// A symlink to a regular marker still reads as that marker.
func TestAppPowerSymlinkedMarkerIsRead(t *testing.T) {
	t.Parallel()

	r := appFixture(t, map[string]string{"real": "since=" + markerSince + " by=s env=prod reason=r\n"}, map[string]string{})
	if err := os.Symlink(filepath.Join(r.offDir, "real.off"), filepath.Join(r.offDir, "link.off")); err != nil {
		t.Fatal(err)
	}
	if row := appRow(t, r.Read(appNow), "link"); row.State != AppOff || row.Env != AppEnvProd {
		t.Errorf("link row %+v, want off in prod", row)
	}
}

// A ledger line that is not JSON says so on the row and leaves the power state
// alone.
func TestAppPowerUnparsableLedgerLine(t *testing.T) {
	t.Parallel()

	p := appFixture(t, map[string]string{}, map[string]string{"abc": "{not json\n"}).Read(appNow)
	r := appRow(t, p, "abc")
	if r.State != AppOn || r.Release != nil || r.ReleaseNote != appLedgerBadLine {
		t.Errorf("row %+v, want on with a ledger note", r)
	}
}

// Only the end of a ledger is read: a ledger far past the bound still yields its
// last line, and a last line longer than the bound is a named error rather than a
// fragment read as a line.
func TestAppPowerLedgerReadIsBounded(t *testing.T) {
	t.Parallel()

	filler := strings.Repeat(`{"time":"2026-10-01T00:00:00Z","action":"skip","reason":"no_green_commit"}`+"\n", 2000)
	if len(filler) < 10*appLedgerTailBytes {
		t.Fatalf("the filler is %d bytes, too small to prove the bound", len(filler))
	}
	last := `{"time":"2026-10-14T11:59:00Z","action":"deploy","reason":"green"}` + "\n"
	p := appFixture(t, map[string]string{}, map[string]string{
		"big":  filler + last,
		"long": `{"reason":"` + strings.Repeat("x", 2*appLedgerTailBytes) + `"}` + "\n",
	}).Read(appNow)

	if r := appRow(t, p, "big"); r.Release == nil || r.Release.Action != "deploy" || r.Release.Reason != "green" {
		t.Errorf("big ledger: release %+v, want the last line", r.Release)
	}
	if r := appRow(t, p, "long"); r.Release != nil || r.ReleaseNote != appLedgerReadFailed {
		t.Errorf("long line: release %+v note %q, want the read error", r.Release, r.ReleaseNote)
	}
}

// The files are other accounts', so every string is stripped of control and
// invisible characters and capped, and a marker larger than its bound is read
// only to its first line.
func TestAppPowerStringsAreStrippedAndCapped(t *testing.T) {
	t.Parallel()

	reason := strings.Repeat("r", 3*appReasonMax)
	p := appFixture(t, map[string]string{
		"m": "since=" + markerSince + " by=" + strings.Repeat("b", 200) + "‮ env=prod reason=" + reason + "\x1b[31m\n" + strings.Repeat("junk\n", 5000),
	}, map[string]string{
		"m": `{"time":"2026-10-14T11:00:00Z","action":"` + strings.Repeat("a", 200) + `","reason":"x‮y\u0007z"}` + "\n",
	}).Read(appNow)

	r := appRow(t, p, "m")
	// A cut string carries a one-rune mark past its cap.
	if got := len([]rune(r.By)); got > appByMax+1 || strings.ContainsRune(r.By, '‮') {
		t.Errorf("by is %d runes or has a format character: %q", got, r.By)
	}
	if got := len([]rune(r.Reason)); got > appReasonMax+1 || !strings.HasSuffix(r.Reason, "…") {
		t.Errorf("reason is %d runes, want a cut at %d", got, appReasonMax)
	}
	if got := len([]rune(r.Release.Action)); got > appActionMax+1 {
		t.Errorf("action is %d runes, want at most %d and the mark", got, appActionMax)
	}
	if strings.ContainsAny(r.Release.Reason, "‮\u0007") || r.Release.Reason != "xyz" {
		t.Errorf("ledger reason %q still carries control or format characters", r.Release.Reason)
	}
}

// A long app name is capped like any other string, and the rows are kept to the
// cap with the rest counted.
func TestAppPowerCapsTheRowsAndNames(t *testing.T) {
	t.Parallel()

	markers := map[string]string{}
	for i := 0; i < appMaxApps+5; i++ {
		markers["app"+strings.Repeat("0", 3)+string(rune('a'+i%26))+string(rune('a'+i/26))] = "since=" + markerSince + " by=s env=all reason=x\n"
	}
	markers[strings.Repeat("a", 200)] = "since=" + markerSince + " by=s env=all reason=x\n"
	p := appFixture(t, markers, map[string]string{}).Read(appNow)

	if len(p.Apps) != appMaxApps || p.More != 6 {
		t.Errorf("%d rows and %d more, want %d and 6", len(p.Apps), p.More, appMaxApps)
	}
	for _, r := range p.Apps {
		if len([]rune(r.App)) > appNameMax+1 {
			t.Errorf("app name %q is over %d runes and the mark", r.App, appNameMax)
		}
	}
}

// The line rides the Cloud read: a reader with an apps reader carries it into
// the state, and one without leaves it absent.
func TestCloudReadFillsTheAppPowerLine(t *testing.T) {
	t.Parallel()

	dir := cloudFixture(t, cloudReportJSON(t), appNow.Format(time.RFC3339))
	apps := appFixture(t, map[string]string{"a": "since=" + markerSince + " by=s env=prod reason=x\n"}, map[string]string{})
	c := NewCloudReader(dir).WithApps(apps).Read(appNow)
	if c.Apps == nil || appRow(t, c.Apps, "a").State != AppOff {
		t.Fatalf("apps %+v, want the line read on the same poll", c.Apps)
	}
	if bare := NewCloudReader(dir).Read(appNow); bare.Apps != nil {
		t.Errorf("a reader with no apps reader filled the line: %+v", bare.Apps)
	}
}
