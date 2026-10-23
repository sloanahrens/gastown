package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	jevrules "github.com/steveyegge/gastown/internal/jev/rules"
)

// newRulesDaemon is a daemon whose rules pass is opened by open, with the patrol
// set by enabled and the price and cap in daemon.json.
func newRulesDaemon(t *testing.T, enabled bool, open func(string, jevrules.Settings, io.Writer) (*jevrules.Pass, error)) *Daemon {
	t.Helper()
	return &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&bytes.Buffer{}, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 17, 30, 0, 0, time.UTC)),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevRules: &JevRulesConfig{
			Enabled: enabled, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
		jevRulesOpen: open,
	}
}

// TestJevRulesOffDoesNotOpenThePass checks a town that has not opted in opens
// no pass and reads nothing.
func TestJevRulesOffDoesNotOpenThePass(t *testing.T) {
	t.Parallel()
	opened := 0
	d := newRulesDaemon(t, false, func(string, jevrules.Settings, io.Writer) (*jevrules.Pass, error) {
		opened++
		return nil, errors.New("not expected")
	})
	d.triggerJevRules()
	if opened != 0 {
		t.Errorf("pass opened %d time(s) with the patrol off, want none", opened)
	}
}

// TestJevRulesRunPassesTheConfiguredSettings checks a run opens the pass with
// the patrol's cap and price, and an open failure is logged, not raised.
func TestJevRulesRunPassesTheConfiguredSettings(t *testing.T) {
	t.Parallel()
	var got jevrules.Settings
	d := newRulesDaemon(t, true, func(_ string, s jevrules.Settings, _ io.Writer) (*jevrules.Pass, error) {
		got = s
		return nil, errors.New("no key")
	})
	d.runJevRules()
	if got.MaxUSDPerDay != 2.5 || got.USDPerBillionTokens != 3 {
		t.Errorf("settings = %+v, want the patrol's cap 2.5 and price 3", got)
	}
	if !bytes.Contains(d.logger.Writer().(*bytes.Buffer).Bytes(), []byte("jev_rules: error: no key")) {
		t.Errorf("open failure was not logged: %q", d.logger.Writer().(*bytes.Buffer).String())
	}
}

// TestJevRulesSkipsARigWithNoBeadsDirectory checks a rig that is registered
// but never cloned does not stop the pass for the rest: the missing rig is
// left out of the settings and named in the log (gt-csq9g).
func TestJevRulesSkipsARigWithNoBeadsDirectory(t *testing.T) {
	t.Parallel()
	var got jevrules.Settings
	d := newRulesDaemon(t, true, func(_ string, s jevrules.Settings, _ io.Writer) (*jevrules.Pass, error) {
		got = s
		return nil, errors.New("stop here")
	})
	town := d.config.TownRoot
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigs := `{"rigs": {"cloned": {}, "never-cloned": {}}}`
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(rigs), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(town, "cloned", ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	d.runJevRules()
	if len(got.Rigs) != 1 || got.Rigs[0] != "cloned" {
		t.Fatalf("rigs = %v, want only the rig with a beads directory", got.Rigs)
	}
	if logText := d.logger.Writer().(*bytes.Buffer).String(); !strings.Contains(logText, "skipping rig never-cloned") {
		t.Errorf("the skipped rig was not logged: %q", logText)
	}
}

// TestJevRulesReportsAStatFailureThatIsNotAMissingDirectory checks only a
// missing beads directory is reported as one: a rig whose path cannot be
// stat'ed at all is skipped with the error, not described as never cloned,
// so a broken path does not hide behind the uncloned-rig case (gt-8chz5).
func TestJevRulesReportsAStatFailureThatIsNotAMissingDirectory(t *testing.T) {
	t.Parallel()
	var got jevrules.Settings
	d := newRulesDaemon(t, true, func(_ string, s jevrules.Settings, _ io.Writer) (*jevrules.Pass, error) {
		got = s
		return nil, errors.New("stop here")
	})
	town := d.config.TownRoot
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigs := `{"rigs": {"cloned": {}, "not-a-directory": {}}}`
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(rigs), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(town, "cloned", ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A regular file where the rig's directory belongs: stat of
	// <town>/not-a-directory/.beads fails with ENOTDIR, which is not a
	// missing directory.
	if err := os.WriteFile(filepath.Join(town, "not-a-directory"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	d.runJevRules()
	if len(got.Rigs) != 1 || got.Rigs[0] != "cloned" {
		t.Fatalf("rigs = %v, want the readable rig only", got.Rigs)
	}
	logText := d.logger.Writer().(*bytes.Buffer).String()
	if !strings.Contains(logText, "cannot stat") || !strings.Contains(logText, "not-a-directory") {
		t.Errorf("the stat failure was not reported as one: %q", logText)
	}
	if strings.Contains(logText, "no beads directory at "+filepath.Join(town, "not-a-directory", ".beads")) {
		t.Errorf("a stat failure was reported as a missing beads directory: %q", logText)
	}
}

// rulesCountingAsker counts the calls a pass makes to Jev. It answers none.
type rulesCountingAsker struct{ calls int }

func (c *rulesCountingAsker) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	c.calls++
	return nil, errors.New("not expected")
}

// rulesDones lists the same submissions every time.
type rulesDones []jevrules.Done

func (f rulesDones) Done(time.Time) ([]jevrules.Done, error) { return f, nil }

// rulesPass is a pass under town over one submission at 17:20, from a seat
// whose transcript began at 17:00. Its cursor sits at 17:00, so the pass asks
// about the submission.
func rulesPass(t *testing.T, town string, asker jevrules.Asker) *jevrules.Pass {
	t.Helper()
	transcript := filepath.Join(town, "session.jsonl")
	line := `{"type":"assistant","timestamp":"2026-10-21T17:00:01Z","message":{"content":[{"type":"text","text":"ran the tests"}]}}`
	if err := os.WriteFile(transcript, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(town, "rules.cursor.json")
	if err := battery.WriteCursor(cursor, battery.Cursor{Time: time.Date(2026, 10, 21, 17, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	return &jevrules.Pass{
		Asker: asker,
		Lister: rulesDones{{
			Rig: "gastown", Bead: "gt-a", Polecat: "bullet",
			SubmittedAt: time.Date(2026, 10, 21, 17, 20, 0, 0, time.UTC),
		}},
		Locate:     func(string, string) string { return town },
		Latest:     func(string) (string, error) { return transcript, nil },
		TownRoot:   town,
		LogPath:    jevrules.LogPath(town),
		CursorPath: cursor,
		Allows:     func() (bool, error) { return true, nil },
	}
}

// TestJevRulesEStopStopsBeforeAsk checks an E-stop raised before a pass asks
// stops it before its first call to Jev, and the log names the stop.
func TestJevRulesEStopStopsBeforeAsk(t *testing.T) {
	t.Parallel()
	asker := &rulesCountingAsker{}
	d := newRulesDaemon(t, true, func(root string, _ jevrules.Settings, _ io.Writer) (*jevrules.Pass, error) {
		return rulesPass(t, root, asker), nil
	})
	if err := estop.Activate(d.config.TownRoot, "test", "operator stop"); err != nil {
		t.Fatal(err)
	}

	d.runJevRules()
	if asker.calls != 0 {
		t.Errorf("Jev was asked %d time(s) under an E-stop, want none", asker.calls)
	}
	if got := d.logger.Writer().(*bytes.Buffer).String(); !strings.Contains(got, "jev_rules: stopped: E-STOP active after asking 0 session(s)") {
		t.Errorf("E-stop was not logged: %q", got)
	}
}

// TestJevRulesSkipsWhenAnotherPassHoldsTheLock checks a run that finds another
// pass holding the run lock logs a skip and asks nothing.
func TestJevRulesSkipsWhenAnotherPassHoldsTheLock(t *testing.T) {
	t.Parallel()
	asker := &rulesCountingAsker{}
	d := newRulesDaemon(t, true, func(root string, _ jevrules.Settings, _ io.Writer) (*jevrules.Pass, error) {
		return rulesPass(t, root, asker), nil
	})
	release, err := battery.Lock(filepath.Join(d.config.TownRoot, "rules.cursor.json.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	d.runJevRules()
	if asker.calls != 0 {
		t.Errorf("Jev was asked %d time(s) while another pass held the lock, want none", asker.calls)
	}
	if got := d.logger.Writer().(*bytes.Buffer).String(); !strings.Contains(got, "jev_rules: skipped: another jev rules pass is running") {
		t.Errorf("skipped run was not logged: %q", got)
	}
}
