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
// section read as the town's live DeepSeek reading. TS is the instant the
// balance is stamped with: the report's own as-of instant, never the poll's, so
// a reading the rows or the ledger no longer advance carries its age rather
// than being passed off as this poll's live rate. The readers take ts as the
// reading's instant and both figures as optional, so a report that carries no
// rate writes no per_hour rather than a zero.
type spendWatch struct {
	TS      time.Time `json:"ts"`
	PerHour *float64  `json:"per_hour,omitempty"`
	Balance float64   `json:"balance"`
}

// writeSpendWatch rewrites the watch's spend file from one spend report. The
// file is stamped with the instant the report's balance is from, and a report
// that names none writes nothing, so a stale figure never overwrites a newer
// reading with the poll's instant (gt-u3fp8i): the town's
// cost rows answer with the newest row's own instant in balance_at, and a file
// source (the ledger, or the tool that reports on it) with the newest sample
// its last_sample names. A report that carries no balance is no reading
// and writes nothing either: the old file keeps its own age rather than being
// restamped with a figure the report does not have. The write goes to a
// temporary file beside the target and is renamed over it, so a reader sees the
// previous reading or this one and never a half-written document.
func writeSpendWatch(dir string, report json.RawMessage) error {
	return writeSpendWatchWith(dir, report, createSpendTemp)
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
func (w *spendWatchWriter) write(dir string, report json.RawMessage) error {
	if w.readOnly {
		return nil
	}
	err := writeSpendWatchWith(dir, report, w.create)
	if !IsReadOnlyErr(err) {
		return err
	}
	w.readOnly = true
	if w.logf != nil {
		w.logf("the spend watch file in %s is read-only (%v); it is not rewritten until the dashboard restarts", dir, err)
	}
	return nil
}

// writeSpendWatchWith is writeSpendWatch with its temporary-file create injected.
func writeSpendWatchWith(dir string, report json.RawMessage, create func(dir string) (spendTempFile, error)) error {
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
	ts, dated, err := spendWatchInstant(&d)
	if err != nil {
		return err
	}
	if !dated {
		return nil
	}
	// The file has one per_hour field, and the 1-hour pace is the report's
	// current rate: the 24-hour pace beside it is a daily average.
	doc := spendWatch{TS: ts.UTC(), PerHour: d.Pace1h.PerHour, Balance: *d.Balance}
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("spend watch: encoding the reading: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("spend watch: creating %s: %w", dir, err)
	}
	return writeSpendWatchVia(dir, data, create)
}

// spendSampleLayouts are the forms a report's last_sample is written in: the
// ledger's own stamp, which the Go report gives to the minute, and the same
// stamp with seconds as some writers format it. Both are naive local times,
// the way the watcher writes balance.log.
var spendSampleLayouts = []string{"2006-01-02T15:04", "2006-01-02T15:04:05"}

// spendWatchInstant is the instant the report's balance is from. A report built
// from the town's cost rows carries it in balance_at, the newest row's own
// instant. A file report carries no balance_at and names the newest sample it
// read in last_sample, the ledger's stamp, which is the instant that balance is
// from. dated is false when the report names neither: the balance is a reading
// the report cannot date, and the watch writer leaves the file's own age alone
// rather than replacing it with the poll's instant. A balance_at of the zero
// instant, or a last_sample that does not parse, is a broken report and an
// error, never a reading.
func spendWatchInstant(d *deepSeekSpend) (ts time.Time, dated bool, err error) {
	if d.BalanceAt != nil {
		if d.BalanceAt.IsZero() {
			return time.Time{}, false, fmt.Errorf("spend watch: the report's balance_at is the zero instant")
		}
		return *d.BalanceAt, true, nil
	}
	if d.LastSample == "" {
		return time.Time{}, false, nil
	}
	for _, layout := range spendSampleLayouts {
		if at, perr := time.ParseInLocation(layout, d.LastSample, time.Local); perr == nil {
			return at, true, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("spend watch: the report's last_sample is not a ledger timestamp")
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
