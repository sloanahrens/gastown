package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	jevnotes "github.com/steveyegge/gastown/internal/jev/notes"
)

func TestJevNotesPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_notes") {
		t.Error("jev_notes must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_notes") {
		t.Error("jev_notes must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevNotes: &JevNotesConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_notes") {
		t.Error("an explicit enabled=false must keep jev_notes off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevNotes: &JevNotesConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_notes") {
		t.Error("an explicit enabled=true must turn jev_notes on")
	}
}

func TestJevNotesConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_notes":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevNotes
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_notes = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}

// The heartbeat runs the step, and it is lifecycle: an E-stopped town spends
// nothing on it.
func TestJevNotesIsALifecycleHeartbeatStep(t *testing.T) {
	t.Parallel()
	for _, s := range heartbeatSteps {
		if s.name == "jev-notes" {
			if !s.lifecycle {
				t.Error("jev-notes must be a lifecycle step")
			}
			return
		}
	}
	t.Error("no jev-notes heartbeat step")
}

// With the patrol off, the trigger runs no pass: it makes no last-run record,
// so a later enable is due at once, and it logs nothing.
func TestJevNotesTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)),
	}

	d.triggerJevNotes()

	if d.jevNotesRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_notes"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled read with no price refuses to run and says why, without reaching
// Jev.
func TestJevNotesRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevNotes: &JevNotesConfig{Enabled: true}},
		},
	}

	d.runJevNotes()

	if !strings.Contains(logs.String(), "jev_notes: error:") ||
		!strings.Contains(logs.String(), "jev_notes") {
		t.Errorf("log = %q, want the missing price reported", logs)
	}
}

// notesAsker answers every file the same way and counts the calls it was made.
type notesAsker struct {
	calls int
	// onAsk, when set, runs before each answer.
	onAsk func()
}

func (a *notesAsker) Ask(_ context.Context, _ any, _ map[string]jev.Question) (*jev.Answers, error) {
	a.calls++
	if a.onAsk != nil {
		a.onAsk()
	}
	return &jev.Answers{
		Model: "jev-test",
		Noul:  map[string]float64{jevnotes.QStillDescribes: 0.8, jevnotes.QNamesRemoved: 0.2},
		Choice: map[string]jev.ChoiceAnswer{jevnotes.QKind: {
			Choice: jevnotes.KindNone, Probabilities: map[string]float64{jevnotes.KindNone: 0.9}, Confidence: 0.9,
		}},
	}, nil
}

// notesRepo is a clone with one file in its tree.
type notesRepo struct{}

func (notesRepo) Sync(context.Context) error                   { return nil }
func (notesRepo) Tree(context.Context) ([]string, error)       { return []string{"docs/reference.md"}, nil }
func (notesRepo) HasKey(context.Context, string) (bool, error) { return true, nil }

type notesHelp struct{}

func (notesHelp) Help(context.Context, string, ...string) (string, error) {
	return "", jevnotes.ErrNoBinary
}

type jevNotesRig struct {
	d      *Daemon
	town   string
	logs   *bytes.Buffer
	clock  *clockwork.FakeClock
	opened int
	got    jevnotes.Settings
	pass   *jevnotes.Pass
	asker  *notesAsker
}

func newJevNotesRig(t *testing.T, files ...string) *jevNotesRig {
	t.Helper()
	town := t.TempDir()
	r := &jevNotesRig{
		town:  town,
		logs:  &bytes.Buffer{},
		clock: clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)),
		asker: &notesAsker{},
	}
	r.d = &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(r.logs, "", 0),
		clock:  r.clock,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevNotes: &JevNotesConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
	}
	dir := t.TempDir()
	var list []jevnotes.File
	for i, name := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("Note "+string(rune('a'+i))+" names `docs/reference.md`.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		list = append(list, jevnotes.File{Source: jevnotes.SourceMemory, Name: name, Path: path})
	}
	r.pass = &jevnotes.Pass{
		Asker:   r.asker,
		Repo:    notesRepo{},
		Help:    notesHelp{},
		Files:   func() ([]jevnotes.File, error) { return list, nil },
		LogPath: jevnotes.LogPath(town),
		Allows:  func() (bool, error) { return true, nil },
		Now:     r.clock.Now,
	}
	r.d.jevNotesOpen = func(_ string, s jevnotes.Settings, _ io.Writer) (*jevnotes.Pass, error) {
		r.opened++
		r.got = s
		// A run wraps the pass's Asker in the E-stop gate, so each open hands
		// back a copy with the plain Asker, as the production Open does.
		p := *r.pass
		p.Asker = r.asker
		return &p, nil
	}
	return r
}

func waitJevNotesIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for d.jevNotesRunning.Load() {
		if ctx.Err() != nil {
			t.Fatal("jev_notes pass did not finish")
		}
		runtime.Gosched()
	}
}

// With the patrol enabled and due, the trigger runs one pass on its own
// goroutine with the configured settings and records the run. It is not due
// again until the interval has passed, and a file asked in the window is not
// asked again.
func TestJevNotesTriggerRunsOnePassWhenEnabledAndDue(t *testing.T) {
	t.Parallel()
	r := newJevNotesRig(t, "a.md")

	r.d.triggerJevNotes()
	waitJevNotesIdle(t, r.d)

	if r.opened != 1 {
		t.Fatalf("pass opened %d times, want 1", r.opened)
	}
	if want := (jevnotes.Settings{MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}); r.got != want {
		t.Errorf("settings = %+v, want %+v from daemon.json", r.got, want)
	}
	if r.asker.calls != 1 {
		t.Errorf("first pass asked %d files, want 1", r.asker.calls)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_notes"); !found {
		t.Error("no last-run record after the pass")
	}

	r.d.triggerJevNotes()
	waitJevNotesIdle(t, r.d)
	if r.opened != 1 {
		t.Errorf("a second trigger inside the week opened a pass (%d opens)", r.opened)
	}

	r.clock.Advance(jevNotesInterval)
	r.d.triggerJevNotes()
	waitJevNotesIdle(t, r.d)
	if r.opened != 2 {
		t.Errorf("pass opened %d times after the interval, want 2", r.opened)
	}
	if r.asker.calls != 1 {
		t.Errorf("a pass inside thirty days asked the same unchanged file again (%d calls)", r.asker.calls)
	}
}

// A trigger that arrives while a pass is in flight is turned away.
func TestJevNotesTriggerSkipsWhileAPassIsRunning(t *testing.T) {
	t.Parallel()
	r := newJevNotesRig(t, "a.md")
	r.d.jevNotesRunning.Store(true)

	r.d.triggerJevNotes()

	if r.opened != 0 {
		t.Errorf("a trigger during a pass opened another (%d opens)", r.opened)
	}
}

// An E-stop raised while a pass runs ends it after the call in flight, and does
// not spend the week: no last-run is recorded, so the next trigger after the
// E-stop clears asks the file the stop cut short.
func TestJevNotesEstopStopsThePassAndLeavesTheWeekUnspent(t *testing.T) {
	t.Parallel()
	r := newJevNotesRig(t, "a.md", "b.md", "c.md")
	r.asker.onAsk = func() {
		if r.asker.calls == 1 {
			if err := estop.Activate(r.town, "test", "stop the notes read"); err != nil {
				t.Fatal(err)
			}
		}
	}

	r.d.triggerJevNotes()
	waitJevNotesIdle(t, r.d)

	if r.asker.calls != 1 {
		t.Errorf("asked %d files after the E-stop, want 1", r.asker.calls)
	}
	if !strings.Contains(r.logs.String(), "jev_notes: stopped: E-STOP active") {
		t.Errorf("log = %q, want the stop named", r.logs)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_notes"); found {
		t.Fatal("an E-stopped pass recorded its last run")
	}

	if err := estop.Deactivate(r.town, false); err != nil {
		t.Fatal(err)
	}
	r.asker.onAsk = nil
	r.d.triggerJevNotes()
	waitJevNotesIdle(t, r.d)

	if r.opened != 2 || r.asker.calls != 3 {
		t.Errorf("opened %d passes with %d calls, want the second pass to ask the two files left", r.opened, r.asker.calls)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_notes"); !found {
		t.Error("no last-run record after the pass that finished")
	}
}

// A run that finds the lock held logs it as skipped, not as a failure.
func TestJevNotesRunLogsABusyLockAsSkipped(t *testing.T) {
	t.Parallel()
	r := newJevNotesRig(t, "a.md")
	release, err := battery.Lock(jevnotes.LogPath(r.town) + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	r.d.runJevNotes()

	if r.asker.calls != 0 || !strings.Contains(r.logs.String(), "jev_notes: skipped:") {
		t.Errorf("calls = %d, log = %q; want no call and a skipped line", r.asker.calls, r.logs)
	}
}
