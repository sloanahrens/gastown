package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// The Cloud panel: the cloud patrol's latest report. The report comes from
// another account and is the only thing on this page that a process outside the
// town produces, so nothing here trusts it: every string is stripped and capped
// before it reaches the page, a report the reader cannot make sense of is a
// named error rather than a guess, and the reader opens the two files for
// reading and writes nothing.

const (
	cloudReportFile    = "report.json"
	cloudHeartbeatFile = "heartbeat"

	// cloudSchema is the report schema this panel reads. A report that names
	// another version is an error state: its fields are not this panel's to
	// guess at.
	cloudSchema = 1

	// cloudReportMaxBytes refuses a larger report before reading it, so a file
	// that is not one of the patrol's own cannot be read into memory to be
	// rejected. The patrol's own reports run a few kilobytes.
	cloudReportMaxBytes = 256 << 10

	// cloudMaxFindings is how many finding rows the page draws; the rest are
	// counted in More, and in Counts and Total over the whole report.
	cloudMaxFindings = 100

	// cloudOverdue is how long after its heartbeat the patrol reads as late.
	// The patrol runs every 30 minutes (install-patrol.sh,
	// GT_PATROL_INTERVAL_SECS, default 1800), so this leaves slack for a few
	// missed runs before the panel calls it late.
	cloudOverdue = 2 * time.Hour

	// The cap on each string the page shows, in runes. Kind and Project are ids,
	// a Resource is a path, and a Detail is the one prose field.
	cloudKindMax     = 40
	cloudProjectMax  = 40
	cloudResourceMax = 120
	cloudDetailMax   = 240
)

// Cloud states. Fresh and overdue are a report with a heartbeat to date or not,
// missing is a report that is not there yet, and error is one the reader could
// not use, named in Cloud.Note.
const (
	CloudFresh   = "fresh"
	CloudOverdue = "overdue"
	CloudMissing = "missing"
	CloudError   = "error"
)

// The sentences the panel's header carries for the states that are not the
// patrol's own report.
const (
	cloudNoPatrol     = "no cloud patrol on this machine"
	cloudNoReport     = "the patrol has written no report yet"
	cloudBadHeartbeat = "the heartbeat is not a timestamp"
)

// cloudOversize names the size cap the way the panel shows it. The number is
// read out of the cap itself, so the sentence and the reader that enforces it
// cannot come apart.
var cloudOversize = fmt.Sprintf("the report is larger than %d KiB", cloudReportMaxBytes>>10)

// Cloud severities, worst first: the order findings are listed in, and the
// three the panel counts. Any other severity is counted as Other and listed
// after them.
const (
	cloudUrgent = "urgent"
	cloudNormal = "normal"
	cloudLow    = "low"
)

var (
	errCloudOversize  = errors.New("cloud: report over the size cap")
	errCloudHeartbeat = errors.New("cloud: heartbeat is not a timestamp")
)

// cloudReport is the shape of the file the patrol writes. Its RunID and its
// run's start and finish times are in the file too; the panel shows none of
// them, so they are not parsed here.
type cloudReport struct {
	SchemaVersion int            `json:"SchemaVersion"`
	Projects      []string       `json:"Projects"`
	Findings      []cloudFinding `json:"Findings"`
}

// cloudFinding is one finding as the file writes it. ObservedAt stays a string:
// a time one finding cannot parse is a missing observation, not a report the
// whole panel should lose.
type cloudFinding struct {
	Kind       string `json:"Kind"`
	Severity   string `json:"Severity"`
	Project    string `json:"Project"`
	Resource   string `json:"Resource"`
	Detail     string `json:"Detail"`
	ObservedAt string `json:"ObservedAt"`
}

// CloudFinding is one finding as the panel draws it, every string stripped and
// capped.
type CloudFinding struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"` // urgent, normal, low, or the report's own word, lowercased
	Project  string `json:"project,omitempty"`
	Resource string `json:"resource,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// ObservedAt is when the patrol saw it, absent when the report's time did
	// not parse.
	ObservedAt time.Time `json:"observed_at,omitzero"`
}

// CloudCounts is how many findings the report held at each severity, over
// every finding rather than the capped list the page draws. Other counts the
// findings whose severity is none of the three.
type CloudCounts struct {
	Urgent int `json:"urgent"`
	Normal int `json:"normal"`
	Low    int `json:"low"`
	Other  int `json:"other,omitempty"`
}

// Cloud is the patrol's latest report as the panel draws it: when it last ran,
// whether that is overdue, the projects it watches, and its open findings.
type Cloud struct {
	At    time.Time `json:"at"`
	State string    `json:"state"` // fresh, overdue, missing, error
	// Note names the state in the panel's header: "fresh", "overdue", the error
	// that stopped the read, or why there is no report to read.
	Note string `json:"note,omitempty"`
	// NoPatrol marks a reports directory that is not there. The panel says so,
	// and the hub re-reads such a machine at the slow interval rather than the
	// page's, since a patrol appearing is the only news there could be.
	NoPatrol bool `json:"no_patrol,omitempty"`
	// LastRun is the patrol's heartbeat: the moment the run it last finished
	// did finish. A run in flight has no heartbeat of its own yet.
	LastRun *time.Time `json:"last_run,omitempty"`
	// Projects are the cloud projects the patrol watches.
	Projects []string `json:"projects"`
	// Findings is the report's findings, most severe first, at most
	// cloudMaxFindings of them. Total, Counts and More cover the whole report.
	Findings []CloudFinding `json:"findings"`
	Total    int            `json:"total"`
	Counts   CloudCounts    `json:"counts"`
	More     int            `json:"more,omitempty"`
	// Checks is the cloud-check plugin's latest status, read from its own file
	// on the same poll as the report. It is nil when the reader has no checks
	// reader wired, or when the plugin has written no status at all.
	Checks *CloudChecks `json:"checks,omitempty"`
}

// CloudReader reads one reports directory.
type CloudReader struct {
	dir string
	// checks reads the cloud-check plugin's status, when one is wired (see
	// WithChecks). Nil leaves Cloud.Checks nil.
	checks *CloudChecksReader
}

// NewCloudReader returns a reader of the patrol's reports in dir. The command
// wiring names the directory (config.CloudReportsDir); a reader reads the one
// it is given.
func NewCloudReader(dir string) *CloudReader { return &CloudReader{dir: dir} }

// WithChecks attaches the cloud-check status reader, so Read fills the pane's
// checks line from the same poll as the patrol's report. A reader with none
// leaves the line absent.
func (r *CloudReader) WithChecks(c *CloudChecksReader) *CloudReader {
	r.checks = c
	return r
}

// Read returns the patrol's latest report as it stands at now, with the
// cloud-check status the reader was given.
func (r *CloudReader) Read(now time.Time) *Cloud {
	c := r.read(now)
	if r.checks != nil {
		c.Checks = r.checks.Read(now)
	}
	return c
}

// read returns the patrol's latest report alone, leaving the checks line to
// Read so no path out of it forgets to fill it.
func (r *CloudReader) read(now time.Time) *Cloud {
	c := &Cloud{At: now, Projects: []string{}, Findings: []CloudFinding{}}
	if fi, err := os.Stat(r.dir); err != nil || !fi.IsDir() {
		c.State, c.Note, c.NoPatrol = CloudMissing, cloudNoPatrol, true
		return c
	}
	raw, err := readCloudFile(filepath.Join(r.dir, cloudReportFile), cloudReportMaxBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		c.State, c.Note = CloudMissing, cloudNoReport
		return c
	case errors.Is(err, errCloudOversize):
		c.State, c.Note = CloudError, cloudOversize
		return c
	case err != nil:
		c.State, c.Note = CloudError, "the report could not be read"
		return c
	}
	var rep cloudReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		c.State, c.Note = CloudError, "the report is not valid JSON"
		return c
	}
	if rep.SchemaVersion != cloudSchema {
		c.State, c.Note = CloudError, fmt.Sprintf("unknown schema version %d", rep.SchemaVersion)
		return c
	}
	c.Projects = cloudProjects(rep.Projects)
	c.addFindings(rep.Findings)
	run, err := r.readHeartbeat()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		c.State, c.Note = CloudError, "the patrol's heartbeat is missing"
		return c
	case errors.Is(err, errCloudHeartbeat):
		c.State, c.Note = CloudError, cloudBadHeartbeat
		return c
	case err != nil:
		c.State, c.Note = CloudError, "the heartbeat could not be read"
		return c
	}
	c.LastRun = &run
	if now.Sub(run) > cloudOverdue {
		c.State, c.Note = CloudOverdue, "overdue"
	} else {
		c.State, c.Note = CloudFresh, "fresh"
	}
	return c
}

// addFindings fills the panel's list and counts from the report's: each string
// stripped and capped, the rows most severe first, and the list kept to
// cloudMaxFindings with the rest counted in More. The counts and the total
// cover the whole report, so a capped list still says how much there was.
func (c *Cloud) addFindings(in []cloudFinding) {
	rows := make([]CloudFinding, 0, len(in))
	var counts CloudCounts
	for _, f := range in {
		row := CloudFinding{
			Kind:     cloudText(f.Kind, cloudKindMax),
			Severity: strings.ToLower(cloudText(f.Severity, cloudKindMax)),
			Project:  cloudText(f.Project, cloudProjectMax),
			Resource: cloudText(f.Resource, cloudResourceMax),
			Detail:   cloudText(f.Detail, cloudDetailMax),
		}
		if at, err := time.Parse(time.RFC3339, strings.TrimSpace(f.ObservedAt)); err == nil {
			row.ObservedAt = at
		}
		switch row.Severity {
		case cloudUrgent:
			counts.Urgent++
		case cloudNormal:
			counts.Normal++
		case cloudLow:
			counts.Low++
		default:
			counts.Other++
		}
		rows = append(rows, row)
	}
	// Stable, so findings of one severity keep the report's own order.
	sort.SliceStable(rows, func(i, j int) bool { return cloudRank(rows[i].Severity) < cloudRank(rows[j].Severity) })
	c.Total = len(rows)
	if len(rows) > cloudMaxFindings {
		c.More = len(rows) - cloudMaxFindings
		rows = rows[:cloudMaxFindings]
	}
	c.Findings, c.Counts = rows, counts
}

// cloudRank orders severities the way the panel lists them: the three the
// patrol writes first, and anything else after them.
func cloudRank(severity string) int {
	switch severity {
	case cloudUrgent:
		return 0
	case cloudNormal:
		return 1
	case cloudLow:
		return 2
	}
	return 3
}

// cloudProjects strips and caps the report's project ids, dropping the ones
// that are blank once stripped.
func cloudProjects(in []string) []string {
	out := []string{}
	for _, p := range in {
		if p = cloudText(p, cloudProjectMax); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// cloudText is the report's text as the page may show it: the characters that
// carry no visible glyph are gone (an invisible format character can reorder
// what the operator reads, and a line break would break the one-line cell it
// sits in) and the string is kept to max runes, marked past the cut. The report
// is another account's file, so nothing here trusts its length or its bytes.
func cloudText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	s, _ = ClipText(strings.TrimSpace(s), max)
	return s
}

// readCloudFile reads path, refusing a file larger than max before it is read.
func readCloudFile(path string, max int64) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > max {
		return nil, errCloudOversize
	}
	return os.ReadFile(path)
}

// readHeartbeat reads the patrol's heartbeat: the RFC3339 timestamp it writes
// beside the report each time it finishes a run.
func (r *CloudReader) readHeartbeat() (time.Time, error) {
	b, err := os.ReadFile(filepath.Join(r.dir, cloudHeartbeatFile))
	if err != nil {
		return time.Time{}, err
	}
	line := strings.TrimSpace(string(b))
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	t, err := time.Parse(time.RFC3339, line)
	if err != nil {
		return time.Time{}, errCloudHeartbeat
	}
	return t, nil
}
