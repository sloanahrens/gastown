package daemon

// The JSONL git backup's tracker branch: a rig whose route backend is the
// tracker (internal/beads/backend, gt-p85pw.57) is read through the tracker's
// export route (POST /v1/export, gt-p85pw.69) instead of Dolt SQL. The backup
// writes the same files under the same names, so the pollution filter, the
// spike guard and the commit after the export read a tracker rig the way they
// read a Dolt rig.
//
// A rig on the tracker is never read from Dolt: the database it left behind is
// stale, and a backup of it would look current. A tracker rig whose endpoint or
// token is missing fails its export, which raises the cycle's export alert like
// any other failed database.
//
// The export carries the rig's wisps as well as its durable issues; the backup
// writes only the durable ones, since a Dolt rig's backup leaves the wisp tables
// out (see trackerExportRow).
//
// The record count of a tracker rig counts what the export carries, not the
// seven Dolt tables it carried, so the tick a rig flips can read as a spike
// until the operator accepts the new level (spikeAcceptHint).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// jsonlTrackerExportTimeout bounds one database's export through the tracker.
// The whole rig streams in one call — its issues, then their labels,
// dependencies and comments — so one Dolt table's 60 s bound
// (jsonlExportTimeout) does not fit it, while the client's own 30-minute
// default would hold the backup cycle's maintenance lock past two ticks.
const jsonlTrackerExportTimeout = 5 * time.Minute

// trackerExportTables are the tables a tracker rig's export fills, in the order
// the backup writes them. The export carries nothing for events, config or
// metadata, so the backup leaves its copies of those as they are and says so in
// the log.
var trackerExportTables = []string{"issues", "labels", "dependencies", "comments"}

// trackerRigExporter streams one rig's export, calling emit for each record in
// stream order. It is the daemon's seam to the tracker client: a test hands it
// fake records, so the branch runs with no network and no sleeps.
type trackerRigExporter func(ctx context.Context, rig string, emit func(api.ExportRecord) error) error

// trackerRigByDatabase maps each database held by the tracker to the rig its
// export names, from the town's route table: a route whose backend is the
// tracker contributes prefix "gt-" -> database "gt" -> rig "gt".
//
// A tracker route the table cannot place in a database is an error, not a
// skipped route: the backup could not tell whether the database it is about to
// export through Dolt is that rig's, and reading a stale Dolt copy is the
// failure this branch exists to prevent. Two routes naming one database is an
// error for the same reason — the export could not say which rig it read.
//
// A town with no route table has no tracker rigs and no error: every database
// stays on the Dolt path, as before.
func trackerRigByDatabase(townRoot string) (map[string]string, error) {
	routes, err := beads.LoadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return nil, fmt.Errorf("reading the route table: %w", err)
	}
	rigs := map[string]string{}
	for _, route := range routes {
		if route.BackendName() != beads.BackendTracker {
			continue
		}
		rig := strings.TrimSuffix(route.Prefix, "-")
		beadsDir := filepath.Join(townRoot, route.Path, ".beads")
		db := doltserver.DatabaseForBeadsDir(beadsDir)
		if db == "" {
			return nil, fmt.Errorf("route %s is on the tracker but %s names no database, so the backup cannot tell which export it is",
				route.Prefix, filepath.Join(beadsDir, "metadata.json"))
		}
		if other, found := rigs[db]; found && other != rig {
			return nil, fmt.Errorf("routes for rigs %s and %s both name database %s", other, rig, db)
		}
		rigs[db] = rig
	}
	return rigs, nil
}

// jsonlTrackerRigs is trackerRigByDatabase for this daemon's town. A daemon
// with no config is a test driving exportDatabaseToJsonl directly: it has no
// route table, and so no tracker rigs.
func (d *Daemon) jsonlTrackerRigs() (map[string]string, error) {
	if d.config == nil {
		return nil, nil
	}
	return trackerRigByDatabase(d.config.TownRoot)
}

// trackerExporterFromEnv builds the production exporter: a tracker client from
// the daemon's environment, whose settings are the ones internal/beads/backend
// reads for a tracker rig (TRACKER_URL, and TRACKER_TOKEN or
// TRACKER_TOKEN_FILE). The error names the setting that is missing and never a
// token's value.
func trackerExporterFromEnv(getenv func(string) string) trackerRigExporter {
	return func(ctx context.Context, rig string, emit func(api.ExportRecord) error) error {
		cfg := client.FromEnv(getenv)
		if cfg.BaseURL == "" {
			return fmt.Errorf("rig %s is on the tracker but TRACKER_URL is not set", rig)
		}
		if cfg.Token == "" && cfg.TokenFile == "" {
			return fmt.Errorf("rig %s is on the tracker but neither TRACKER_TOKEN nor TRACKER_TOKEN_FILE is set", rig)
		}
		c, err := client.New(cfg)
		if err != nil {
			return fmt.Errorf("rig %s: %w", rig, err)
		}
		return c.Export(ctx, api.ExportRequest{Rig: rig}, emit)
	}
}

// exportTrackerDatabaseToJsonl exports one tracker rig into dbDir, as
// exportDatabaseToJsonl does for a Dolt database, and returns the number of
// records written across the tables.
//
// The whole stream is read before any file is written, so a stream that breaks
// part way leaves the previous cycle's files in place — the failed database
// keeps its last verified copy, as a failed Dolt export does.
func (d *Daemon) exportTrackerDatabaseToJsonl(rig, db, dbDir string) (int, error) {
	export := d.jsonlTrackerExportFn
	if export == nil {
		export = trackerExporterFromEnv(os.Getenv)
	}
	rows := make(map[string][]json.RawMessage, len(trackerExportTables))
	ctx, cancel := context.WithTimeout(context.Background(), jsonlTrackerExportTimeout)
	defer cancel()
	if err := export(ctx, rig, func(rec api.ExportRecord) error {
		table, row, err := trackerExportRow(rec)
		if err != nil {
			return err
		}
		if table == "" {
			return nil // a record the backup leaves out, such as a wisp
		}
		line, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		rows[table] = append(rows[table], line)
		return nil
	}); err != nil {
		return 0, fmt.Errorf("tracker export: %w", err)
	}
	total := 0
	for _, table := range trackerExportTables {
		n, err := writeTableShards(dbDir, table, rows[table], jsonlShardMaxBytes)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", table, err)
		}
		total += n
	}
	d.logger.Printf("jsonl_git_backup: %s: exported %d records from rig %s through the tracker export", db, total, rig)
	d.logger.Printf("jsonl_git_backup: %s: the tracker export carries no events, config or metadata — the backup's copies of those are left as they are", db)
	return total, nil
}

// trackerExportRow maps one record of a tracker export to the table it belongs
// in and the row the backup writes there; a record with no row is one the
// backup leaves out, and an empty table says so. The row's keys are the column
// names of the Dolt export the backup used to write, so a rig's files keep
// their shape when it moves to the tracker.
//
// A wisp gets no row: a Dolt rig's backup leaves the wisp tables out, and the
// pollution firewall drops any "-wisp-" id it finds in issues.jsonl, so a wisp
// written there would be deleted one step later.
//
// A column the export does not carry is left out rather than invented: an
// issue's started_at and bd's legacy columns, a link's row id and its separate
// target columns. A link that names another rig arrives as external:<rig>:<id>
// and is written to depends_on_external as that reference, since the exporter
// cannot see which of the three target columns bd's row used.
func trackerExportRow(rec api.ExportRecord) (string, any, error) {
	switch rec.Type {
	case api.RecordIssue:
		if rec.Issue == nil {
			return "", nil, missingPayload(rec.Type)
		}
		if rec.Issue.Ephemeral {
			return "", nil, nil
		}
		return "issues", trackerIssueRowOf(*rec.Issue), nil
	case api.RecordLabel:
		if rec.Label == nil {
			return "", nil, missingPayload(rec.Type)
		}
		return "labels", trackerLabelRow{IssueID: rec.Label.IssueID, Label: rec.Label.Label}, nil
	case api.RecordDependency:
		if rec.Dependency == nil {
			return "", nil, missingPayload(rec.Type)
		}
		return "dependencies", trackerDependencyRowOf(*rec.Dependency), nil
	case api.RecordComment:
		if rec.Comment == nil {
			return "", nil, missingPayload(rec.Type)
		}
		return "comments", trackerCommentRow{
			ID: rec.Comment.ID, IssueID: rec.Comment.IssueID, Author: rec.Comment.Author,
			Text: rec.Comment.Text, CreatedAt: trackerBackupTime(rec.Comment.CreatedAt),
		}, nil
	default:
		return "", nil, fmt.Errorf("a %q record is not one the backup writes", rec.Type)
	}
}

func missingPayload(recordType string) error {
	return fmt.Errorf("a %q record with no payload is not one the backup writes", recordType)
}

// trackerBackupTime renders a wire time the way the export's other timestamps
// are rendered: RFC 3339 in UTC, empty for the zero time. Postgres keeps
// microseconds and Dolt's JSON did not, so whole seconds keep two exports of
// an unchanged rig identical.
func trackerBackupTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func trackerBackupTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return trackerBackupTime(*t)
}

// The row types below are the shapes the backup's files have always had; each
// field's JSON name is the column name of bd's Dolt table for that file.

type trackerIssueRow struct {
	ID                 string `json:"id"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	Design             string `json:"design,omitempty"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty"`
	Notes              string `json:"notes,omitempty"`
	Status             string `json:"status"`
	Priority           int    `json:"priority"`
	Type               string `json:"issue_type"`
	Assignee           string `json:"assignee,omitempty"`
	Owner              string `json:"owner,omitempty"`
	CreatedBy          string `json:"created_by,omitempty"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
	ClosedAt           string `json:"closed_at,omitempty"`
	DeferUntil         string `json:"defer_until,omitempty"`
	CloseReason        string `json:"close_reason,omitempty"`
	Ephemeral          bool   `json:"ephemeral"`
	WispType           string `json:"wisp_type,omitempty"`
	EventKind          string `json:"event_kind,omitempty"`
	Payload            string `json:"payload,omitempty"`
	HookBead           string `json:"hook_bead,omitempty"`
	AgentState         string `json:"agent_state,omitempty"`
	LastActivity       string `json:"last_activity,omitempty"`
}

func trackerIssueRowOf(is api.Issue) trackerIssueRow {
	return trackerIssueRow{
		ID: is.ID, Title: is.Title, Description: is.Description, Design: is.Design,
		AcceptanceCriteria: is.AcceptanceCriteria, Notes: is.Notes,
		Status: is.Status, Priority: is.Priority, Type: is.Type,
		Assignee: is.Assignee, Owner: is.Owner, CreatedBy: is.CreatedBy,
		CreatedAt: trackerBackupTime(is.CreatedAt), UpdatedAt: trackerBackupTime(is.UpdatedAt),
		ClosedAt: trackerBackupTimePtr(is.ClosedAt), DeferUntil: trackerBackupTimePtr(is.DeferUntil),
		CloseReason: is.CloseReason, Ephemeral: is.Ephemeral, WispType: is.WispType,
		EventKind: is.EventKind, Payload: is.Payload, HookBead: is.HookBead,
		AgentState: is.AgentState, LastActivity: trackerBackupTimePtr(is.LastActivity),
	}
}

type trackerLabelRow struct {
	IssueID string `json:"issue_id"`
	Label   string `json:"label"`
}

type trackerDependencyRow struct {
	IssueID           string `json:"issue_id"`
	DependsOnIssueID  string `json:"depends_on_issue_id,omitempty"`
	DependsOnExternal string `json:"depends_on_external,omitempty"`
	Type              string `json:"type"`
	ThreadID          string `json:"thread_id,omitempty"`
	CreatedBy         string `json:"created_by,omitempty"`
	CreatedAt         string `json:"created_at"`
	Metadata          string `json:"metadata,omitempty"`
}

func trackerDependencyRowOf(dep api.ExportDependency) trackerDependencyRow {
	row := trackerDependencyRow{
		IssueID: dep.IssueID, Type: dep.Type, ThreadID: dep.ThreadID, CreatedBy: dep.CreatedBy,
		CreatedAt: trackerBackupTime(dep.CreatedAt), Metadata: trackerBackupMetadata(dep.Metadata),
	}
	if strings.HasPrefix(dep.DependsOn, "external:") {
		row.DependsOnExternal = dep.DependsOn
	} else {
		row.DependsOnIssueID = dep.DependsOn
	}
	return row
}

// trackerBackupMetadata is a link's stored metadata as bd's text column holds
// it: the JSON the tracker returned, with a JSON null counting as none.
func trackerBackupMetadata(raw json.RawMessage) string {
	metadata := string(raw)
	if metadata == "null" {
		return ""
	}
	return metadata
}

type trackerCommentRow struct {
	ID        string `json:"id"`
	IssueID   string `json:"issue_id"`
	Author    string `json:"author"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}
