package daemon

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	jevattention "github.com/steveyegge/gastown/internal/jev/attention"
	jevconflict "github.com/steveyegge/gastown/internal/jev/conflict"
	jevdispatch "github.com/steveyegge/gastown/internal/jev/dispatch"
	"github.com/steveyegge/gastown/internal/jev/landing"
	"github.com/steveyegge/gastown/internal/jev/logtriage"
	jevnotes "github.com/steveyegge/gastown/internal/jev/notes"
	"github.com/steveyegge/gastown/internal/jev/rework"
	jevrules "github.com/steveyegge/gastown/internal/jev/rules"
	jevthrash "github.com/steveyegge/gastown/internal/jev/thrash"
)

// A patrol whose last-run save fails must still not be due on the next
// heartbeat: with no record on disk every check read it as due and opened
// another pass (gt-4nqej, gt-th2am, gt-cekkh). Each test below skips the pass
// with an opener that fails, so the run records its time without reaching Jev,
// and counts the opens across two triggers inside the interval.

// failedSaveDaemon returns a daemon over a town whose last-run record cannot be
// written (its directory is a regular file), a fake clock, and its log.
func failedSaveDaemon(t *testing.T, cfg *PatrolsConfig) (*Daemon, *clockwork.FakeClock, *bytes.Buffer) {
	t.Helper()
	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, "daemon"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	d := &Daemon{
		config:       &Config{TownRoot: town},
		logger:       log.New(logs, "", 0),
		clock:        clk,
		patrolConfig: &DaemonPatrolConfig{Patrols: cfg},
	}
	return d, clk, logs
}

// assertFailedSaveSuppressesTheRerun triggers the patrol twice inside its
// interval and once after it, and checks the pass opened once, then twice.
func assertFailedSaveSuppressesTheRerun(t *testing.T, trigger func(), running *atomic.Bool, opens *int, clk *clockwork.FakeClock, logs *bytes.Buffer, interval time.Duration) {
	t.Helper()

	trigger()
	waitFlagClear(t, running)
	trigger()
	waitFlagClear(t, running)

	if *opens != 1 {
		t.Errorf("pass opened %d times inside one interval after a failed save, want 1", *opens)
	}
	if !strings.Contains(logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", logs)
	}

	clk.Advance(interval)
	trigger()
	waitFlagClear(t, running)
	if *opens != 2 {
		t.Errorf("pass opened %d times after the interval, want 2", *opens)
	}
}

var errSkipPass = errors.New("pass skipped by the test")

func TestJevAttentionFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	d, clk, logs := failedSaveDaemon(t, &PatrolsConfig{JevAttention: &JevAttentionConfig{Enabled: true}})
	opens := 0
	d.jevAttentionOpen = func(string, jevattention.Settings, io.Writer) (*jevattention.Pass, error) {
		opens++
		return nil, errSkipPass
	}
	assertFailedSaveSuppressesTheRerun(t, d.triggerJevAttention, &d.jevAttentionRunning, &opens, clk, logs, jevAttentionInterval)
}

func TestJevLandingFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	d, clk, logs := failedSaveDaemon(t, &PatrolsConfig{JevLanding: &JevLandingConfig{Enabled: true}})
	opens := 0
	d.jevLandingOpen = func(string, landing.Settings, io.Writer) (*landing.Pass, error) {
		opens++
		return nil, errSkipPass
	}
	assertFailedSaveSuppressesTheRerun(t, d.triggerJevLanding, &d.jevLandingRunning, &opens, clk, logs, jevLandingInterval)
}

func TestJevLogtriageFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	d, clk, logs := failedSaveDaemon(t, &PatrolsConfig{JevLogtriage: &JevLogtriageConfig{Enabled: true}})
	opens := 0
	d.jevLogtriageOpen = func(string, logtriage.Settings, io.Writer) (*logtriage.Pass, error) {
		opens++
		return nil, errSkipPass
	}
	assertFailedSaveSuppressesTheRerun(t, d.triggerJevLogtriage, &d.jevLogtriageRunning, &opens, clk, logs, jevLogtriageInterval)
}

func TestJevReworkFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	d, clk, logs := failedSaveDaemon(t, &PatrolsConfig{JevRework: &JevReworkConfig{Enabled: true}})
	opens := 0
	d.jevReworkOpen = func(string, rework.Settings, io.Writer) (*rework.Pass, error) {
		opens++
		return nil, errSkipPass
	}
	assertFailedSaveSuppressesTheRerun(t, d.triggerJevRework, &d.jevReworkRunning, &opens, clk, logs, jevReworkInterval)
}

func TestJevConflictFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	d, clk, logs := failedSaveDaemon(t, &PatrolsConfig{JevConflict: &JevConflictConfig{Enabled: true}})
	opens := 0
	d.jevConflictOpen = func(string, jevconflict.Settings, io.Writer) (*jevconflict.Pass, error) {
		opens++
		return nil, errSkipPass
	}
	assertFailedSaveSuppressesTheRerun(t, d.triggerJevConflict, &d.jevConflictRunning, &opens, clk, logs, jevConflictInterval)
}

func TestJevNotesFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	d, clk, logs := failedSaveDaemon(t, &PatrolsConfig{JevNotes: &JevNotesConfig{Enabled: true}})
	opens := 0
	d.jevNotesOpen = func(string, jevnotes.Settings, io.Writer) (*jevnotes.Pass, error) {
		opens++
		return nil, errSkipPass
	}
	assertFailedSaveSuppressesTheRerun(t, d.triggerJevNotes, &d.jevNotesRunning, &opens, clk, logs, jevNotesInterval)
}

func TestJevRulesFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	d, clk, logs := failedSaveDaemon(t, &PatrolsConfig{JevRules: &JevRulesConfig{Enabled: true}})
	opens := 0
	d.jevRulesOpen = func(string, jevrules.Settings, io.Writer) (*jevrules.Pass, error) {
		opens++
		return nil, errSkipPass
	}
	assertFailedSaveSuppressesTheRerun(t, d.triggerJevRules, &d.jevRulesRunning, &opens, clk, logs, jevRulesInterval)
}

func TestJevDispatchFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	d, clk, logs := failedSaveDaemon(t, &PatrolsConfig{JevDispatch: &JevDispatchConfig{Enabled: true}})
	opens := 0
	d.jevDispatchOpen = func(string, jevdispatch.Settings, io.Writer) (*jevdispatch.Pass, error) {
		opens++
		return nil, errSkipPass
	}
	assertFailedSaveSuppressesTheRerun(t, d.triggerJevDispatch, &d.jevDispatchRunning, &opens, clk, logs, jevDispatchInterval)
}

func TestJevThrashFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	d, clk, logs := failedSaveDaemon(t, &PatrolsConfig{JevThrash: &JevThrashConfig{Enabled: true}})
	opens := 0
	d.jevThrashOpen = func(string, jevthrash.Settings, io.Writer) (*jevthrash.Pass, error) {
		opens++
		return nil, errSkipPass
	}
	assertFailedSaveSuppressesTheRerun(t, d.triggerJevThrash, &d.jevThrashRunning, &opens, clk, logs, jevThrashInterval)
}
