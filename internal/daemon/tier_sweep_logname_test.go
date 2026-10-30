package daemon

import (
	"path/filepath"
	"testing"
)

// TestTierSweepUnitLogsNameTheGoCommandsFiles pins the unit-log names the
// daemon reads to the names the Go command writes (gt-j6vlz, gt-tpg0p.104).
// The Go command names a Go tier's package log with a hash of the package
// (tiersweep.LogName, pinned by TestTierLogsAndFailureExcerpt), so a daemon
// that rebuilt the bare name would read no file and lose the slowcheck
// findings. A shell script's log keeps the "shell-" form the shell runner
// writes. A unit that names no package or script reads no log.
func TestTierSweepUnitLogsNameTheGoCommandsFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tests := []struct {
		name, tier, unit, want string
	}{
		{name: "an integration package", tier: "integration", unit: "./internal/cmd", want: "integration-internal_cmd-9addf82b.log"},
		{name: "a nested module package", tier: "integration", unit: "./tracker/store", want: "integration-tracker_store-182ef2eb.log"},
		{name: "a shell script", tier: "shell", unit: "scripts/test-makefile.sh", want: "shell-scripts_test-makefile.sh.log"},
		{name: "a stand-in unit reads no log", tier: "integration", unit: "(no failing unit named)", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tierSweepUnitLogPath(dir, tc.tier, tc.unit)
			want := ""
			if tc.want != "" {
				want = filepath.Join(dir, tc.want)
			}
			if got != want {
				t.Fatalf("tierSweepUnitLogPath(%q, %q, %q) = %q, want %q", dir, tc.tier, tc.unit, got, want)
			}
		})
	}
}
