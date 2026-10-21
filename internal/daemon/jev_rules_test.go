package daemon

import (
	"bytes"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/jonboulle/clockwork"

	jevrules "github.com/steveyegge/gastown/internal/jev/rules"
)

// newRulesDaemon is a daemon whose rules pass is opened by open, with the patrol
// set by enabled and the price and cap in daemon.json.
func newRulesDaemon(t *testing.T, enabled bool, open func(string, jevrules.Settings, io.Writer) (*jevrules.Pass, error)) *Daemon {
	t.Helper()
	return &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&bytes.Buffer{}, "", 0),
		clock:  clockwork.NewFakeClock(),
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
