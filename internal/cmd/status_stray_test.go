package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/townstatus"
)

// TestOutputStatusTextListsStrayDolt pins the text half of gt-gyw5w: a stray
// Dolt server is printed with its pid, its port, its data-dir and the command
// that clears it.
func TestOutputStatusTextListsStrayDolt(t *testing.T) {
	status := townstatus.TownStatus{
		Name:     "gt",
		Location: t.TempDir(),
		Dolt: &townstatus.DoltInfo{
			Running: true,
			Port:    3307,
			Stray: []townstatus.StrayDoltInfo{
				{PID: 29490, Port: 3399, DataDir: "/tmp/doltprobe/data", Remedy: "kill 29490"},
				{PID: 500, Port: 3307, DataDir: "/tmp/other/data", Remedy: "gt dolt kill-imposters"},
			},
		},
	}

	var buf bytes.Buffer
	if err := outputStatusText(&buf, status); err != nil {
		t.Fatalf("outputStatusText() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"2 stray Dolt server(s)",
		"PID 29490",
		":3399",
		"/tmp/doltprobe/data",
		"kill 29490",
		"gt dolt kill-imposters",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status text missing %q:\n%s", want, out)
		}
	}
}
