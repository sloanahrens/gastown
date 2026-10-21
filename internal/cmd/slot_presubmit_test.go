package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/slot"
)

// TestPrintPresubmitStatusText_ListsHolders: gt slot status names the polecat
// holding each presubmit slot, and a free slot reads free.
func TestPrintPresubmitStatusText_ListsHolders(t *testing.T) {
	t.Parallel()

	held := &slot.Owner{Role: "gastown/polecats/alpha", PID: 4242, AcquiredAt: time.Now().Add(-90 * time.Second)}
	rep := slot.Report{
		Held:      true,
		Owner:     held,
		HeldCount: 1,
		Total:     2,
		Slots: []slot.SlotState{
			{Index: 0, Held: true, Owner: held},
			{Index: 1},
		},
	}

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printPresubmitStatusText(cmd, rep)
	out := buf.String()

	for _, want := range []string{
		"Presubmit pool: 1/2 held",
		"presubmit slot 0: held by gastown/polecats/alpha (pid 4242",
		"presubmit slot 1: free",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestPrintPresubmitStatusText_HeldWithoutOwner: a held slot whose owner file
// is unreadable says so, rather than reading as free.
func TestPrintPresubmitStatusText_HeldWithoutOwner(t *testing.T) {
	t.Parallel()

	rep := slot.Report{HeldCount: 1, Total: 1, Slots: []slot.SlotState{{Index: 0, Held: true}}}
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printPresubmitStatusText(cmd, rep)

	if out := buf.String(); !strings.Contains(out, "presubmit slot 0: held (owner metadata unavailable)") {
		t.Errorf("held slot without owner not reported as held:\n%s", out)
	}
}
