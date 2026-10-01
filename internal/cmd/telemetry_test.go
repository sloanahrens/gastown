package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestUsageActor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		env         map[string]string
		interactive bool
		want        string
	}{
		{"qualified role wins over BD_ACTOR", map[string]string{"GT_ROLE": "gastown/witness", "BD_ACTOR": "daemon"}, false, "gastown/witness"},
		{"daemon spawn", map[string]string{"BD_ACTOR": "daemon"}, false, "daemon"},
		{"BD_ACTOR qualifies bare role", map[string]string{"GT_ROLE": "polecat", "BD_ACTOR": "gastown/polecats/opal"}, false, "gastown/polecats/opal"},
		{"bare polecat role from rig and name", map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "opal"}, false, "gastown/polecats/opal"},
		{"bare crew role from rig and name", map[string]string{"GT_ROLE": "crew", "GT_RIG": "gastown", "GT_CREW": "sloan"}, false, "gastown/crew/sloan"},
		{"bare role without rig", map[string]string{"GT_ROLE": "mayor"}, true, "mayor"},
		{"operator at a terminal", nil, true, "operator"},
		{"no identity off-terminal", nil, false, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := usageActor(envOf(tt.env), tt.interactive); got != tt.want {
				t.Errorf("usageActor = %q, want %q", got, tt.want)
			}
		})
	}
}

// The usage log reads identity only from the D5 allowlist.
func TestUsageActorReadsOnlyIdentityAllowlist(t *testing.T) {
	t.Parallel()
	var read []string
	getenv := func(k string) string {
		read = append(read, k)
		if k == "GT_ROLE" {
			return "polecat"
		}
		return ""
	}
	usageActor(getenv, false)
	for _, k := range read {
		if !slices.Contains(config.IdentityEnvVars, k) {
			t.Errorf("usageActor read %s, which is not in config.IdentityEnvVars", k)
		}
	}
}

func TestUsageLogSuppressed(t *testing.T) {
	t.Parallel()
	if !usageLogSuppressed(true, envOf(nil)) {
		t.Error("a test binary must not write the usage log")
	}
	if !usageLogSuppressed(false, envOf(map[string]string{"GT_TEST_HERMETIC": "1"})) {
		t.Error("a hermetic-harness subprocess must not write the usage log")
	}
	if usageLogSuppressed(false, envOf(nil)) {
		t.Error("a production gt must write the usage log")
	}
}

func TestAppendUsageRotatesDailyAndPrunes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	day1 := time.Date(2026, 9, 1, 23, 59, 0, 0, time.Local)
	day2 := day1.Add(2 * time.Minute)
	stale := filepath.Join(dir, "cmd-usage-2026-07-01.jsonl")
	kept := filepath.Join(dir, "cmd-usage-2026-08-03.jsonl")
	legacy := filepath.Join(dir, "cmd-usage.jsonl")
	for _, p := range []string{stale, kept, legacy} {
		if err := os.WriteFile(p, []byte(`{"cmd":"gt old"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, e := range []struct {
		now time.Time
		cmd string
	}{{day1, "gt a"}, {day1, "gt b"}, {day2, "gt c"}} {
		if err := appendUsage(dir, e.now, e.cmd, "operator", 0); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale log not pruned: %v", err)
	}
	for _, p := range []string{kept, legacy} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", filepath.Base(p), err)
		}
	}
	d1, _ := os.ReadFile(usageLogPath(dir, day1))
	d2, _ := os.ReadFile(usageLogPath(dir, day2))
	if strings.Count(string(d1), "\n") != 2 || strings.Count(string(d2), "\n") != 1 {
		t.Errorf("day files = %q / %q, want 2 and 1 entries", d1, d2)
	}

	entries, err := readUsageLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var cmds []string
	for _, e := range entries {
		cmds = append(cmds, e.Cmd)
	}
	if want := []string{"gt old", "gt old", "gt a", "gt b", "gt c"}; !slices.Equal(cmds, want) {
		t.Errorf("readUsageLog cmds = %v, want %v (legacy first, then days in order)", cmds, want)
	}
	if entries[2].Actor != "operator" || entries[2].Ts != day1.Format(time.RFC3339) {
		t.Errorf("entry = %+v", entries[2])
	}
}

func TestReadUsageLogEmpty(t *testing.T) {
	t.Parallel()
	if _, err := readUsageLog(t.TempDir()); err == nil {
		t.Error("want an error when there is no usage data")
	}
}
