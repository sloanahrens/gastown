// Package cloudcheck runs the network half of the cloud-check plugin: it reads
// the apps list, fetches each app's URL, and reports one level per check as a
// text report and as the JSON status file the dashboard reads.
package cloudcheck

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
)

// Level is one check's verdict.
type Level string

const (
	// LevelOK is a 2xx answer.
	LevelOK Level = "ok"
	// LevelWarn is an answer that is neither ok nor a failure: a 3xx or 4xx
	// response, or a malformed line in the apps file. A warning never changes
	// the run's exit status.
	LevelWarn Level = "warn"
	// LevelFail is a 5xx answer, a refused or timed-out connection, an
	// unreadable off marker, or an apps file that names no apps.
	LevelFail Level = "fail"
	// LevelOff is an app an .off marker stands down on purpose: it is not
	// fetched and does not change the exit status.
	LevelOff Level = "off"
)

// App is one check from the apps file.
type App struct {
	// Section is the app= name that preceded it, empty when none did.
	Section string
	// Name is the left side of its name=url line.
	Name string
	// URL is the right side, an absolute http or https URL.
	URL string
}

// Problem is an apps file defect the run reports rather than guessing at.
type Problem struct {
	// Level is LevelWarn for a malformed line and LevelFail for a file that
	// names no apps.
	Level Level
	// Line is the 1-based line, or 0 for the whole file.
	Line int
	// Reason says what is wrong without echoing the line: a URL there may
	// carry a credential.
	Reason string
}

// detail is the problem as one phrase, for a row or an item.
func (p Problem) detail() string {
	if p.Line == 0 {
		return p.Reason
	}
	return fmt.Sprintf("line %d: %s", p.Line, p.Reason)
}

// Item is one warning or failure as the status file carries it.
type Item struct {
	Level   Level  `json:"level"`
	Section string `json:"section,omitempty"`
	Text    string `json:"text"`
}

// Row is one check as the text report shows it.
type Row struct {
	Level   Level
	Section string
	Name    string
	Detail  string
}

// Report is one run: its time, the counts over the whole run, and the warnings
// and failures as the status file carries them. At is in unix seconds and the
// at, ok, warnings, failed and items names are the ones the dashboard's checks
// reader reads; off is this command's own count of the apps a marker stands
// down, and a reader that does not know it ignores it.
type Report struct {
	At       int64  `json:"at"`
	OK       int    `json:"ok"`
	Warnings int    `json:"warnings"`
	Failed   int    `json:"failed"`
	Off      int    `json:"off,omitempty"`
	Items    []Item `json:"items"`
	// Rows is every check in file order for the text report. The status file
	// carries only the warnings and failures.
	Rows []Row `json:"-"`
}

// Print writes one line per check in file order, then the counts. With quiet
// it writes only the warnings and failures and no counts.
func (r Report) Print(w io.Writer, quiet bool) {
	for _, row := range r.Rows {
		if quiet && row.Level != LevelWarn && row.Level != LevelFail {
			continue
		}
		name := row.Name
		if row.Section != "" {
			name = row.Section + "/" + row.Name
		}
		fmt.Fprintf(w, "%-4s %-28s %s\n", row.Level, name, row.Detail)
	}
	if !quiet {
		fmt.Fprintf(w, "%d ok, %d warnings, %d failed, %d off\n", r.OK, r.Warnings, r.Failed, r.Off)
	}
}

// record adds row to the report, counting its level and keeping the warnings
// and failures as items.
func (r *Report) record(row Row) {
	switch row.Level {
	case LevelOK, LevelWarn, LevelFail, LevelOff:
	default:
		// A level this report does not know must not count as a pass, and the
		// row keeps the name that produced it.
		known := row.Level
		row.Level = LevelWarn
		row.Detail = strings.TrimSpace(fmt.Sprintf("unknown level '%s': %s", known, row.Detail))
	}
	row.Section = clean(row.Section, sectionMax)
	row.Name = clean(row.Name, nameMax)
	row.Detail = clean(row.Detail, detailMax)
	r.Rows = append(r.Rows, row)
	switch row.Level {
	case LevelFail:
		r.Failed++
	case LevelWarn:
		r.Warnings++
	case LevelOff:
		r.Off++
	default:
		r.OK++
	}
	if row.Level == LevelWarn || row.Level == LevelFail {
		text := row.Detail
		if row.Name != "" {
			text = row.Name + ": " + row.Detail
		}
		r.Items = append(r.Items, Item{Level: row.Level, Section: row.Section, Text: clean(text, detailMax)})
	}
}

// FailureReport is the status of a run that could not be made: one failure
// whose detail is reason, so a status file written by a broken run never keeps
// the previous, healthy one. reason must name the defect without echoing an
// apps-file line, which may carry a credential.
func FailureReport(reason string) Report {
	r := Report{At: time.Now().Unix(), Items: []Item{}}
	r.record(Row{Level: LevelFail, Name: "apps file", Detail: reason})
	return r
}

// The caps on a reported value in runes. The dashboard's checks reader caps
// the same fields, so a longer value would be cut there anyway.
const (
	sectionMax = 60
	nameMax    = 60
	detailMax  = 200
)

// clean returns s with control characters removed and cut to max runes, so a
// name from the apps file or an error from the network cannot drive the
// terminal or outgrow the status file.
func clean(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == max {
			break
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
