// Package birdreports reads the birds' daily reports under a town's runtime
// directory and says which are due.
//
// Where a report lives is internal/reportdir's rule: this package reads the
// directory it resolves and judges the sidecar. `gt birds due` reads them
// here; the daemon's bird_reports patrol (internal/daemon/bird_reports.go) and
// the dashboard's Birds panel and page route
// (internal/dashboard/birdreports.go) ask reportdir for the same directory.
// The patrol's reader can judge one sidecar differently — it reads no `bird`
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
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/reportdir"
)

const (
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
	oversizeNote  = fmt.Sprintf("%s is larger than %d KiB", reportdir.SidecarName, SidecarMaxBytes>>10)
)

// Read returns the reporter's daily report as it stands at now. maxAge is the
// age past which a report is stale; a report exactly maxAge old is fresh. The
// directory it reads is internal/reportdir's answer.
func Read(runtimeDir, reporter string, now time.Time, maxAge time.Duration) Report {
	loc, err := reportdir.Resolve(runtimeDir, reporter)
	if err != nil {
		// A name the resolver refuses is a report it cannot read, never one it
		// reports fresh.
		return Report{Reporter: reporter, Status: Unreadable, Note: err.Error()}
	}
	report := Report{Reporter: reporter}

	// The words a path that cannot be used is named by: the published layout
	// keeps a reporter directory inside the published reports directory, the
	// old one a reports directory inside the reporter's own.
	dirLabel, parentLabel := "reports directory", "reporter directory"
	if loc.Published {
		dirLabel, parentLabel = "reporter directory", "published reports directory"
	}

	// A path that is not there is Missing, as a missing sidecar is. A link, a
	// file where a directory belongs, or a path that cannot be read is
	// Unreadable: only a path the filesystem says is absent is absent.
	for _, dir := range []struct{ path, label string }{
		{filepath.Dir(loc.Dir), parentLabel},
		{loc.Dir, dirLabel},
	} {
		fi, err := os.Lstat(dir.path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			report.Status, report.Note = Missing, "no "+reportdir.SidecarName
			return report
		case err != nil:
			report.Status, report.Note = Unreadable, dir.label+" could not be read"
			return report
		case !fi.IsDir():
			report.Status, report.Note = Unreadable, dir.label+" is not a real directory"
			return report
		}
	}

	raw, err := readSidecar(filepath.Join(loc.Dir, reportdir.SidecarName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		report.Status, report.Note = Missing, "no "+reportdir.SidecarName
		return report
	case errors.Is(err, errOversize):
		report.Status, report.Note = Unreadable, oversizeNote
		return report
	case errors.Is(err, errNotRegular):
		report.Status, report.Note = Unreadable, reportdir.SidecarName+" is not a regular file"
		return report
	case err != nil:
		// A file that is there and cannot be read is unknown, not absent.
		report.Status, report.Note = Unreadable, reportdir.SidecarName+" could not be read"
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
		return time.Time{}, errors.New(reportdir.SidecarName + " is not valid JSON")
	}
	if side.Bird != reporter {
		// The sidecar's own claim, checked against the reporter it is filed
		// under: one reporter's report never discharges another's day.
		return time.Time{}, errors.New(reportdir.SidecarName + " names a different reporter")
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
