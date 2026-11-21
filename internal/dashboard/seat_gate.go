package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// readSeatGate is the gate time the overrun guard has charged a seat's current
// sling: gate_spent, in the nanoseconds a duration marshals to, from
// <runtimeDir>/overrun-guard/<bead>.json. ok is false when the file is missing,
// the bead id could leave that directory, the file does not parse, or the value
// is negative: the seat then has no gate time to show, never an error the hub
// must handle. A zero gate is a valid answer: the guard's omitempty leaves the
// key out.
func readSeatGate(runtimeDir, bead string) (time.Duration, bool) {
	if bead == "" || bead != filepath.Base(bead) || strings.ContainsAny(bead, `/\`) {
		return 0, false
	}
	data, err := os.ReadFile(filepath.Join(runtimeDir, "overrun-guard", bead+".json"))
	if err != nil {
		return 0, false
	}
	var doc struct {
		GateSpent time.Duration `json:"gate_spent"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.GateSpent < 0 {
		return 0, false
	}
	return doc.GateSpent, true
}
