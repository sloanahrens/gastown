package dashboard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// The Report panel: the overseer's latest hourly report. The overseer writes
// report files with a tool outside the repo — one file per run, named for the
// instant it was written, plus the latest.md copy that tool leaves for a human
// to read — and this dashboard only reads the directory. So the report the pane
// shows is the newest file that names a timestamp, never a guess at which file
// is current, and a directory the reader cannot read is a named state rather
// than an empty pane (a failed read must not look like no reports at all).
//
// The reader opens files for reading and writes nothing, so nothing on this
// page can reach into the overseer's own output.

const (
	// reportsNameLayout is the name a report file carries: the UTC instant the
	// overseer wrote it, then .md. The reader takes a report's time from its
	// name rather than the file's own times, which a copy or a sync resets.
	reportsNameLayout = "20060102T150405Z"
	reportsExt        = ".md"

	// reportsNameShape is the name a route may ask for, as a whole: the digits
	// the layout above writes and nothing else, so a separator, a dot or a
	// letter is refused before any path could be built from it.
	reportsNameShape = `^[0-9]{8}T[0-9]{6}Z$`

	// reportsMaxBytes refuses a larger report before reading it, so a file that
	// is not one of the overseer's reports cannot be read into memory to be
	// rejected. An hourly report runs a few kilobytes.
	reportsMaxBytes = 64 << 10

	// reportsKept is how many report timestamps the pane's list holds. The town
	// keeps every report; the pane shows the newest day of them.
	reportsKept = 24

	// reportsOverdue is how long after its own timestamp the latest report
	// reads as late. The overseer reports hourly, so this leaves half an
	// interval of slack on either side of a run that has not happened yet.
	reportsOverdue = 90 * time.Minute
)

// Reports states. Fresh is a latest report inside the overdue window, overdue
// one past it, missing is a directory or a report that is not there, and error
// is one the reader could not use, named in Reports.Note.
const (
	ReportsFresh   = "fresh"
	ReportsOverdue = "overdue"
	ReportsMissing = "missing"
	ReportsError   = "error"
)

// The sentences the panel's header carries for the states that are not the
// overseer's own report. An overdue latest is not one of them: the pane names
// it in its own words, so the reader leaves Note empty.
const (
	reportsNoOverseer = "no overseer reports on this machine"
	reportsNoneYet    = "the overseer has written no report yet"
	reportsBadDir     = "the reports directory could not be read"
	reportsBadReport  = "the latest report could not be read"
)

// reportsOversize names the size cap the way the panel shows it. The number is
// read out of the cap itself, so the sentence and the reader that enforces it
// cannot come apart.
var reportsOversize = fmt.Sprintf("the report is larger than %d KiB", reportsMaxBytes>>10)

var errReportsOversize = errors.New("reports: report over the size cap")

// errReportsNotFile is an entry the reader refuses to read as a report: a
// symlink or anything else that is not a regular file.
var errReportsNotFile = errors.New("reports: not a regular file")

// errReportsUnlisted is a report name the reader does not list: a name of the
// right shape for an instant the pane is not offering.
var errReportsUnlisted = errors.New("reports: report is not one the reader lists")

var reportsNameRe = regexp.MustCompile(reportsNameShape)

// Reports is the overseer's latest report as the pane draws it: the report's
// own text, when it was written, and the writes before it.
type Reports struct {
	At    time.Time `json:"at"`
	State string    `json:"state"` // fresh, overdue, missing, error
	// Note names a state the reader could not read past: a missing or
	// unreadable directory, or a report it refused. It is empty while there is
	// a report to show, whose age the pane draws instead.
	Note string `json:"note,omitempty"`
	// Text is the latest report's own text, with every control character but a
	// newline and a tab stripped. The pane writes it with textContent, so a tag
	// in the report is text and never markup.
	Text string `json:"text,omitempty"`
	// Written is when the latest report was written, off its file name.
	Written time.Time `json:"written,omitzero"`
	// Overdue marks a latest report older than reportsOverdue. The pane shows
	// it in the amber it marks every other stale reading with.
	Overdue bool `json:"overdue,omitempty"`
	// Times are the newest writes, newest first, at most reportsKept of them
	// and the latest among them. The pane lists the ones after the first behind
	// a disclosure.
	Times []time.Time `json:"times"`
}

// ReportsReader reads one reports directory.
type ReportsReader struct{ dir string }

// NewReportsReader returns a reader of the overseer's reports in dir. The
// command wiring names the directory (under the town root); a reader reads the
// one it is given.
func NewReportsReader(dir string) *ReportsReader { return &ReportsReader{dir: dir} }

// Read returns the overseer's latest report as it stands at now.
func (r *ReportsReader) Read(now time.Time) *Reports {
	rep := &Reports{At: now, Times: []time.Time{}}
	entries, err := os.ReadDir(r.dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		rep.State, rep.Note = ReportsMissing, reportsNoOverseer
		return rep
	case err != nil:
		rep.State, rep.Note = ReportsError, reportsBadDir
		return rep
	}
	writes := reportsWrites(entries)
	if len(writes) == 0 {
		rep.State, rep.Note = ReportsMissing, reportsNoneYet
		return rep
	}
	if len(writes) > reportsKept {
		writes = writes[:reportsKept]
	}
	rep.Times = make([]time.Time, len(writes))
	for i, w := range writes {
		rep.Times[i] = w.at
	}
	raw, err := readReportsFile(filepath.Join(r.dir, writes[0].name), reportsMaxBytes)
	switch {
	case errors.Is(err, errReportsOversize):
		rep.State, rep.Note = ReportsError, reportsOversize
		return rep
	case err != nil:
		rep.State, rep.Note = ReportsError, reportsBadReport
		return rep
	}
	rep.Written = writes[0].at
	rep.Text = reportsText(string(raw))
	if now.Sub(writes[0].at) > reportsOverdue {
		rep.State, rep.Overdue = ReportsOverdue, true
	} else {
		rep.State = ReportsFresh
	}
	return rep
}

// ReportText is one report as the /api/report route returns it: the same text
// field the pane draws, and nothing else.
type ReportText struct {
	Text string `json:"text"`
}

// reportsNameTime parses the instant a report's name carries. It accepts the
// name's whole shape first — the layout's own parser is lenient about how many
// digits a field has — so a name with a separator or a letter in it is refused
// here rather than becoming a path.
func reportsNameTime(name string) (time.Time, bool) {
	if !reportsNameRe.MatchString(name) {
		return time.Time{}, false
	}
	at, err := time.Parse(reportsNameLayout, name)
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC(), true
}

// ReadNamed returns the text of the report written at at, when at names one of
// the reports the reader lists. The file it opens is rebuilt from the instant —
// never joined from a caller's string — so a query can only reach a report the
// pane already offers, and only a regular file is read.
func (r *ReportsReader) ReadNamed(at time.Time) (string, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return "", err
	}
	writes := reportsWrites(entries)
	if len(writes) > reportsKept {
		writes = writes[:reportsKept]
	}
	listed := false
	for _, w := range writes {
		if w.at.Equal(at) {
			listed = true
			break
		}
	}
	if !listed {
		return "", errReportsUnlisted
	}
	raw, err := readReportsFile(filepath.Join(r.dir, at.UTC().Format(reportsNameLayout)+reportsExt), reportsMaxBytes)
	if err != nil {
		return "", err
	}
	return reportsText(string(raw)), nil
}

// reportWrite is one report file: the instant its name carries, and the name.
type reportWrite struct {
	at   time.Time
	name string
}

// reportsWrites is the directory's report files, newest first: every entry
// whose name is the overseer's timestamp and .md. Everything else there is the
// writer's own business — the latest.md copy is not a report of its own, since
// the timestamped file it copies is already the one shown — so a directory
// holding something else is never an error.
func reportsWrites(entries []os.DirEntry) []reportWrite {
	out := make([]reportWrite, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		stem, ok := strings.CutSuffix(e.Name(), reportsExt)
		if !ok {
			continue
		}
		at, err := time.Parse(reportsNameLayout, stem)
		if err != nil {
			continue
		}
		out = append(out, reportWrite{at: at, name: e.Name()})
	}
	// Newest first. Two reports of one instant are ordered by name, so the list
	// the pane draws does not depend on the order the directory happened to
	// hand back.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.After(out[j].at)
		}
		return out[i].name > out[j].name
	})
	return out
}

// reportsText is the report's text as the page may show it: every control
// character but a newline and a tab is gone — the pane draws the text with
// pre-wrap, so those two are the ones that carry the report's shape, and a
// carriage return would only double the break its newline already makes —
// along with the invisible format characters, which can reorder what the
// operator reads.
func reportsText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// readReportsFile reads path when it is a regular file no larger than max. The
// stat is a Lstat so a symlink named like a report is refused rather than
// followed: the writer's own output is the only thing a report may be.
func readReportsFile(path string, max int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errReportsNotFile
	}
	if fi.Size() > max {
		return nil, errReportsOversize
	}
	return os.ReadFile(path)
}
