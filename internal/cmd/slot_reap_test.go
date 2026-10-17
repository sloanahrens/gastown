package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
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
	t.Parallel()
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

	// A held gate leaves age-only debris in Kept, so the report must not read
	// as "no gate debris" (gt-c115n).
	t.Run("held gate", func(t *testing.T) {
		out := &bytes.Buffer{}
		printSlotReapReport(slotTestCmd(out), slot.ReapReport{
			OlderThan: 30 * time.Minute,
			Kept:      []slot.ContainerVerdict{{Container: slot.GateContainer{Image: "testcontainers/ryuk:0.9.0", Name: "ryuk-old"}, Verdict: slot.VerdictDebris}},
			HeldSlots: []int{0},
		})
		got := out.String()
		if !strings.Contains(got, "slot(s) 0 are held") {
			t.Errorf("output = %q, want the held slot named", got)
		}
		if strings.Contains(got, "No gate debris") {
			t.Errorf("output = %q, want no clean-window claim while a suite holds the gate", got)
		}
	})
}

func TestPrintSlotReapJSON(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// reapWithFailedRemoval is what slot.Reap returns when one of two debris
// containers will not go: both sit in Debris, the stuck one in Failed with no
// error from Reap, the other in Removed.
func reapWithFailedRemoval() (slot.ReapReport, slot.ContainerVerdict, slot.ContainerVerdict) {
	stuck := slot.ContainerVerdict{
		Container: slot.GateContainer{ID: "stuck-id", Image: "dolthub/dolt-sql-server:2.2.0", Name: "stuck_one"},
		Verdict:   slot.VerdictDebris,
		Reason:    "owning test process is gone",
	}
	gone := slot.ContainerVerdict{
		Container: slot.GateContainer{ID: "gone-id", Image: "dolthub/dolt-sql-server:2.2.0", Name: "gone_one"},
		Verdict:   slot.VerdictDebris,
		Reason:    "owning test process is gone",
	}
	return slot.ReapReport{
		OlderThan: 30 * time.Minute,
		Debris:    []slot.ContainerVerdict{stuck, gone},
		Removed:   []string{gone.Container.Display()},
		Failed:    []string{stuck.Container.Display() + ": removal in progress"},
	}, stuck, gone
}

// A container whose removal errored sits in Debris and Failed with no error
// from Reap, so the command must turn it into a non-zero exit and must not
// count it as removed (gt-uh7hy).
func TestReap_FailedRemovalIsReported(t *testing.T) {
	t.Parallel()
	report, stuck, gone := reapWithFailedRemoval()

	out := &bytes.Buffer{}
	err := finishSlotReap(slotTestCmd(out), report, nil, false)
	if err == nil {
		t.Fatal("finishSlotReap returned nil for a run with a failed removal, want a non-zero exit")
	}
	got := out.String()
	if !strings.Contains(got, "Removed 1 of 2 gate container(s)") {
		t.Errorf("output = %q, want the failed container left out of the removed count", got)
	}
	if !strings.Contains(got, "failed: ") || !strings.Contains(got, "stuck_one") {
		t.Errorf("output = %q, want the failed container named", got)
	}

	report.Failed, report.Removed = nil, []string{stuck.Container.Display(), gone.Container.Display()}
	out.Reset()
	if err := finishSlotReap(slotTestCmd(out), report, nil, false); err != nil {
		t.Errorf("finishSlotReap = %v for a run that removed everything, want nil", err)
	}
	if got := out.String(); !strings.Contains(got, "Removed 2 gate container(s)") {
		t.Errorf("output = %q, want the plain count when nothing failed", got)
	}
}

// TestReapGateContainers drives the command from the town lookup to the exit
// error in both output modes, so a failed removal that reaches the report but
// not the exit code, or that the JSON path leaves out, is caught where the
// operator sees it (gt-ylkm2).
func TestReapGateContainers(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	findTown := func() (string, error) { return town, nil }

	t.Run("failed removal text", func(t *testing.T) {
		t.Parallel()
		report, _, _ := reapWithFailedRemoval()
		var gotTown string
		var gotOpts slot.ReapOptions
		reap := func(townRoot string, opts slot.ReapOptions) (slot.ReapReport, error) {
			gotTown, gotOpts = townRoot, opts
			return report, nil
		}

		out := &bytes.Buffer{}
		err := reapGateContainers(slotTestCmd(out), findTown, reap, slot.ReapOptions{OlderThan: 30 * time.Minute, DryRun: true}, false)
		if err == nil || !strings.Contains(err.Error(), "1 gate container(s) could not be removed") {
			t.Fatalf("error = %v, want the failed count as the exit error", err)
		}
		if gotTown != town || !gotOpts.DryRun || gotOpts.OlderThan != 30*time.Minute {
			t.Errorf("reap got town %q opts %+v, want the lookup's town and the flags passed through", gotTown, gotOpts)
		}
		got := out.String()
		if !strings.Contains(got, "stuck_one") || !strings.Contains(got, "removal in progress") {
			t.Errorf("output = %q, want the failed container and docker's reason named", got)
		}
	})

	t.Run("failed removal json", func(t *testing.T) {
		t.Parallel()
		report, _, _ := reapWithFailedRemoval()
		reap := func(string, slot.ReapOptions) (slot.ReapReport, error) { return report, nil }

		out := &bytes.Buffer{}
		err := reapGateContainers(slotTestCmd(out), findTown, reap, slot.ReapOptions{}, true)
		if err == nil || !strings.Contains(err.Error(), "1 gate container(s) could not be removed") {
			t.Fatalf("error = %v, want the failed count as the exit error", err)
		}
		var decoded struct {
			Removed []string `json:"removed"`
			Failed  []string `json:"failed"`
		}
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatalf("decoding output %q: %v", out.String(), err)
		}
		if len(decoded.Removed) != 1 || !strings.Contains(decoded.Removed[0], "gone_one") {
			t.Errorf("removed = %v, want only gone_one", decoded.Removed)
		}
		if len(decoded.Failed) != 1 || !strings.Contains(decoded.Failed[0], "stuck_one") || !strings.Contains(decoded.Failed[0], "removal in progress") {
			t.Errorf("failed = %v, want stuck_one with docker's reason", decoded.Failed)
		}
	})

	t.Run("reap error wins", func(t *testing.T) {
		t.Parallel()
		report, _, _ := reapWithFailedRemoval()
		boom := errors.New("listing gate containers: docker unreachable")
		reap := func(string, slot.ReapOptions) (slot.ReapReport, error) { return report, boom }

		out := &bytes.Buffer{}
		if err := reapGateContainers(slotTestCmd(out), findTown, reap, slot.ReapOptions{}, false); !errors.Is(err, boom) {
			t.Errorf("error = %v, want Reap's own error", err)
		}
		if !strings.Contains(out.String(), "stuck_one") {
			t.Errorf("output = %q, want the report printed before the error", out.String())
		}
	})

	t.Run("not in a town", func(t *testing.T) {
		t.Parallel()
		noTown := func() (string, error) { return "", errors.New("no town") }
		reap := func(string, slot.ReapOptions) (slot.ReapReport, error) {
			t.Error("reap ran with no town")
			return slot.ReapReport{}, nil
		}
		err := reapGateContainers(slotTestCmd(&bytes.Buffer{}), noTown, reap, slot.ReapOptions{}, false)
		if err == nil || !strings.Contains(err.Error(), "not in a Gas Town workspace") {
			t.Errorf("error = %v, want the workspace refusal", err)
		}
	})
}
