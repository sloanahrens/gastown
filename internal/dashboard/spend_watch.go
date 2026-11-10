package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
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
	return writeSpendWatchWith(dir, report, now, createSpendTemp)
}

// spendWatchWriter is the hub's copy of writeSpendWatch, with the state a
// read-only directory needs. The first refusal the filesystem gives as read-only
// latches it: the writer logs one line and stops trying until the dashboard
// restarts. The reading the page holds is in memory and is not affected, so the
// latch is not a failed read.
type spendWatchWriter struct {
	// create makes the temporary file a reading is written to. It is a field so
	// a test refuses the create the way a read-only town does.
	create func(dir string) (spendTempFile, error)
	// logf writes the one line a read-only directory earns.
	logf     func(format string, args ...any)
	readOnly bool
}

// write rewrites the watch file from one spend report, as writeSpendWatch does.
// A read-only refusal is reported as nil, after the latch.
func (w *spendWatchWriter) write(dir string, report json.RawMessage, now time.Time) error {
	if w.readOnly {
		return nil
	}
	err := writeSpendWatchWith(dir, report, now, w.create)
	if !IsReadOnlyErr(err) {
		return err
	}
	w.readOnly = true
	w.logf("the spend watch file in %s is read-only (%v); it is not rewritten until the dashboard restarts", dir, err)
	return nil
}

// writeSpendWatchWith is writeSpendWatch with its temporary-file create injected.
func writeSpendWatchWith(dir string, report json.RawMessage, now time.Time, create func(dir string) (spendTempFile, error)) error {
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
	return writeSpendWatchVia(dir, data, create)
}

// spendTempFile is the temporary file a reading is written to. An *os.File is
// one; a test substitutes a file whose write or close it scripts to fail.
type spendTempFile interface {
	io.Writer
	io.Closer
	Name() string
}

// writeSpendWatchVia writes the encoded reading to a temporary file made by
// create, then renames it over the watch file. create is injected so a test can
// script the refusals a directory the writer cannot write gives, which a root
// run does not produce.
func writeSpendWatchVia(dir string, data []byte, create func(dir string) (spendTempFile, error)) error {
	path := filepath.Join(dir, spendWatchName)
	f, err := create(dir)
	if err != nil {
		return fmt.Errorf("spend watch: writing %s: %w", dir, err)
	}
	tmp := f.Name()
	// A failed write or close leaves the same way as a refused rename: the
	// temporary file goes and the reading is reported. Close runs even when the
	// write failed, so the descriptor is never leaked.
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		removeSpendTemp(tmp)
		return fmt.Errorf("spend watch: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		removeSpendTemp(tmp)
		return fmt.Errorf("spend watch: replacing %s: %w", path, err)
	}
	return nil
}

// createSpendTemp creates a temporary file beside the watch file under a random
// name no other writer holds, so two hubs writing the same town each rename
// their own reading and neither renames over a file the other is still writing.
// It is created 0644 as os.WriteFile creates it, so the umask applies as it
// always has. O_EXCL means a name already taken is an error the poll reports,
// never a file it overwrites; a 64-bit random name makes that unlikely, and the
// next poll draws a new name. A run that died mid-write leaves its own *.tmp
// behind, where the fixed name used to be overwritten by the next run.
func createSpendTemp(dir string) (spendTempFile, error) {
	name := filepath.Join(dir, fmt.Sprintf("%s.%016x.tmp", spendWatchName, rand.Uint64()))
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// removeSpendTemp drops the temporary file after a failed write, so nothing is
// left beside the reading that failed to land. A removal that fails is not
// reported over the failure that caused it.
func removeSpendTemp(path string) {
	_ = os.Remove(path)
}
