package cmd

import (
	"bytes"
	"testing"
)

// TestStepWispRetireDryRunBondLineNamesRootOnly: the bond line a dry run
// prints is the command the real bond runs. It names --root-only exactly when
// the town's daemon settings ask for a root-only bond, after the --var
// arguments where Beads.Bond appends it. A settings file that does not load
// omits the flag and warns, as the real bond does.
func TestStepWispRetireDryRunBondLineNamesRootOnly(t *testing.T) {
	t.Parallel()
	const (
		on      = `{"type":"daemon-patrol-config","version":1,"bond_root_only":true}`
		off     = `{"type":"daemon-patrol-config","version":1,"bond_root_only":false}`
		invalid = `{"type":"daemon-patrol-config","bond_root_only":`
		base    = `bd mol bond mol-polecat-work gt-abc --json --ephemeral --var feature="build it" --var issue="gt-abc"`
	)
	tests := []struct {
		name     string
		settings string // "" writes no settings file at all
		want     string
		wantWarn bool
	}{
		{"key true", on, base + " --root-only", false},
		{"key false", off, base, false},
		{"settings file absent", "", base, false},
		{"settings file invalid", invalid, base, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if tc.settings != "" {
				writeDaemonSettings(t, townRoot, tc.settings)
			}
			var warned bytes.Buffer
			got := dryRunBondLine(townRoot, "mol-polecat-work", "gt-abc", "build it", &warned)
			if got != tc.want {
				t.Errorf("dry-run bond line = %q, want %q", got, tc.want)
			}
			if gotWarn := warned.Len() > 0; gotWarn != tc.wantWarn {
				t.Errorf("warned %q, want a warning = %v", warned.String(), tc.wantWarn)
			}
		})
	}
}
