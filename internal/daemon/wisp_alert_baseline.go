package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/reaper"
)

// Bookkeeping for the reaper's open-wisp alert (gt-11kyy): what the last cycle
// read, so a cycle can be judged against something. No single count can say
// whether the wisp lifecycle is healthy — only a change can — so the alert
// compares cycles, and this is where the one it compares against lives.
//
// The daemon patrol and a hand-run `gt reaper run` keep separate files: they
// sample on schedules that have nothing to do with each other, so one series
// would read the other producer's idle gap as growth. A town that only ever
// reaps by hand still builds the series the alert needs, in the CLI's file.

const (
	wispAlertDaemonBaselineFile = "wisp_open_baseline_daemon.json"
	wispAlertCLIBaselineFile    = "wisp_open_baseline_cli.json"
)

// WispAlertBaselinePath returns the file holding the daemon patrol's open-wisp
// series, one reading per patrol cycle.
func WispAlertBaselinePath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", wispAlertDaemonBaselineFile)
}

// WispAlertCLIBaselinePath returns the file holding the open-wisp series of
// hand-run `gt reaper run` invocations, one reading per invocation.
func WispAlertCLIBaselinePath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", wispAlertCLIBaselineFile)
}

// LoadWispAlertState returns the state the last cycle of this series recorded,
// and nil when none has been recorded. An unreadable or corrupt file is an
// error the caller acts on and then treats as no state: a reading that cannot
// be trusted is one to replace, not one to compare against (gt-11kyy).
func LoadWispAlertState(path string) (*reaper.OpenWispAlertState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var state reaper.OpenWispAlertState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return &state, nil
}

// SaveWispAlertState records this cycle's state as the one the next cycle of
// the same series is judged against.
func SaveWispAlertState(path string, state reaper.OpenWispAlertState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return atomicfile.WriteJSON(path, state)
}
