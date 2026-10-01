package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/reaper"
)

// Bookkeeping for the reaper's open-wisp alert (gt-11kyy): the previous
// cycle's reading, so a cycle can be judged against something. No single count
// can say whether the wisp lifecycle is healthy — only a change can — so the
// alert compares cycles, and this is where the one it compares against lives.
//
// Both the daemon patrol and a hand-run `gt reaper run` read and write it, so a
// town that only ever reaps by hand still builds the series the daemon needs.

const wispAlertBaselineFileName = "wisp_open_baseline.json"

// WispAlertBaselinePath returns the path of the file holding the reaper's last
// recorded open-wisp sample.
func WispAlertBaselinePath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", wispAlertBaselineFileName)
}

// LoadWispAlertBaseline returns the sample the last reaper cycle recorded, and
// nil when none has been recorded. An unreadable or corrupt file is an error
// the caller reports and then treats as no baseline: a reading that cannot be
// trusted is one to replace, not one to compare against (gt-11kyy).
func LoadWispAlertBaseline(townRoot string) (*reaper.OpenWispSample, error) {
	data, err := os.ReadFile(WispAlertBaselinePath(townRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sample reaper.OpenWispSample
	if err := json.Unmarshal(data, &sample); err != nil {
		return nil, fmt.Errorf("parse %s: %w", wispAlertBaselineFileName, err)
	}
	return &sample, nil
}

// SaveWispAlertBaseline records this cycle's sample as the reading the next
// cycle is judged against.
func SaveWispAlertBaseline(townRoot string, sample reaper.OpenWispSample) error {
	path := WispAlertBaselinePath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return atomicfile.WriteJSON(path, sample)
}
