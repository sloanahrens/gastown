package config

import (
	"os"
	"path/filepath"
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

// A negative split_added_lines is refused at decode, naming the key: a line
// count below zero is a typo, and read as written it would stop every seat that
// reached the mark.
func TestRunawayBeadSplitAddedLinesRefusesNegative(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"-1", "-300"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			_, err := loadOverrunGuardBlock(t, `{"type":"daemon-patrol-config","version":1,"patrols":{`+
				`"overrun_guard":{"enabled":true,"split_added_lines":`+value+`}}}`)
			if err == nil {
				t.Fatalf("LoadDaemonPatrolConfig accepted split_added_lines %s", value)
			}
			if !strings.Contains(err.Error(), "daemon.patrols.overrun_guard.split_added_lines") {
				t.Errorf("error = %v, want it to name daemon.patrols.overrun_guard.split_added_lines", err)
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

// The block decodes its own JSON, so it repeats the kernel's unknown-key
// refusal: a key that is not split_added_lines is a typo, never an ignored
// setting.
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

// The block's other keys still load through its custom decoder, so adding
// split_added_lines takes nothing away from them.
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
