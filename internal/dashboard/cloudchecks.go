package dashboard

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
)

// The Cloud pane's checks line: the cloud-check plugin's latest status. The
// patrol's report says what the patrol found; this says whether the checks that
// cover what the patrol cannot see — the app URLs answering, the deploy runners
// online, Colima and Docker Desktop up, the release bot's flags — ran and what
// they reported. The file is another process's output, so nothing here trusts
// its size, its shape or its text: an oversized, unreadable or misshapen file
// is one named error rather than a guess, and every string the page shows is
// stripped and capped first.

const (
	// cloudChecksMaxBytes refuses a larger file before reading it, so a file
	// that is not the plugin's own cannot be read into memory to be rejected.
	// The plugin's own status runs a few kilobytes.
	cloudChecksMaxBytes = 64 << 10

	// cloudChecksMaxItems is how many warning and failure rows the page draws.
	// The plugin's status carries only the warnings and failures, so a status
	// with more than this is cut with the counts still covering the whole run.
	cloudChecksMaxItems = 30

	// The cap on each string the page shows, in runes: a Section is a short
	// label, and the Text is the one prose field.
	cloudChecksSectionMax = 60
	cloudChecksTextMax    = 200

	// cloudChecksOverdue is how long after its own timestamp the status reads
	// as stale. The plugin runs every 10 minutes, so this leaves two missed
	// runs of slack before the pane says it has stopped (gt-im2c8).
	cloudChecksOverdue = 25 * time.Minute
)

// The cloud checks states: fresh is a status inside the overdue window, stale
// one past it, and error one the reader could not use.
const (
	CloudChecksFresh = "fresh"
	CloudChecksStale = "stale"
	CloudChecksError = "error"
)

// cloudChecksUnreadable is the one line the page shows for a status it could
// not read. It names the reader, not the plugin, so the operator knows to look
// at the file rather than at the checks.
const cloudChecksUnreadable = "cloud checks: unreadable"

// The levels the plugin writes and the page draws: a failure and a warning.
// Any other level is not this reader's to interpret, so its item is dropped.
const (
	cloudCheckFail = "fail"
	cloudCheckWarn = "warn"
)

// CloudChecks is the plugin's latest status as the pane draws it: when it last
// ran, whether that is stale, the counts over the whole run, and the warnings
// and failures it found.
type CloudChecks struct {
	// At is when the plugin wrote the status, read out of the file's own
	// timestamp. The pane draws its age from this.
	At time.Time `json:"at"`
	// State is fresh, stale, or the error the reader stopped on. The pane
	// composes the line from it, so a state and the line it draws cannot come
	// apart.
	State string `json:"state"`
	// OK, Warnings and Failed count the checks that passed, warned and failed.
	// They cover the whole run, not the capped list the page draws.
	OK       int `json:"ok"`
	Warnings int `json:"warnings"`
	Failed   int `json:"failed"`
	// Items are the run's warnings and failures, most severe first, at most
	// cloudChecksMaxItems of them.
	Items []CloudCheckItem `json:"items"`
}

// CloudCheckItem is one warning or failure as the pane draws it, every string
// stripped and capped.
type CloudCheckItem struct {
	Level   string `json:"level"` // fail or warn
	Section string `json:"section,omitempty"`
	Text    string `json:"text"`
}

// CloudChecksReader reads one cloud-check status file.
type CloudChecksReader struct{ path string }

// NewCloudChecksReader returns a reader of the status file at path. The command
// wiring names the path (under the town root); a reader reads the one it is
// given.
func NewCloudChecksReader(path string) *CloudChecksReader { return &CloudChecksReader{path: path} }

// Read returns the plugin's latest status as it stands at now, or nil when
// there is no status file at all: a plugin that is not installed is nothing to
// draw, while a file the reader cannot use is a named error for the operator.
func (r *CloudChecksReader) Read(now time.Time) *CloudChecks {
	raw, err := readCloudChecksFile(r.path, cloudChecksMaxBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return &CloudChecks{At: now, State: CloudChecksError}
	}
	var st cloudChecksStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return &CloudChecks{At: now, State: CloudChecksError}
	}
	if st.At <= 0 || st.OK < 0 || st.Warnings < 0 || st.Failed < 0 {
		return &CloudChecks{At: now, State: CloudChecksError}
	}
	c := &CloudChecks{
		At:       time.Unix(st.At, 0),
		OK:       st.OK,
		Warnings: st.Warnings,
		Failed:   st.Failed,
		Items:    cloudCheckItems(st.Items),
	}
	if now.Sub(c.At) > cloudChecksOverdue {
		c.State = CloudChecksStale
	} else {
		c.State = CloudChecksFresh
	}
	return c
}

// cloudChecksStatus is the shape of the file the plugin writes, a JSON object
// per run overwritten each time. At is the run's time in unix seconds.
type cloudChecksStatus struct {
	At       int64             `json:"at"`
	Failed   int               `json:"failed"`
	Warnings int               `json:"warnings"`
	OK       int               `json:"ok"`
	Items    []cloudCheckEntry `json:"items"`
}

// cloudCheckEntry is one item as the file writes it. A level this reader does
// not know is dropped rather than guessed at.
type cloudCheckEntry struct {
	Level   string `json:"level"`
	Section string `json:"section"`
	Text    string `json:"text"`
}

// cloudCheckItems is the status's items as the page may show them: the known
// levels only, each string stripped and capped, warnings and failures ordered
// with the failures first, and the list kept to cloudChecksMaxItems.
func cloudCheckItems(in []cloudCheckEntry) []CloudCheckItem {
	out := []CloudCheckItem{}
	for _, e := range in {
		level := strings.ToLower(strings.TrimSpace(e.Level))
		if level != cloudCheckFail && level != cloudCheckWarn {
			continue
		}
		out = append(out, CloudCheckItem{
			Level:   level,
			Section: cloudText(e.Section, cloudChecksSectionMax),
			Text:    cloudText(e.Text, cloudChecksTextMax),
		})
	}
	// Stable, so items of one level keep the file's own order.
	sort.SliceStable(out, func(i, j int) bool { return cloudCheckRank(out[i].Level) < cloudCheckRank(out[j].Level) })
	if len(out) > cloudChecksMaxItems {
		out = out[:cloudChecksMaxItems]
	}
	return out
}

// cloudCheckRank orders the levels the way the page lists them: failures first.
func cloudCheckRank(level string) int {
	if level == cloudCheckFail {
		return 0
	}
	return 1
}

// readCloudChecksFile reads path, refusing a file larger than max before it is
// read.
func readCloudChecksFile(path string, max int64) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > max {
		return nil, errors.New("cloud checks: status over the size cap")
	}
	return os.ReadFile(path)
}
