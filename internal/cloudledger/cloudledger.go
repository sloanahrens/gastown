// Package cloudledger reads the cloud step's records: the patrol's latest
// report and heartbeat, and the release bot's ledger lines per app. The file
// source keeps the reads the dashboard and the daemon have always done; the
// Postgres source reads the same records from the cluster's ledger tables.
package cloudledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// The ledger tables the Postgres source reads, as devops landed them.
const (
	PatrolReportTable    = "ledger_patrol_report"
	PatrolHeartbeatTable = "ledger_patrol_heartbeat"
	ReleaseTable         = "ledger_release"
)

// The size bounds the file source applies before reading. The report's own
// files run a few kilobytes and a ledger grows by a line per run, so a larger
// one is not the writer's own output.
const (
	ReportMaxBytes    = 256 << 10
	HeartbeatMaxBytes = 64
	LedgerTailBytes   = 8 << 10
)

// The errors a source reports, each one a state the readers name rather than a
// guess: a missing report is not an empty one and an unreadable source is not a
// fresh heartbeat.
var (
	ErrNotConfigured = errors.New("cloudledger: cloud step not configured")
	ErrNoPatrolDir   = errors.New("cloudledger: no reports directory")
	ErrNoReport      = errors.New("cloudledger: no patrol report")
	ErrOversize      = errors.New("cloudledger: file over the size cap")
	ErrNotRegular    = errors.New("cloudledger: not a regular file")
	ErrInvalidReport = errors.New("cloudledger: report is not valid JSON")
	ErrNoHeartbeat   = errors.New("cloudledger: no heartbeat")
	ErrBadHeartbeat  = errors.New("cloudledger: heartbeat is not a timestamp")
	ErrNoRelease     = errors.New("cloudledger: no release")
	ErrBadRelease    = errors.New("cloudledger: release line is not JSON")
	ErrLedgerRead    = errors.New("cloudledger: ledger could not be read")
	ErrWriterRole    = errors.New("cloudledger: role can write the ledger")
	ErrEmptyDSN      = errors.New("cloudledger: DSN file is empty")
)

// Source reads the cloud step's records.
type Source interface {
	// LatestPatrolReport returns the newest report the patrol wrote.
	LatestPatrolReport(ctx context.Context) (Report, error)
	// Heartbeat returns when the patrol's last run finished.
	Heartbeat(ctx context.Context) (time.Time, error)
	// ReleasesSince returns the release lines for app after since, oldest first.
	ReleasesSince(ctx context.Context, app string, since time.Time) ([]Release, error)
	// LastRelease returns app's newest release line.
	LastRelease(ctx context.Context, app string) (Release, error)
}

// ReleaseLister names the apps a source holds release lines for, so the app
// power line can draw an app that has a ledger and no off marker.
type ReleaseLister interface {
	Apps(ctx context.Context) ([]string, error)
}

// Report is a patrol report as the ledger stores it: the schema version, the
// projects the run watched, and its findings in the report's own order. The
// other file fields carry nothing a reader here uses.
type Report struct {
	SchemaVersion int       `json:"SchemaVersion"`
	Projects      []string  `json:"Projects"`
	Findings      []Finding `json:"Findings"`
}

// Finding is one finding as the report writes it. Every field is untrusted
// text from another account's process; a reader strips it before display.
type Finding struct {
	ID         string `json:"ID"`
	Kind       string `json:"Kind"`
	Severity   string `json:"Severity"`
	Project    string `json:"Project"`
	Resource   string `json:"Resource"`
	Detail     string `json:"Detail"`
	ObservedAt string `json:"ObservedAt"`
}

// Release is one release bot ledger line for an app. At is zero when the
// line's time did not parse, so one bad time is a release without a date
// rather than a ledger the reader loses.
type Release struct {
	At      time.Time
	Action  string
	Reason  string
	Commit  string
	Version string
}

// releaseLine is a ledger line's JSON as the bot writes it. The release table
// stores these bytes in its line column, which is why both sources decode the
// same shape.
type releaseLine struct {
	Time    string `json:"time"`
	Action  string `json:"action"`
	Reason  string `json:"reason"`
	Commit  string `json:"commit"`
	Version string `json:"version"`
}

// release decodes one ledger line, refusing bytes that are not a JSON object.
func release(raw []byte) (Release, error) {
	var line releaseLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return Release{}, fmt.Errorf("%w: %v", ErrBadRelease, err)
	}
	r := Release{Action: line.Action, Reason: line.Reason, Commit: line.Commit, Version: line.Version}
	if at, err := time.Parse(time.RFC3339, strings.TrimSpace(line.Time)); err == nil {
		r.At = at
	}
	return r, nil
}

// decodeReport decodes a patrol report's bytes.
func decodeReport(raw []byte) (Report, error) {
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrInvalidReport, err)
	}
	return rep, nil
}

// NewSource returns the source the resolved settings select. A town with no
// cloud block keeps the file directories the readers have always used.
func NewSource(cs config.CloudSettings) Source {
	switch {
	case cs.Off:
		return offSource{}
	case cs.Postgres:
		return NewPostgresSource(cs.DSNFile)
	default:
		return NewFileSource(cs.ReportsDir, cs.ReleasesDir)
	}
}

// offSource is a present but empty cloud block: every read says the step is
// off, so a reader shows that rather than a guess.
type offSource struct{}

func (offSource) LatestPatrolReport(context.Context) (Report, error) {
	return Report{}, ErrNotConfigured
}

func (offSource) Heartbeat(context.Context) (time.Time, error) {
	return time.Time{}, ErrNotConfigured
}

func (offSource) ReleasesSince(context.Context, string, time.Time) ([]Release, error) {
	return nil, ErrNotConfigured
}

func (offSource) LastRelease(context.Context, string) (Release, error) {
	return Release{}, ErrNotConfigured
}

func (offSource) Apps(context.Context) ([]string, error) { return nil, ErrNotConfigured }
