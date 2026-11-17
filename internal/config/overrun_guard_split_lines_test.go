package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// loadOverrunGuardBlock loads raw as a daemon patrol config from t.TempDir() and
// returns the guard block the loader produced, so every row below drives the
// production load path rather than decoding the block on its own.
func loadOverrunGuardBlock(t *testing.T, raw string) (*OverrunGuardConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadDaemonPatrolConfig(path)
	if err != nil {
		return nil, err
	}
	if cfg.Patrols == nil {
		t.Fatal("patrols = nil, want the overrun_guard block")
	}
	return cfg.Patrols.OverrunGuard, nil
}

// An absent split_added_lines and an explicit 0 both leave the tripwire off, so
// a town that sets nothing pays no line-counting stop.
func TestRunawayBeadSplitAddedLinesOff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		block string
	}{
		{name: "absent", block: `"overrun_guard":{"enabled":true}`},
		{name: "zero", block: `"overrun_guard":{"enabled":true,"split_added_lines":0}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			og, err := loadOverrunGuardBlock(t, `{"type":"daemon-patrol-config","version":1,"patrols":{`+tc.block+`}}`)
			if err != nil {
				t.Fatalf("LoadDaemonPatrolConfig: %v", err)
			}
			if og == nil {
				t.Fatal("overrun_guard = nil, want the block")
			}
			if got := og.SplitAddedLines; got != 0 {
				t.Errorf("SplitAddedLines = %d, want 0", got)
			}
			if got := og.SplitAddedLinesOrOff(); got != 0 {
				t.Errorf("SplitAddedLinesOrOff() = %d, want 0", got)
			}
		})
	}
}

// A positive split_added_lines loads as written, so the guard can compare a
// seat's added lines against it.
func TestRunawayBeadSplitAddedLinesReadsValue(t *testing.T) {
	t.Parallel()
	og, err := loadOverrunGuardBlock(t, `{"type":"daemon-patrol-config","version":1,"patrols":{`+
		`"overrun_guard":{"enabled":true,"split_added_lines":300}}}`)
	if err != nil {
		t.Fatalf("LoadDaemonPatrolConfig: %v", err)
	}
	if og == nil {
		t.Fatal("overrun_guard = nil, want the block")
	}
	if got := og.SplitAddedLines; got != 300 {
		t.Errorf("SplitAddedLines = %d, want 300", got)
	}
	if got := og.SplitAddedLinesOrOff(); got != 300 {
		t.Errorf("SplitAddedLinesOrOff() = %d, want 300", got)
	}
}

// A negative split_added_lines loads the way its sibling durations do: the
// load warns naming the key and keeps the operator's value as written, and the
// tripwire reads it as off, so a typo cannot fail the whole load or stop every
// seat that reached the mark.
func TestRunawayBeadSplitAddedLinesWarnsNegative(t *testing.T) {
	t.Parallel()
	for _, value := range []int{-1, -300} {
		t.Run(strconv.Itoa(value), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "daemon.json")
			raw := `{"type":"daemon-patrol-config","version":1,"patrols":{` +
				`"overrun_guard":{"enabled":true,"split_added_lines":` + strconv.Itoa(value) + `}}}`
			if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}

			var warn strings.Builder
			cfg, err := loadDaemonPatrolConfig(path, &warn)
			if err != nil {
				t.Fatalf("loadDaemonPatrolConfig refused split_added_lines %d: %v", value, err)
			}
			og := cfg.Patrols.OverrunGuard
			if og == nil {
				t.Fatal("overrun_guard = nil, want the block")
			}
			if !og.Enabled {
				t.Error("Enabled = false, want the block's other settings kept")
			}
			if og.SplitAddedLines != value {
				t.Errorf("SplitAddedLines = %d, want the operator's %d kept", og.SplitAddedLines, value)
			}
			if got := og.SplitAddedLinesOrOff(); got != 0 {
				t.Errorf("SplitAddedLinesOrOff() = %d, want 0 (off)", got)
			}
			got := warn.String()
			if !strings.Contains(got, "daemon.patrols.overrun_guard.split_added_lines") {
				t.Errorf("warning = %q, want it to name daemon.patrols.overrun_guard.split_added_lines", got)
			}
			if !strings.Contains(got, strconv.Itoa(value)) {
				t.Errorf("warning = %q, want it to name the value %d", got, value)
			}
		})
	}
}

// A negative split_added_lines is refused on save, naming the key, so a writer
// cannot persist a line count the load only warns about.
func TestSaveDaemonPatrolConfigRefusesNegativeSplitAddedLines(t *testing.T) {
	t.Parallel()
	for _, value := range []int{-1, -300} {
		t.Run(strconv.Itoa(value), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "daemon.json")
			cfg := &DaemonPatrolConfig{
				Type:    "daemon-patrol-config",
				Version: 1,
				Patrols: &PatrolsConfig{OverrunGuard: &OverrunGuardConfig{Enabled: true, SplitAddedLines: value}},
			}
			err := SaveDaemonPatrolConfig(path, cfg)
			if err == nil {
				t.Fatalf("SaveDaemonPatrolConfig accepted split_added_lines %d", value)
			}
			if !strings.Contains(err.Error(), "daemon.patrols.overrun_guard.split_added_lines") {
				t.Errorf("error = %v, want it to name daemon.patrols.overrun_guard.split_added_lines", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("a refused save left a file behind: %v", err)
			}
		})
	}
}

// A usable split_added_lines loads without a word: only a value below zero is
// a typo the load reports.
func TestRunawayBeadSplitAddedLinesGoodValueWarnsNothing(t *testing.T) {
	t.Parallel()
	for _, value := range []int{0, 300} {
		t.Run(strconv.Itoa(value), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "daemon.json")
			raw := `{"type":"daemon-patrol-config","version":1,"patrols":{` +
				`"overrun_guard":{"enabled":true,"split_added_lines":` + strconv.Itoa(value) + `}}}`
			if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}

			var warn strings.Builder
			if _, err := loadDaemonPatrolConfig(path, &warn); err != nil {
				t.Fatalf("loadDaemonPatrolConfig: %v", err)
			}
			if warn.Len() != 0 {
				t.Errorf("a usable split_added_lines %d warned: %q", value, warn.String())
			}
		})
	}
}

// The accessor answers off on a nil block, which is what an absent
// overrun_guard leaves its callers holding.
func TestRunawayBeadSplitAddedLinesNilIsOff(t *testing.T) {
	t.Parallel()
	var og *OverrunGuardConfig
	if got := og.SplitAddedLinesOrOff(); got != 0 {
		t.Errorf("SplitAddedLinesOrOff() on a nil block = %d, want 0", got)
	}
}

// The kernel's strict decode walks the block, so a key that is not one of its
// own is a typo, never an ignored setting.
func TestRunawayBeadSplitAddedLinesRefusesUnknownKey(t *testing.T) {
	t.Parallel()
	_, err := loadOverrunGuardBlock(t, `{"type":"daemon-patrol-config","version":1,"patrols":{`+
		`"overrun_guard":{"enabled":true,"split_added_line":300}}}`)
	if err == nil {
		t.Fatal("LoadDaemonPatrolConfig accepted split_added_line")
	}
	if !strings.Contains(err.Error(), "split_added_line") {
		t.Errorf("error = %v, want it to name split_added_line", err)
	}
}

// The block's other keys still load, so adding split_added_lines takes nothing
// away from them.
func TestRunawayBeadOverrunGuardBlockStillDecodes(t *testing.T) {
	t.Parallel()
	og, err := loadOverrunGuardBlock(t, `{"type":"daemon-patrol-config","version":1,"patrols":{`+
		`"overrun_guard":{"enabled":true,"interval":"2m","nudge_after":"10m",`+
		`"stop_after":"25m","scheduled_stop_after":"45m","gate_allowance":"5m",`+
		`"split_added_lines":300,"dry_run":true}}}`)
	if err != nil {
		t.Fatalf("LoadDaemonPatrolConfig: %v", err)
	}
	if og == nil {
		t.Fatal("overrun_guard = nil, want the block")
	}
	want := OverrunGuardConfig{
		Enabled:            true,
		IntervalStr:        "2m",
		NudgeAfter:         "10m",
		StopAfter:          "25m",
		ScheduledStopAfter: "45m",
		GateAllowance:      "5m",
		SplitAddedLines:    300,
		DryRun:             true,
	}
	if *og != want {
		t.Errorf("overrun_guard = %+v, want %+v", *og, want)
	}
}
