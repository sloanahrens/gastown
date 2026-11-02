package dashboard

import (
	"encoding/json"
	"errors"
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
	// The temporary file takes a unique name beside the target, so two hubs
	// writing the same town each rename their own reading and neither renames
	// over a temporary file the other is still writing. A run that died mid-write
	// leaves its own *.tmp behind, where the fixed name used to be overwritten by
	// the next run.
	f, err := os.CreateTemp(dir, spendWatchName+".*.tmp")
	if err != nil {
		return fmt.Errorf("spend watch: writing %s: %w", dir, err)
	}
	tmp := f.Name()
	// CreateTemp makes the file 0600; set it to the 0644 the watch file has
	// always had. A failed write, close or chmod and a refused rename all leave
	// the same way: the temporary file goes and the reading is reported.
	_, werr := f.Write(data)
	err = errors.Join(werr, f.Close(), os.Chmod(tmp, 0o644))
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		removeSpendTemp(tmp)
		return fmt.Errorf("spend watch: writing %s: %w", path, err)
	}
	return nil
}

// removeSpendTemp drops the temporary file after a failed write, so nothing is
// left beside the reading that failed to land. A removal that fails is not
// reported over the failure that caused it.
func removeSpendTemp(path string) {
	_ = os.Remove(path)
}
