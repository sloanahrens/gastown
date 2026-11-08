// Package birdreports reads the birds' daily reports under a town's runtime
// directory and says which are due.
//
// A bird publishes <runtimeDir>/reports/<bird>/daily.json with `gt report
// publish`, the daemon-owned location for its report. Read falls back to
// <runtimeDir>/<bird>/reports/daily.json, where the birds wrote before the
// command existed, so a town is readable while its birds switch. `gt birds due`
// reads them here. The daemon's bird_reports patrol
// (internal/daemon/bird_reports.go) and the dashboard's Birds panel
// (internal/dashboard/birdreports.go) read the old path with their own readers;
// the patrol's reader can judge one sidecar differently — it reads no `bird`
// field, applies no size cap, and calls any future written_at late, where Read
// forgives one within FutureSlack.
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

// dirCheck is one directory on the way to a sidecar, and the words Read uses
// when it is not the directory it should be.
type dirCheck struct {
	path  string
	label string
}

// reportLocation is one layout a report may be in: the directories Read
// checks, then the sidecar below them.
type reportLocation struct {
	dirs    []dirCheck
	sidecar string
}

// reportLocations lists where a reporter's report may be, in the order Read
// tries them: the location gt report publish writes, then the reporter's own
// tree, which the birds wrote before the command existed.
func reportLocations(runtimeDir, reporter string) []reportLocation {
	published := PublishedReportDir(runtimeDir, reporter)
	legacy := legacyReportDir(runtimeDir, reporter)
	return []reportLocation{
		{
			dirs: []dirCheck{
				{filepath.Join(runtimeDir, PublishedDirName), "published reports directory"},
				{published, "reporter directory"},
			},
			sidecar: filepath.Join(published, SidecarName),
		},
		{
			dirs: []dirCheck{
				{filepath.Join(runtimeDir, reporter), "reporter directory"},
				{legacy, "reports directory"},
			},
			sidecar: filepath.Join(legacy, SidecarName),
		},
	}
}

// Read returns the reporter's daily report as it stands at now. maxAge is the
// age past which a report is stale; a report exactly maxAge old is fresh. The
// published location wins when it holds a report; a location that holds none
// falls through to the next, so a bird that has not switched keeps its status.
//
// It follows no symlink below the reporter name: every directory on the way to
// the sidecar must be the thing it names, as the dashboard's reader also
// requires, so a link cannot put another path's report in this reporter's
// place.
func Read(runtimeDir, reporter string, now time.Time, maxAge time.Duration) Report {
	if !reporterNameRe.MatchString(reporter) {
		return Report{Reporter: reporter, Status: Unreadable, Note: "not a reporter name"}
	}
	for _, loc := range reportLocations(runtimeDir, reporter) {
		report := readLocation(loc, reporter, now, maxAge)
		if report.Status != Missing {
			return report
		}
	}
	return Report{Reporter: reporter, Status: Missing, Note: "no " + SidecarName}
}

// readLocation reads one layout's report. Missing means this layout holds no
// report at all, so the caller may try the next; every other status is this
// layout's answer, and an unreadable one is never read as absent.
func readLocation(loc reportLocation, reporter string, now time.Time, maxAge time.Duration) Report {
	report := Report{Reporter: reporter}

	// A path that is not there is Missing, as a missing sidecar is. A link, a
	// file where a directory belongs, or a path that cannot be read is
	// Unreadable: only a path the filesystem says is absent is absent.
	for _, dir := range loc.dirs {
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

	raw, err := readSidecar(loc.sidecar)
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
