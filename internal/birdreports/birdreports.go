// Package birdreports reads the birds' daily reports under a town's runtime
// directory and says which are due.
//
// A bird publishes <runtimeDir>/reports/<bird>/daily.json with `gt report
// publish`, the daemon-owned location for its report. ReportDir is the one
// resolver of where a reporter's report lives: the published directory when it
// holds a regular daily.json or daily.html, else the old
// <runtimeDir>/<bird>/reports/daily.json the birds wrote before the command
// existed, so a town is readable while its birds switch. `gt birds due` reads
// them here; the daemon's bird_reports patrol
// (internal/daemon/bird_reports.go) and the dashboard's Birds panel and page
// route (internal/dashboard/birdreports.go) call ReportDir rather than build
// the path themselves. The patrol's reader can judge one sidecar differently —
// it reads no `bird` field, applies no size cap, and calls any future
// written_at late, where Read forgives one within FutureSlack.
package birdreports

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const (
	// ReportsSubdir and SidecarName are where a bird wrote its report before
	// gt report publish: <runtimeDir>/<bird>/reports/daily.json.
	ReportsSubdir = "reports"
	SidecarName   = "daily.json"

	// PublishedDirName is the daemon-owned directory, directly under the
	// runtime directory, that gt report publish writes:
	// <runtimeDir>/reports/<bird>/daily.html and daily.json.
	PublishedDirName = "reports"

	// PageName and PrevPageName are the report page and the page before it,
	// beside SidecarName in the resolved report directory. PrevSidecarName is
	// the sidecar before the current one, kept beside them.
	PageName        = "daily.html"
	PrevPageName    = "daily.prev.html"
	PrevSidecarName = "daily.prev.json"

	// SidecarMaxBytes refuses a larger daily.json before it is read: a sidecar
	// is a short summary, and anything bigger is not one.
	SidecarMaxBytes = 16 << 10

	// FutureSlack is how far written_at may lead the reader's clock before the
	// sidecar is refused. Bird and reader share a machine, so a sidecar minutes
	// ahead is two clocks, not a forgery; further ahead is not a written time.
	FutureSlack = 5 * time.Minute

	// MaxAge is how old a report may be before it is stale. Reports are daily,
	// so a report more than a day and two hours old is one the bird did not
	// rewrite.
	MaxAge = 26 * time.Hour
)

// reporterNameShape is the name a reporter directory may carry, as a whole. The
// shape is checked before a path is built, so a name that came from a config
// file and holds a separator or a dot never reaches the filesystem.
const reporterNameShape = `^[a-z][a-z0-9-]{0,31}$`

var reporterNameRe = regexp.MustCompile(reporterNameShape)

// defaultReporters is the nine birds and the overseer, the reporters every
// daily report is expected from.
var defaultReporters = []string{
	"dunlin", "dipper", "eagle", "goose", "peacock",
	"egret", "seagull", "owl", "merlin", "overseer",
}

// DefaultReporters returns the reporters every daily report is expected from,
// as a new slice the caller owns.
func DefaultReporters() []string {
	return append([]string(nil), defaultReporters...)
}

// Status is what a reporter's daily report is, at the clock it was read.
type Status string

const (
	// Fresh is a report inside MaxAge.
	Fresh Status = "fresh"
	// Stale is a report past MaxAge.
	Stale Status = "stale"
	// Missing is a reporter with no daily.json.
	Missing Status = "missing"
	// Unreadable is a daily.json the reader refused or could not use. It is
	// never Fresh: an unknown report cannot discharge the day's obligation.
	Unreadable Status = "unreadable"
)

// Report is one reporter's daily report at a clock.
type Report struct {
	// Reporter is the directory name the report was read from.
	Reporter string
	// Status is Fresh, Stale, Missing or Unreadable.
	Status Status
	// Written is the sidecar's written_at, UTC. It is zero unless Status is
	// Fresh or Stale.
	Written time.Time
	// Age is now minus Written when Status is Fresh or Stale, else zero.
	Age time.Duration
	// Note is why the sidecar is Unreadable, or empty. It names the defect in
	// fixed words and never carries file contents.
	Note string
}

// Due reports whether the report is late: anything but a fresh one is.
func (r Report) Due() bool { return r.Status != Fresh }

// The reasons a sidecar is refused, in the words the command prints. Each is a
// fixed sentence, so no file content can reach a log through a note. The
// oversize sentence is built from the cap, so the two cannot come apart.
var (
	errNotRegular = errors.New("not a regular file")
	errOversize   = errors.New("larger than the sidecar cap")
	oversizeNote  = fmt.Sprintf("%s is larger than %d KiB", SidecarName, SidecarMaxBytes>>10)
)

// PublishedReportDir returns the directory a published report lives in,
// <runtimeDir>/reports/<reporter>, holding daily.html and its sidecar.
func PublishedReportDir(runtimeDir, reporter string) string {
	return filepath.Join(runtimeDir, PublishedDirName, reporter)
}

// legacyReportDir returns the directory a bird wrote before gt report publish,
// <runtimeDir>/<reporter>/reports.
func legacyReportDir(runtimeDir, reporter string) string {
	return filepath.Join(runtimeDir, reporter, ReportsSubdir)
}

// reportLocation is where a reporter's report is: the directory, and the words
// Read uses when that directory, or the one above it, is not what it should be.
type reportLocation struct {
	dir         string
	dirLabel    string
	parentLabel string
}

// ReportDir returns the directory holding reporter's daily report: the
// published directory <runtimeDir>/reports/<reporter> when it holds a regular
// daily.json or daily.html, else the reporter's own
// <runtimeDir>/<reporter>/reports. Every reader resolves the path here rather
// than building it. No symlink is followed: a linked directory or a linked
// daily file holds nothing, so the report falls back to the old location and a
// link cannot put another path's report in this reporter's place.
func ReportDir(runtimeDir, reporter string) string {
	return reportLocationFor(runtimeDir, reporter).dir
}

// reportLocationFor chooses the one layout a reporter's report is read from.
// The published directory wins when it holds a regular daily.json or daily.html;
// anything else falls back to the old location, so a bird that has not switched
// keeps its status.
func reportLocationFor(runtimeDir, reporter string) reportLocation {
	if publishedHoldsReport(runtimeDir, reporter) {
		return reportLocation{
			dir:         PublishedReportDir(runtimeDir, reporter),
			dirLabel:    "reporter directory",
			parentLabel: "published reports directory",
		}
	}
	return reportLocation{
		dir:         legacyReportDir(runtimeDir, reporter),
		dirLabel:    "reports directory",
		parentLabel: "reporter directory",
	}
}

// publishedHoldsReport reports whether the published directory is a real
// directory holding a regular daily.json or daily.html. A link at the directory
// or at either file is not a report this reader will follow.
func publishedHoldsReport(runtimeDir, reporter string) bool {
	if !isRealDir(filepath.Join(runtimeDir, PublishedDirName)) {
		return false
	}
	dir := PublishedReportDir(runtimeDir, reporter)
	if !isRealDir(dir) {
		return false
	}
	for _, name := range []string{SidecarName, PageName} {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// isRealDir reports whether path is a directory in its own right. The Lstat is
// what refuses a symlink: a linked directory is not the directory it names.
func isRealDir(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir()
}

// Read returns the reporter's daily report as it stands at now. maxAge is the
// age past which a report is stale; a report exactly maxAge old is fresh. It
// reads the directory ReportDir resolves, so a report published with
// `gt report publish` wins and a bird that has not switched keeps its status.
func Read(runtimeDir, reporter string, now time.Time, maxAge time.Duration) Report {
	if !reporterNameRe.MatchString(reporter) {
		return Report{Reporter: reporter, Status: Unreadable, Note: "not a reporter name"}
	}
	loc := reportLocationFor(runtimeDir, reporter)
	report := Report{Reporter: reporter}

	// A path that is not there is Missing, as a missing sidecar is. A link, a
	// file where a directory belongs, or a path that cannot be read is
	// Unreadable: only a path the filesystem says is absent is absent.
	for _, dir := range []struct{ path, label string }{
		{filepath.Dir(loc.dir), loc.parentLabel},
		{loc.dir, loc.dirLabel},
	} {
		fi, err := os.Lstat(dir.path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			report.Status, report.Note = Missing, "no "+SidecarName
			return report
		case err != nil:
			report.Status, report.Note = Unreadable, dir.label+" could not be read"
			return report
		case !fi.IsDir():
			report.Status, report.Note = Unreadable, dir.label+" is not a real directory"
			return report
		}
	}

	raw, err := readSidecar(filepath.Join(loc.dir, SidecarName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		report.Status, report.Note = Missing, "no "+SidecarName
		return report
	case errors.Is(err, errOversize):
		report.Status, report.Note = Unreadable, oversizeNote
		return report
	case errors.Is(err, errNotRegular):
		report.Status, report.Note = Unreadable, SidecarName+" is not a regular file"
		return report
	case err != nil:
		// A file that is there and cannot be read is unknown, not absent.
		report.Status, report.Note = Unreadable, SidecarName+" could not be read"
		return report
	}

	written, err := ParseSidecar(reporter, raw, now)
	if err != nil {
		report.Status, report.Note = Unreadable, err.Error()
		return report
	}

	report.Written = written
	report.Age = now.Sub(written)
	if report.Age > maxAge {
		report.Status = Stale
	} else {
		report.Status = Fresh
	}
	return report
}

// ParseSidecar returns the instant raw names, checking it as Read does: valid
// JSON, the reporter's own bird, an RFC 3339 written_at no more than FutureSlack
// ahead of now. Each refusal repeats the sentence Read puts in a report's Note,
// so a publisher cannot write a sidecar the reader would then refuse.
func ParseSidecar(reporter string, raw []byte, now time.Time) (time.Time, error) {
	var side struct {
		Bird      string `json:"bird"`
		WrittenAt string `json:"written_at"`
	}
	if err := json.Unmarshal(raw, &side); err != nil {
		return time.Time{}, errors.New(SidecarName + " is not valid JSON")
	}
	if side.Bird != reporter {
		// The sidecar's own claim, checked against the reporter it is filed
		// under: one reporter's report never discharges another's day.
		return time.Time{}, errors.New(SidecarName + " names a different reporter")
	}
	written, err := time.Parse(time.RFC3339, side.WrittenAt)
	if err != nil {
		return time.Time{}, errors.New("written_at is not an RFC 3339 instant")
	}
	written = written.UTC()
	if written.Sub(now) > FutureSlack {
		return time.Time{}, errors.New("written_at is in the future")
	}
	return written, nil
}

// ReadAll returns one report per reporter, in name order, so the same town
// state always prints the same list.
func ReadAll(runtimeDir string, reporters []string, now time.Time, maxAge time.Duration) []Report {
	reports := make([]Report, 0, len(reporters))
	for _, reporter := range reporters {
		reports = append(reports, Read(runtimeDir, reporter, now, maxAge))
	}
	sort.SliceStable(reports, func(i, j int) bool { return reports[i].Reporter < reports[j].Reporter })
	return reports
}

// readSidecar reads the sidecar at path, refusing anything that is not a
// regular file of at most SidecarMaxBytes. The Lstat both bounds the read and
// refuses a symlink named daily.json, as the panel's reader does.
func readSidecar(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	if fi.Size() > SidecarMaxBytes {
		return nil, errOversize
	}
	return os.ReadFile(path)
}
