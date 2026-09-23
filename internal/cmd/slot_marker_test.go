package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/slot"
)

// markerRow is the report row an om review in flight produces: held, numbered
// after the pool, and carrying the holder metadata its readers key on.
func markerRow(index int) slot.SlotState {
	return slot.SlotState{
		Index:  index,
		Held:   true,
		Marker: true,
		Name:   "om-review-gt-mr-1",
		Owner: &slot.Owner{
			Role:       "gastown/om-review",
			PID:        os.Getpid(),
			AcquiredAt: time.Now().Add(-3 * time.Minute),
			Slot:       index,
			Name:       "om-review-gt-mr-1",
		},
	}
}

// markerReport is a one-slot pool with an om review in flight: slot 0 free, and
// the review's marker numbered after it.
func markerReport() slot.Report {
	return slot.Report{Slots: []slot.SlotState{{Index: 0}, markerRow(1)}, Total: 1}
}

func slotStatusTextOf(t *testing.T, rep slot.Report) string {
	t.Helper()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printSlotStatusText(cmd, rep)
	return buf.String()
}

// TestPrintSlotStatusText_ListsTheMarkerOutsideThePoolListing is gt-97cm's
// acceptance for the status half: the review is named, with the age its callers
// ask about, and it is not printed as a pool slot the pool does not count.
func TestPrintSlotStatusText_ListsTheMarkerOutsideThePoolListing(t *testing.T) {
	rep := slot.Report{
		Slots:    []slot.SlotState{{Index: 0}, {Index: 1}, markerRow(2)},
		Total:    2,
		Reserved: 1,
	}

	out := slotStatusTextOf(t, rep)

	if !strings.Contains(out, "Container-gate pool: 0/2 held") {
		t.Errorf("the pool's own count should be untouched by a marker:\n%s", out)
	}
	if strings.Contains(out, "slot 2") {
		t.Errorf("the marker's index leaked into the pool listing:\n%s", out)
	}
	if !strings.Contains(out, "In-flight marker om-review-gt-mr-1") {
		t.Errorf("the marker is not named:\n%s", out)
	}
	if !strings.Contains(out, "gastown/om-review") {
		t.Errorf("the marker's role (what rebuild-gt keys on) is missing:\n%s", out)
	}
	if !strings.Contains(out, "age 3m0s") {
		t.Errorf("the marker is listed without its age:\n%s", out)
	}
}

// TestPrintSlotStatusJSON_ReportsAMarkerOutsideThePoolCount pins the wire
// contract the other readers consume: an in-flight review is a slots[] entry a
// machine can tell from a pool slot, while total stays the pool's own count.
func TestPrintSlotStatusJSON_ReportsAMarkerOutsideThePoolCount(t *testing.T) {
	out := slotStatusJSONOf(t, markerReport(), nil)

	var decoded struct {
		Total int `json:"total"`
		Slots []struct {
			Index  int    `json:"index"`
			Held   bool   `json:"held"`
			Marker bool   `json:"marker"`
			Name   string `json:"name"`
			Owner  *struct {
				Role string `json:"role"`
				PID  int    `json:"pid"`
			} `json:"owner"`
		} `json:"slots"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	if decoded.Total != 1 {
		t.Errorf("total = %d, want the pool's 1", decoded.Total)
	}
	if len(decoded.Slots) != 2 {
		t.Fatalf("slots = %d entries, want the pool slot plus the marker: %s", len(decoded.Slots), out)
	}
	if decoded.Slots[0].Marker {
		t.Errorf("slot 0 reported as a marker: %+v", decoded.Slots[0])
	}
	m := decoded.Slots[1]
	if !m.Marker || !m.Held || m.Name != "om-review-gt-mr-1" {
		t.Fatalf("marker entry lost its identity: %+v", m)
	}
	if m.Owner == nil || m.Owner.Role != "gastown/om-review" || m.Owner.PID != os.Getpid() {
		t.Fatalf("marker entry lost its holder: %+v", m.Owner)
	}
}
