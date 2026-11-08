// Package reportdir resolves where a bird's daily report lives under a town's
// runtime directory.
//
// The rule, written here once: `gt report publish` writes
// <runtimeDir>/reports/<bird>/daily.html and daily.json; a bird that wrote its
// report before the command existed left <runtimeDir>/<bird>/reports/daily.json.
// A reader asks this package for the directory and gets the published one when
// it holds a regular daily.json or daily.html, else the old one. No reader
// builds the path itself, and a topic page list is both locations' pages
// unioned by name with the published page winning.
//
// A report name is lowercase letters, 2 to 24, which is every name the bird
// pool hands out, and it is checked before a path is built from it.
package reportdir

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const (
	// PublishedDirName is the daemon-owned directory directly under the
	// runtime directory that holds one directory per reporter that has
	// published.
	PublishedDirName = "reports"

	// ReportsSubdir is the directory a bird kept its reports in before gt
	// report publish existed: <runtimeDir>/<bird>/reports.
	ReportsSubdir = "reports"

	// SidecarName is the summary a report carries, beside its page.
	SidecarName = "daily.json"

	// PageName and PrevPageName are the report page and the page before it,
	// beside SidecarName. PrevSidecarName is the sidecar before the current
	// one, kept beside them.
	PageName        = "daily.html"
	PrevPageName    = "daily.prev.html"
	PrevSidecarName = "daily.prev.json"
)

// The refusals a caller can tell apart. Each is a fixed sentence with no part
// of the value in it: a name or a page can come from a request.
var (
	// ErrBadName is a reporter name that is not lowercase letters, 2 to 24.
	ErrBadName = errors.New("not a reporter name")
	// ErrBadPage is a file name a bird does not write in its report directory.
	ErrBadPage = errors.New("not a page a bird writes")
)

// nameShape is the whole name a reporter may carry: a lowercase letter, then
// lowercase letters, digits and hyphens, 32 at most. It is the shape the
// readers accepted before this package existed, so no existing reporter loses
// its report; it holds no slash, dot or uppercase letter, so a name cannot step
// out of the runtime directory.
const nameShape = `^[a-z][a-z0-9-]{0,31}$`

// topicPageShape is the name of a topic page a bird writes beside its daily
// one: a slug and .html, no dots in the slug, so daily.json, a dot segment and
// another extension never match. daily.html fits the shape and is the daily
// page, listed apart from the topic pages.
const topicPageShape = `^[a-z0-9][a-z0-9-]{0,60}\.html$`

var (
	nameRe      = regexp.MustCompile(nameShape)
	topicPageRe = regexp.MustCompile(topicPageShape)
)

// ValidName reports whether reporter is the whole name a report may be filed
// under. It is checked before any path is built.
func ValidName(reporter string) bool { return nameRe.MatchString(reporter) }

// IsTopicPage reports whether file is a topic page name a bird writes: a slug
// and .html, other than the daily page itself.
func IsTopicPage(file string) bool { return topicPageRe.MatchString(file) && file != PageName }

// Location is the directory one reporter's report is read from.
type Location struct {
	// Dir is the directory holding the report.
	Dir string
	// Published is true when Dir is the published location rather than the
	// old one.
	Published bool
}

// Resolve returns where reporter's report is read from: the published
// directory when it holds a regular daily.json or daily.html, else the old
// one. No symlink is followed: a linked directory or a linked daily file holds
// nothing, so the report falls back and a link cannot put another path's
// report in this reporter's place.
func Resolve(runtimeDir, reporter string) (Location, error) {
	if !ValidName(reporter) {
		return Location{}, ErrBadName
	}
	return resolveValid(runtimeDir, reporter), nil
}

// resolveValid is Resolve's answer for a name ValidName has accepted.
func resolveValid(runtimeDir, reporter string) Location {
	published := publishedPath(runtimeDir, reporter)
	if publishedHoldsReport(published) {
		return Location{Dir: published, Published: true}
	}
	return Location{Dir: legacyPath(runtimeDir, reporter)}
}

// Dir returns the directory holding reporter's daily report, as Resolve does.
func Dir(runtimeDir, reporter string) (string, error) {
	loc, err := Resolve(runtimeDir, reporter)
	return loc.Dir, err
}

// Published returns the directory gt report publish writes for reporter,
// <runtimeDir>/reports/<reporter>, whether or not it exists yet.
func Published(runtimeDir, reporter string) (string, error) {
	if !ValidName(reporter) {
		return "", ErrBadName
	}
	return publishedPath(runtimeDir, reporter), nil
}

// publishedPath returns the published directory for reporter. It is called only
// after ValidName has accepted the name.
func publishedPath(runtimeDir, reporter string) string {
	return filepath.Join(runtimeDir, PublishedDirName, reporter)
}

// PublishedReportsDir returns the daemon-owned directory holding one
// directory per reporter that has published, <runtimeDir>/reports. It is not
// itself a reporter.
func PublishedReportsDir(runtimeDir string) string {
	return filepath.Join(runtimeDir, PublishedDirName)
}

// legacyPath returns the directory a bird wrote before gt report publish,
// <runtimeDir>/<reporter>/reports.
func legacyPath(runtimeDir, reporter string) string {
	return filepath.Join(runtimeDir, reporter, ReportsSubdir)
}

// publishedHoldsReport reports whether dir is a real directory holding a
// regular daily.json, the sidecar every reader loads. A directory with only a
// daily.html (a publish interrupted between its two renames) is not a report:
// the readers would find no sidecar there and report Missing while a legacy
// daily.json exists, so it falls through to the legacy location. A link at the
// directory or at the file is not a report this reader will follow.
func publishedHoldsReport(dir string) bool {
	if !isRealDir(filepath.Dir(dir)) || !isRealDir(dir) {
		return false
	}
	fi, err := os.Lstat(filepath.Join(dir, SidecarName))
	return err == nil && fi.Mode().IsRegular()
}

// isRealDir reports whether path is a directory in its own right. The Lstat is
// what refuses a symlink: a linked directory is not the directory it names.
func isRealDir(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir()
}

// Page is one topic page of a reporter: its file name, when it was last
// written, and the directory it was found in, so a caller that reads its text
// opens the page the merge chose.
type Page struct {
	Name  string
	MTime time.Time
	Dir   string
}

// TopicPages returns the pages reporter wrote beside its daily page, in both
// locations: one entry per page name, the published location's page winning a
// name collision, so a legacy topic page survives the first publish. Each is a
// regular file directly inside a real directory, not the daily page, under
// maxBytes. Newest first, the name breaking a tie. A directory that is missing,
// is not a real directory, or cannot be listed holds no pages.
func TopicPages(runtimeDir, reporter string, maxBytes int64) ([]Page, error) {
	if !ValidName(reporter) {
		return nil, ErrBadName
	}
	merged := map[string]Page{}
	for _, dir := range []string{publishedPath(runtimeDir, reporter), legacyPath(runtimeDir, reporter)} {
		for _, page := range topicPagesIn(dir, maxBytes) {
			if _, taken := merged[page.Name]; !taken {
				merged[page.Name] = page
			}
		}
	}
	out := make([]Page, 0, len(merged))
	for _, page := range merged {
		out = append(out, page)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].MTime.Equal(out[j].MTime) {
			return out[i].MTime.After(out[j].MTime)
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// topicPagesIn lists the topic pages directly inside dir.
func topicPagesIn(dir string, maxBytes int64) []Page {
	if !isRealDir(dir) {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Page
	for _, entry := range entries {
		name := entry.Name()
		if !IsTopicPage(name) {
			continue
		}
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() >= maxBytes {
			continue
		}
		out = append(out, Page{Name: name, MTime: fi.ModTime().UTC(), Dir: dir})
	}
	return out
}

// PagePath returns the path of one page of reporter: daily.html and
// daily.prev.html are in the directory Dir resolves, and a topic page is
// looked for in the published location first and the old one second, so a
// legacy topic page is still served after the bird publishes. A name or page
// that is not one a bird writes is refused before the path is built.
func PagePath(runtimeDir, reporter, file string) (string, error) {
	if !ValidName(reporter) {
		return "", ErrBadName
	}
	switch file {
	case PageName, PrevPageName:
		return filepath.Join(resolveValid(runtimeDir, reporter).Dir, file), nil
	}
	if !IsTopicPage(file) {
		return "", ErrBadPage
	}
	published := publishedPath(runtimeDir, reporter)
	if fi, err := os.Lstat(filepath.Join(published, file)); err == nil && fi.Mode().IsRegular() {
		return filepath.Join(published, file), nil
	}
	return filepath.Join(legacyPath(runtimeDir, reporter), file), nil
}
