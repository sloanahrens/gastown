package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/slot"
)

// slotTestCmd returns a command whose output the test can read back.
func slotTestCmd(out *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd
}

func TestPrintSlotReapReport(t *testing.T) {
	orphan := slot.ContainerVerdict{
		Container: slot.GateContainer{ID: "orphan-id", Image: "dolthub/dolt-sql-server:2.2.0", Name: "wizardly_goldberg"},
		Verdict:   slot.VerdictDebris,
		Reason:    "age 5h0m0s exceeds the 30m0s staleness window with no reaper running for its session",
	}

	t.Run("dry run", func(t *testing.T) {
		out := &bytes.Buffer{}
		printSlotReapReport(slotTestCmd(out), slot.ReapReport{
			OlderThan: 30 * time.Minute,
			DryRun:    true,
			Debris:    []slot.ContainerVerdict{orphan},
			Kept:      []slot.ContainerVerdict{{Container: slot.GateContainer{Image: "dolt/dolt-sql-server:2.2.0", Name: "live"}, Verdict: slot.VerdictLive}},
		})
		got := out.String()
		if !strings.Contains(got, "Would remove") {
			t.Errorf("output = %q, want the dry-run wording", got)
		}
		if !strings.Contains(got, "wizardly_goldberg") {
			t.Errorf("output = %q, want the orphan named", got)
		}
		if !strings.Contains(got, "Kept 1 container") {
			t.Errorf("output = %q, want the kept container counted", got)
		}
	})

	t.Run("nothing to do", func(t *testing.T) {
		out := &bytes.Buffer{}
		printSlotReapReport(slotTestCmd(out), slot.ReapReport{OlderThan: 30 * time.Minute})
		if got := out.String(); !strings.Contains(got, "No gate debris") {
			t.Errorf("output = %q, want the clean reading", got)
		}
	})

	t.Run("owner files", func(t *testing.T) {
		out := &bytes.Buffer{}
		printSlotReapReport(slotTestCmd(out), slot.ReapReport{
			OlderThan:  30 * time.Minute,
			OwnerFiles: []slot.StaleOwnerFile{{Slot: 0, PID: 4242, AcquiredAt: time.Date(2026, 9, 21, 10, 41, 0, 0, time.UTC)}},
		})
		got := out.String()
		if !strings.Contains(got, "slot 0") || !strings.Contains(got, "pid 4242") {
			t.Errorf("output = %q, want the stale owner file named", got)
		}
	})
}

func TestPrintSlotReapJSON(t *testing.T) {
	out := &bytes.Buffer{}
	err := printSlotReapJSON(slotTestCmd(out), slot.ReapReport{
		OlderThan: 30 * time.Minute,
		Debris: []slot.ContainerVerdict{{
			Container: slot.GateContainer{ID: "orphan-id", Image: "dolt/dolt-sql-server:2.2.0", Name: "orphan", Labels: map[string]string{"k": "v"}},
			Verdict:   slot.VerdictDebris,
			Reason:    "old",
		}},
		Removed: []string{"dolt/dolt-sql-server:2.2.0 orphan"},
	})
	if err != nil {
		t.Fatalf("printSlotReapJSON: %v", err)
	}

	var decoded struct {
		OlderThan string `json:"older_than"`
		DryRun    bool   `json:"dry_run"`
		Debris    []struct {
			Container string `json:"container"`
			Labels    string `json:"labels"`
		} `json:"debris"`
		Removed []string `json:"removed"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("decoding output %q: %v", out.String(), err)
	}
	if decoded.OlderThan != "30m0s" {
		t.Errorf("older_than = %q, want 30m0s", decoded.OlderThan)
	}
	if len(decoded.Debris) != 1 || decoded.Debris[0].Labels != "k=v" {
		t.Errorf("debris = %+v, want the orphan with its labels", decoded.Debris)
	}
	if len(decoded.Removed) != 1 {
		t.Errorf("removed = %v, want the removed container", decoded.Removed)
	}
}

// TestPrintSlotDebris pins the status-side half of the verdict: the container
// the gate walked past is named, so an operator reading "free" can see what
// the gate decided about.
func TestPrintSlotDebris(t *testing.T) {
	out := &bytes.Buffer{}
	printSlotDebris(slotTestCmd(out), []string{"dolt/dolt-sql-server:2.2.0 wizardly_goldberg"})
	got := out.String()
	if !strings.Contains(got, "Gate debris (1)") || !strings.Contains(got, "wizardly_goldberg") {
		t.Errorf("output = %q, want the debris container named", got)
	}
	if !strings.Contains(got, "gt slot reap") {
		t.Errorf("output = %q, want it to point at the command that removes it", got)
	}

	empty := &bytes.Buffer{}
	printSlotDebris(slotTestCmd(empty), nil)
	if empty.Len() != 0 {
		t.Errorf("output = %q, want nothing printed when there is no debris", empty.String())
	}
}
