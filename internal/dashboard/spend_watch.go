package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// spendWatchName is the file the hub keeps current from the spend report it
// already reads: <town>/.runtime/watch/spend.json.
const spendWatchName = "spend.json"

// spendWatch is the document that file carries, the one the hourly snapshot
// (internal/metrics moneyAddDeepSeek), gt tail's rate and gt report's spend
// section read as the town's live DeepSeek reading. The readers take ts as the
// reading's instant and both figures as optional, so a report that carries no
// rate writes no per_hour rather than a zero.
type spendWatch struct {
	TS      time.Time `json:"ts"`
	PerHour *float64  `json:"per_hour,omitempty"`
	Balance float64   `json:"balance"`
}

// writeSpendWatch rewrites the watch's spend file from one spend report at now,
// which is the instant the report was read. A report that carries no balance is
// no reading and writes nothing: the old file keeps its own age rather than
// being restamped with a figure the report does not have. The write goes to a
// temporary file beside the target and is renamed over it, so a reader sees the
// previous reading or this one and never a half-written document.
func writeSpendWatch(dir string, report json.RawMessage, now time.Time) error {
	if dir == "" {
		return nil
	}
	var d deepSeekSpend
	if err := json.Unmarshal(report, &d); err != nil {
		return fmt.Errorf("spend watch: reading the spend report: %w", err)
	}
	if d.Balance == nil {
		return nil
	}
	// The file has one per_hour field, and the 1-hour pace is the report's
	// current rate: the 24-hour pace beside it is a daily average.
	doc := spendWatch{TS: now.UTC(), PerHour: d.Pace1h.PerHour, Balance: *d.Balance}
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("spend watch: encoding the reading: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("spend watch: creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, spendWatchName)
	// The temporary name is fixed and its write truncates, so a run that died
	// mid-write leaves a file the next one overwrites rather than a directory
	// that fills with them.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		removeSpendTemp(tmp)
		return fmt.Errorf("spend watch: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		removeSpendTemp(tmp)
		return fmt.Errorf("spend watch: replacing %s: %w", path, err)
	}
	return nil
}

// removeSpendTemp drops the temporary file after a failed write, so nothing is
// left beside the reading that failed to land. A removal that fails is not
// reported over the failure that caused it.
func removeSpendTemp(path string) {
	_ = os.Remove(path)
}
