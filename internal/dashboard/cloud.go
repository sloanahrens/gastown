package dashboard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/cloudledger"
	"github.com/steveyegge/gastown/internal/config"
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
	cloudReportMaxBytes = cloudledger.ReportMaxBytes

	// cloudHeartbeatMaxBytes refuses a larger heartbeat before reading it: the
	// file is one RFC3339 timestamp, so anything longer is not one.
	cloudHeartbeatMaxBytes = cloudledger.HeartbeatMaxBytes

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

// The errors the cloud line names. They are the source's own sentinels, so a
// state here and a state the daemon reports cannot come apart.
var (
	errCloudOversize  = cloudledger.ErrOversize
	errCloudHeartbeat = cloudledger.ErrBadHeartbeat
)

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
	// Apps is the app power line: each app's on or off state and the release
	// bot's latest ledger line. It is nil when the reader has no apps reader
	// wired, or when neither of the apps reader's directories is there.
	Apps *AppPower `json:"apps,omitempty"`
}

// CloudReader reads the patrol's report and heartbeat from one source.
type CloudReader struct {
	source cloudledger.Source
	// checks reads the cloud-check plugin's status, when one is wired (see
	// WithChecks). Nil leaves Cloud.Checks nil.
	checks *CloudChecksReader
	// apps reads the app power line, when one is wired (see WithApps). Nil
	// leaves Cloud.Apps nil.
	apps *AppPowerReader
}

// NewCloudReader returns a reader of the patrol's reports in the file directory
// dir. It is the file-only reader; the command wiring builds its reader from the
// source the town settings select, with NewCloudReaderFromSource.
func NewCloudReader(dir string) *CloudReader {
	return NewCloudReaderFromSource(cloudledger.NewFileSource(dir, ""))
}

// NewCloudReaderFromSource returns a reader of the source's report and
// heartbeat, which the town settings select.
func NewCloudReaderFromSource(s cloudledger.Source) *CloudReader {
	return &CloudReader{source: s}
}

// WithChecks attaches the cloud-check status reader, so Read fills the pane's
// checks line from the same poll as the patrol's report. A reader with none
// leaves the line absent.
func (r *CloudReader) WithChecks(c *CloudChecksReader) *CloudReader {
	r.checks = c
	return r
}

// WithApps attaches the app power reader, so Read fills the pane's app power
// line from the same poll as the patrol's report. A reader with none leaves the
// line absent.
func (r *CloudReader) WithApps(a *AppPowerReader) *CloudReader {
	r.apps = a
	return r
}

// Read returns the patrol's latest report as it stands at now, with the
// cloud-check status and the app power line the reader was given.
func (r *CloudReader) Read(now time.Time) *Cloud {
	c := r.read(now)
	if r.checks != nil {
		c.Checks = r.checks.Read(now)
	}
	if r.apps != nil {
		c.Apps = r.apps.Read(now)
	}
	return c
}

// read returns the patrol's latest report alone, leaving the checks and app
// power lines to Read so no path out of it forgets to fill them.
func (r *CloudReader) read(now time.Time) *Cloud {
	c := &Cloud{At: now, Projects: []string{}, Findings: []CloudFinding{}}
	rep, err := r.source.LatestPatrolReport(context.Background())
	if err != nil {
		switch {
		case errors.Is(err, cloudledger.ErrNoPatrolDir):
			c.State, c.Note, c.NoPatrol = CloudMissing, cloudNoPatrol, true
		case errors.Is(err, cloudledger.ErrNoReport):
			c.State, c.Note = CloudMissing, cloudNoReport
		case errors.Is(err, cloudledger.ErrNotConfigured):
			c.State, c.Note = CloudError, config.CloudNotConfigured
		case errors.Is(err, cloudledger.ErrOversize):
			c.State, c.Note = CloudError, cloudOversize
		case errors.Is(err, cloudledger.ErrInvalidReport):
			c.State, c.Note = CloudError, "the report is not valid JSON"
		default:
			// The reason rides the line: an unreadable Postgres source must
			// say why, and its own errors keep the DSN out of their text.
			c.State, c.Note = CloudError, "the report could not be read: "+err.Error()
		}
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
	case errors.Is(err, cloudledger.ErrNoHeartbeat):
		c.State, c.Note = CloudError, "the patrol's heartbeat is missing"
		return c
	case errors.Is(err, errCloudHeartbeat):
		c.State, c.Note = CloudError, cloudBadHeartbeat
		return c
	case err != nil:
		c.State, c.Note = CloudError, "the heartbeat could not be read: "+err.Error()
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
func (c *Cloud) addFindings(in []cloudledger.Finding) {
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

// readCloudFile reads the patrol's report at path, refusing one larger than max
// and one that is not a regular file, both before the read.
func readCloudFile(path string, max int64) ([]byte, error) {
	return readRegularCloudFile(path, max, errCloudOversize)
}

// readRegularCloudFile reads path when it is a regular file no larger than max,
// reporting a larger one as oversize, through the source's shared guard: a
// symlink named like one of the pane's files is refused rather than followed,
// and a FIFO, socket or device is refused before the read (gt-u3eb1).
func readRegularCloudFile(path string, max int64, oversize error) ([]byte, error) {
	return cloudledger.ReadRegularFile(path, max, oversize)
}

// readHeartbeat reads the patrol's heartbeat: the RFC3339 timestamp it writes
// beside the report each time it finishes a run.
func (r *CloudReader) readHeartbeat() (time.Time, error) {
	return r.source.Heartbeat(context.Background())
}
