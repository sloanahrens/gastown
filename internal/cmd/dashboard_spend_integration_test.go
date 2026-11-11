//go:build integration

package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// The in-cluster switch reaches the spend read from the real environment, through
// resolveSpend, the production caller. The unit tier cannot set the environment
// (docs/testing.md, `no-env`), so the read is tested here. The tool is written
// without its execute bit, so a run that is attempted fails and logs the fallback;
// a run that is skipped logs nothing (gt-30pmxi).
func TestIntegrationResolveSpendReadsTheClusterSignalFromEnvironment(t *testing.T) {
	const fallback = "spend tool read failed: no answer; the balance ledger answers"
	tests := []struct {
		name       string
		supervisor string
		wantTool   bool
	}{
		{"the flag the compose file exports reads the ledger alone", config.SupervisorContainer, false},
		{"an unset flag keeps the tool first and falls back to the ledger", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(config.ContainerModeEnv, tt.supervisor)
			writeSpendLedger(t, home)
			writeSpendTool(t, home)

			var logged []string
			whyLog := newDashWhyLog(func(format string, args ...any) {
				logged = append(logged, fmt.Sprintf(format, args...))
			})
			spend, reason := resolveSpend("", whyLog)
			if spend == nil || reason != "" {
				t.Fatalf("resolveSpend: spend nil %v, reason %q; want a source and no reason", spend == nil, reason)
			}
			out := spend()
			var report struct {
				Samples int `json:"samples"`
			}
			if err := json.Unmarshal(out, &report); err != nil || report.Samples != 2 {
				t.Fatalf("served %s (err %v), want the ledger report of 2 rows", out, err)
			}
			var toolLines []string
			for _, line := range logged {
				if strings.HasPrefix(line, "spend tool") {
					toolLines = append(toolLines, line)
				}
			}
			if tt.wantTool {
				if len(toolLines) != 1 || toolLines[0] != fallback {
					t.Errorf("why-log tool lines = %q, want exactly %q", toolLines, fallback)
				}
				return
			}
			if len(toolLines) != 0 {
				t.Errorf("why-log tool lines = %q, want none: the tool must not be run in the cluster", toolLines)
			}
		})
	}
}
