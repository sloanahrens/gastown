package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The Cloud panel's app power line: for each app, whether it is on, and when it
// is off the environment, since when, by whom and why, beside the release bot's
// latest ledger line for it. The markers and the ledgers are other accounts'
// files, so this reader holds to the same rules as cloud.go: it opens them for
// reading only, bounds every read, strips and caps every string, and never lets
// a file it cannot make sense of read as an app that is on.

const (
	appOffSuffix    = ".off"
	appLedgerSuffix = ".jsonl"

	// appMarkerMaxBytes bounds the read of a marker. A marker is one short line;
	// only its first line is read, and a longer file is cut here rather than
	// read whole.
	appMarkerMaxBytes = 4 << 10

	// appLedgerTailBytes bounds the read of a ledger. A ledger grows by a line
	// per release run, so only this much of its end is read, never the file.
	// A ledger line runs about 150 bytes.
	appLedgerTailBytes = 8 << 10

	// appMaxApps is how many app rows the page draws; the rest are counted in More.
	appMaxApps = 50

	// The cap on each string the page shows, in runes.
	appNameMax   = 40
	appByMax     = 40
	appReasonMax = 160
	appActionMax = 40
)

// App power states. Unknown is an app the reader could not place: the page
// never reads it as on.
const (
	AppOn      = "on"
	AppOff     = "off"
	AppUnknown = "unknown"
)

// The environments a marker names. A marker with no env= is the legacy form and
// means all.
const (
	AppEnvAll     = "all"
	AppEnvStaging = "staging"
	AppEnvProd    = "prod"
)

// The sentences a row or the line carries for what the reader could not read.
const (
	appOffDirUnreadable = "off directory unreadable"
	appMarkerUnreadable = "unreadable marker"
	appMarkerReadFailed = "marker unreadable"
	appMarkerNotAFile   = "marker is not a regular file"
	appLedgerBadLine    = "ledger line unreadable"
	appLedgerReadFailed = "ledger unreadable"
)

// AppRelease is the release bot's latest ledger line for an app, every string
// stripped and capped.
type AppRelease struct {
	At     time.Time `json:"at,omitzero"`
	Action string    `json:"action,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// AppPowerRow is one app as the line draws it.
type AppPowerRow struct {
	App   string `json:"app"`
	State string `json:"state"` // on, off, unknown
	// Env is where an off app is off: all, staging or prod. It is empty for an
	// app that is on or unknown, and for a marker the reader could not parse.
	Env    string    `json:"env,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	By     string    `json:"by,omitempty"`
	Reason string    `json:"reason,omitempty"`
	// Note says why a state is not the plain one: the reason an app is unknown,
	// or that an off app's marker did not parse.
	Note string `json:"note,omitempty"`
	// Release is the bot's latest ledger line, nil when the app has no ledger or
	// an empty one. ReleaseNote says why a ledger that exists could not be read.
	Release     *AppRelease `json:"release,omitempty"`
	ReleaseNote string      `json:"release_note,omitempty"`
}

// AppPower is the app power line: one row per app, sorted by name.
type AppPower struct {
	At   time.Time     `json:"at"`
	Apps []AppPowerRow `json:"apps"`
	// More counts the apps past appMaxApps that the page does not draw.
	More int `json:"more,omitempty"`
	// Note names why the line has no rows to draw because the reader could not
	// look: an off directory it cannot list, with no ledger to name an app.
	Note string `json:"note,omitempty"`
}

// AppPowerReader reads the off markers directory and the release ledgers
// directory.
type AppPowerReader struct{ offDir, releasesDir string }

// NewAppPowerReader returns a reader of the markers in offDir and the ledgers
// in releasesDir. The command wiring names both (config.CloudOffDir and
// config.CloudReleasesDir); a reader reads the two it is given.
func NewAppPowerReader(offDir, releasesDir string) *AppPowerReader {
	return &AppPowerReader{offDir: offDir, releasesDir: releasesDir}
}

// Read returns the app power line as it stands at now, or nil when neither
// directory is there: a machine with no cloud setup has no apps to draw. The app
// list is the union of the two directories' file names. An app is on only when
// the markers directory was read and holds no marker for it.
func (r *AppPowerReader) Read(now time.Time) *AppPower {
	p := &AppPower{At: now, Apps: []AppPowerRow{}}
	markers, offErr := listApps(r.offDir, appOffSuffix)
	ledgers, relErr := listApps(r.releasesDir, appLedgerSuffix)
	if errors.Is(offErr, fs.ErrNotExist) && errors.Is(relErr, fs.ErrNotExist) {
		return nil
	}

	names := map[string]struct{}{}
	for n := range markers {
		names[n] = struct{}{}
	}
	for n := range ledgers {
		names[n] = struct{}{}
	}
	if offErr != nil && len(names) == 0 {
		p.Note = appOffDirUnreadable
		return p
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	if len(sorted) > appMaxApps {
		p.More = len(sorted) - appMaxApps
		sorted = sorted[:appMaxApps]
	}

	for _, name := range sorted {
		row := AppPowerRow{App: cloudText(name, appNameMax)}
		switch {
		case offErr != nil:
			row.State, row.Note = AppUnknown, appOffDirUnreadable
		default:
			if file, off := markers[name]; off {
				readMarker(&row, filepath.Join(r.offDir, file))
			} else {
				row.State = AppOn
			}
		}
		if file, ok := ledgers[name]; ok {
			readLedger(&row, filepath.Join(r.releasesDir, file))
		} else if relErr != nil && !errors.Is(relErr, fs.ErrNotExist) {
			row.ReleaseNote = appLedgerReadFailed
		}
		p.Apps = append(p.Apps, row)
	}
	return p
}

// listApps maps the app name of each entry in dir ending in suffix to the
// entry's name. An entry that is not a regular file is kept: its name still
// stands for an app, and the readers report it as one they cannot read rather
// than the app being on. The error is the directory's own when it cannot be listed.
func listApps(dir, suffix string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		file := e.Name()
		name := strings.TrimSuffix(file, suffix)
		if name == "" || name == file || strings.HasPrefix(file, ".") {
			continue
		}
		out[name] = file
	}
	return out, nil
}

// readMarker fills row from the marker at path. A marker is one line of
// space-separated key=value fields, since=<RFC3339> by=<user> env=<all|staging|prod>
// reason=<text>, with env= missing in the legacy form. Fields the reader does not
// know are skipped, and reason= runs to the end of the line, so it may hold spaces.
// A marker that cannot be read at all is unknown; one that is there but does not
// parse is still off, since a file exists to say the app is stopped.
func readMarker(row *AppPowerRow, path string) {
	line, err := readFirstLine(path, appMarkerMaxBytes)
	if err != nil {
		row.State, row.Note = AppUnknown, appMarkerReadFailed
		if errors.Is(err, errNotRegular) {
			row.Note = appMarkerNotAFile
		}
		return
	}
	row.State = AppOff
	m, ok := parseMarker(line)
	if !ok {
		row.Note = appMarkerUnreadable
		return
	}
	row.Env, row.Since = m.env, m.since
	row.By, row.Reason = cloudText(m.by, appByMax), cloudText(m.reason, appReasonMax)
}

type appMarker struct {
	since           time.Time
	by, env, reason string
}

// parseMarker reads one marker line. It fails on a line with no usable since=,
// or an env= outside the three the wrapper writes, so a marker the reader does
// not understand is shown as such rather than half-read.
func parseMarker(line string) (appMarker, bool) {
	m := appMarker{env: AppEnvAll}
	var haveSince bool
	rest := strings.TrimSpace(line)
	for rest != "" {
		field := rest
		if i := strings.IndexAny(rest, " \t"); i >= 0 {
			field, rest = rest[:i], strings.TrimLeft(rest[i:], " \t")
		} else {
			rest = ""
		}
		key, val, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		switch key {
		case "since":
			t, err := time.Parse(time.RFC3339, val)
			if err != nil {
				return appMarker{}, false
			}
			m.since, haveSince = t, true
		case "by":
			m.by = val
		case "env":
			if val != AppEnvAll && val != AppEnvStaging && val != AppEnvProd {
				return appMarker{}, false
			}
			m.env = val
		case "reason":
			// The reason is the rest of the line, its first word included.
			m.reason = strings.TrimSpace(val + " " + rest)
			rest = ""
		}
	}
	return m, haveSince
}

// readLedger fills row's Release from the last line of the ledger at path.
func readLedger(row *AppPowerRow, path string) {
	line, err := readLastLine(path, appLedgerTailBytes)
	switch {
	case err != nil:
		row.ReleaseNote = appLedgerReadFailed
		return
	case line == "":
		return
	}
	var entry struct {
		Time   string `json:"time"`
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		row.ReleaseNote = appLedgerBadLine
		return
	}
	rel := &AppRelease{
		Action: cloudText(entry.Action, appActionMax),
		Reason: cloudText(entry.Reason, appReasonMax),
	}
	if at, err := time.Parse(time.RFC3339, strings.TrimSpace(entry.Time)); err == nil {
		rel.At = at
	}
	row.Release = rel
}

var errNotRegular = errors.New("not a regular file")

// openRegular opens path for reading when it is a regular file, following a
// symlink to one. A FIFO, socket or device is refused before it is opened, since
// opening a FIFO blocks until a writer appears and the poll has no timeout.
func openRegular(path string) (*os.File, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	return os.Open(path)
}

// readFirstLine reads at most max bytes from the start of path and returns the
// first line of them. It does not open a file that is not a regular one.
func readFirstLine(path string, max int64) (string, error) {
	f, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return "", err
	}
	if i := bytes.IndexAny(b, "\r\n"); i >= 0 {
		b = b[:i]
	}
	return string(b), nil
}

// readLastLine reads at most max bytes from the end of path and returns the last
// non-blank line of them, "" for a file with none. A last line longer than the
// tail cannot be told apart from the middle of one, so it is an error rather than
// a guess. It does not open a file that is not a regular one.
func readLastLine(path string, max int64) (string, error) {
	f, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if fi.Size() > max {
		start = fi.Size() - max
	}
	b := make([]byte, fi.Size()-start)
	n, err := f.ReadAt(b, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	b = bytes.TrimSpace(b[:n])
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		return string(bytes.TrimSpace(b[i+1:])), nil
	}
	if start > 0 {
		return "", errors.New("ledger: last line over the read bound")
	}
	return string(b), nil
}
