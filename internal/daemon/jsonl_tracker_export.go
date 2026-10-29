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
// The route table is read once per cycle, and strictly: a table the backup
// cannot read whole places nothing it did not read. A database no readable
// route names is refused rather than exported from Dolt, because it may be a
// tracker rig whose route the backup could not read, and the cycle raises the
// table's failure. A database the table does place is exported as its route
// says, so a Dolt rig's backup never depends on a route only tracker rigs are
// read through.
//
// The export carries the rig's wisps as well as its durable issues; the backup
// writes only the durable ones, since a Dolt rig's backup leaves the wisp
// tables out (see trackerExportRow and trackerSpool.publish). The stream's
// ephemeral flag is the authority, so a wisp's rows are dropped whether they
// arrive before or after the wisp's own issue record.
//
// The record count of a tracker rig counts what the export carries, not the
// seven Dolt tables it carried. The commit subject marks a tracker-measured
// count (jsonlTrackerCountToken) so the spike guard compares it with the
// counts measured the same way, not with the Dolt-era level it is not
// comparable to.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// trackerRoutes is the town's tracker routing as one backup cycle reads it.
type trackerRoutes struct {
	// rigs maps a database to the rig whose export fills it.
	rigs map[string]string
	// refused maps a database to why the backup must not export it at all:
	// two tracker routes name it, so an export of it could not say which rig
	// it read, and reading the wrong one writes another rig's beads over the
	// offsite copy.
	refused map[string]string
	// placed names every database a readable route gives a store, Dolt or
	// tracker. A database it does not name is one the backup cannot attribute
	// once the table has a problem (see unplaced).
	placed map[string]bool
	// problems names every tracker route the table could not place in a
	// database, and the table's own failure to read whole. A problem makes an
	// unattributed database unexportable: the backup cannot tell whether it is
	// a tracker rig whose route the table could not read, and reading it from
	// Dolt would back up the stale copy the rig left behind.
	problems []string
}

// trackerRouteSet is a routing with no tracker rigs, which a daemon with no
// config (a test driving exportDatabaseToJsonl directly) has.
func trackerRouteSet() trackerRoutes {
	return trackerRoutes{rigs: map[string]string{}, refused: map[string]string{}, placed: map[string]bool{}}
}

// unplaced is why the backup must not export db from Dolt, or "" when the
// routing can place it or reads cleanly. A clean table that does not name db
// leaves it on Dolt, as it was before this branch existed; a table with a
// problem does not, since the missing route could be the one that moves db to
// the tracker.
func (r trackerRoutes) unplaced(db string) string {
	if len(r.problems) == 0 || r.placed[db] {
		return ""
	}
	return fmt.Sprintf("the route table could not be read whole (%s), so the backup cannot tell whether %s is on the tracker; it is not exported from Dolt", strings.Join(r.problems, "; "), db)
}

// readTrackerRoutes reads the town's route table for the backup: each database
// the tracker holds is mapped to the rig its export names, from a route whose
// backend is the tracker. A town with no route table has no tracker rigs.
func readTrackerRoutes(townRoot string) trackerRoutes {
	routes := trackerRouteSet()
	// The routes read before a line the table could not give are still placed:
	// a Dolt rig the table names keeps its backup, while the databases it does
	// not name are refused (unplaced) rather than risked on Dolt.
	table, err := loadTrackerRouteTable(filepath.Join(townRoot, ".beads"))
	if err != nil {
		routes.problems = append(routes.problems, fmt.Sprintf("reading the route table: %v", err))
	}
	for _, route := range table {
		beadsDir := filepath.Join(townRoot, route.Path, ".beads")
		db := doltserver.DatabaseForBeadsDir(beadsDir)
		if db != "" {
			routes.placed[db] = true
		}
		switch route.BackendName() {
		case beads.BackendDolt:
			continue
		case beads.BackendTracker:
		default:
			// A backend neither store names is not one the backup can read the
			// database from, and defaulting it to Dolt would export the stale
			// copy a rig that moved left behind.
			routes.problems = append(routes.problems, fmt.Sprintf(
				"route %s names the backend %q, which is neither %q nor %q", route.Prefix, route.BackendName(), beads.BackendDolt, beads.BackendTracker))
			if db != "" {
				routes.refused[db] = fmt.Sprintf("route %s names the backend %q, which is neither %q nor %q", route.Prefix, route.BackendName(), beads.BackendDolt, beads.BackendTracker)
			}
			continue
		}
		rig := strings.TrimSuffix(route.Prefix, "-")
		if db == "" {
			routes.problems = append(routes.problems, fmt.Sprintf(
				"route %s is on the tracker but %s names no database, so the backup cannot tell which export it is",
				route.Prefix, filepath.Join(beadsDir, "metadata.json")))
			continue
		}
		if other, claimed := routes.rigs[db]; claimed && other != rig {
			// Two rigs' exports would fill one directory, and the backup could
			// not say which one it read, so this database is refused rather
			// than exported as whichever route came first.
			delete(routes.rigs, db)
			routes.refused[db] = fmt.Sprintf("routes for rigs %s and %s both name database %s, so an export of it could not say which rig it read", other, rig, db)
			continue
		}
		if _, refused := routes.refused[db]; refused {
			continue
		}
		routes.rigs[db] = rig
	}
	return routes
}

// loadTrackerRouteTable parses the town's routes.jsonl. Unlike
// beads.LoadRoutes, which warns on stderr and skips a line it cannot parse, a
// malformed line stops the read: a skipped line could be the tracker route of
// the database it named, and a daemon never sees that warning before it backs
// the database up from the Dolt copy the rig left behind. The routes read
// before the bad line are returned with the error, so a rig the table did place
// is not refused for a neighbour's line.
func loadTrackerRouteTable(beadsDir string) ([]beads.Route, error) {
	path := filepath.Join(beadsDir, beads.RoutesFileName)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // No table is not an error: it names no tracker rig.
		}
		return nil, err
	}
	defer f.Close()

	var table []beads.Route
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var route beads.Route
		if err := json.Unmarshal([]byte(text), &route); err != nil {
			return table, fmt.Errorf("%s:%d: %v", path, line, err)
		}
		if route.Prefix == "" || route.Path == "" {
			return table, fmt.Errorf("%s:%d: a route with no prefix or no path is not one the backup can place", path, line)
		}
		table = append(table, route)
	}
	if err := scanner.Err(); err != nil {
		return table, err
	}
	return table, nil
}

// jsonlTrackerRoutes is readTrackerRoutes for this daemon's town.
func (d *Daemon) jsonlTrackerRoutes() trackerRoutes {
	if d.config == nil {
		return trackerRouteSet()
	}
	return readTrackerRoutes(d.config.TownRoot)
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
// rows written across the tables.
//
// Records are staged as they arrive and nothing is published until the stream
// ends, so a stream that breaks part way leaves the previous cycle's files in
// place — the failed database keeps its last verified copy, as a failed Dolt
// export does. At publish, one table at a time is packed into that table's
// shards in memory, so the daemon holds one table's rows at a time rather than
// every table's.
func (d *Daemon) exportTrackerDatabaseToJsonl(rig, db, dbDir string) (int, error) {
	export := d.jsonlTrackerExportFn
	if export == nil {
		export = trackerExporterFromEnv(os.Getenv)
	}
	spool := newTrackerSpool(dbDir)
	defer spool.discard()

	ctx, cancel := context.WithTimeout(context.Background(), jsonlTrackerExportTimeout)
	defer cancel()
	if err := export(ctx, rig, spool.add); err != nil {
		return 0, fmt.Errorf("tracker export: %w", err)
	}
	total, err := spool.publish()
	if err != nil {
		return 0, err
	}
	d.logger.Printf("jsonl_git_backup: %s: exported %d records from rig %s through the tracker export", db, total, rig)
	d.logger.Printf("jsonl_git_backup: %s: the tracker export carries no events, config or metadata — the backup's copies of those are left as they are", db)
	return total, nil
}

// trackerSpoolName is one table's staging file. It carries the shard temp's
// suffix so removeShardTemps, which publishTableShards runs for the table,
// clears it with the rest, and so jsonlShardPaths never reads it as a shard.
func trackerSpoolName(table string) string {
	return table + ".spool.jsonl.tmp"
}

// trackerSpool stages one rig's export in dbDir until the whole stream has
// arrived: one file per table, appended as the records come in, so the rows
// cost disk while the stream runs. publish reads each table back once, packing
// its surviving rows into that table's shards in memory — one table's rows at
// a time, and only the shards publishTableShards is given.
type trackerSpool struct {
	dir   string
	files map[string]*os.File
	// wisps are the ids of the issues the export marked ephemeral, known only
	// once the stream has ended. Their labels, dependencies and comments are
	// dropped at publish, when the flag is known whatever order the stream
	// used: the export carries them inline, and a Dolt rig's backup leaves the
	// wisp tables out, so a wisp row written here would be a record kind the
	// Dolt path never produced.
	wisps map[string]bool
}

func newTrackerSpool(dir string) *trackerSpool {
	return &trackerSpool{
		dir:   dir,
		files: map[string]*os.File{},
		wisps: map[string]bool{},
	}
}

// add stages one record of the export stream.
func (s *trackerSpool) add(rec api.ExportRecord) error {
	if rec.Type == api.RecordIssue && rec.Issue != nil && rec.Issue.Ephemeral {
		s.wisps[rec.Issue.ID] = true
		return nil
	}
	table, row, err := trackerExportRow(rec)
	if err != nil {
		return err
	}
	if table == "" {
		return nil // a record the backup leaves out
	}
	if table == "issues" {
		return s.stage(table, trackerLine(row))
	}
	// The row's owner is staged with it: whether the owner is a wisp is not
	// known until the stream ends (see trackerSpool.wispOwned).
	return s.stageOwned(table, trackerRecordOwner(rec), trackerLine(row))
}

// trackerRecordOwner is the issue a record belongs to, which decides whether it
// is a wisp's row.
func trackerRecordOwner(rec api.ExportRecord) string {
	switch {
	case rec.Label != nil:
		return rec.Label.IssueID
	case rec.Dependency != nil:
		return rec.Dependency.IssueID
	case rec.Comment != nil:
		return rec.Comment.IssueID
	case rec.Issue != nil:
		return rec.Issue.ID
	}
	return ""
}

// wispOwned reports whether an issue's rows belong to a wisp. It is consulted
// at publish, once the stream has ended, so the ephemeral flag decides for a
// wisp's rows whatever order they arrived in compared with the wisp's issue
// record. The id form is the pollution firewall's own test for a wisp
// (testPollutionPatterns), which catches the rows of a wisp the stream never
// names.
func (s *trackerSpool) wispOwned(issueID string) bool {
	return s.wisps[issueID] || strings.Contains(issueID, "-wisp-")
}

// stage appends one line to table's staging file, opening the file on first
// use. Opening and writing share one error because both mean the row did not
// reach the spool, and a partial spool must not be published.
func (s *trackerSpool) stage(table string, line []byte) error {
	f, err := s.file(table)
	if err == nil {
		_, err = f.Write(append(line, '\n'))
	}
	if err != nil {
		return fmt.Errorf("staging %s: %w", table, err)
	}
	return nil
}

// stageOwned stages one row of a table keyed by an issue with its owner's id in
// front, so publish can drop the row if the stream marks the owner ephemeral.
func (s *trackerSpool) stageOwned(table, owner string, line []byte) error {
	return s.stage(table, append([]byte(owner+"\t"), line...))
}

// file opens (once) the staging file of table.
func (s *trackerSpool) file(table string) (*os.File, error) {
	if f, open := s.files[table]; open {
		return f, nil
	}
	f, err := os.OpenFile(filepath.Join(s.dir, trackerSpoolName(table)), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	s.files[table] = f
	return f, nil
}

// publish writes the staged rows as the backup's shards and returns the number
// of rows written, the rows of a wisp's issues excluded. It publishes one table
// at a time, so an error part way leaves the tables already published from this
// stream beside the rest from the previous export; the caller reports the
// database as failed, and no count is committed for it.
func (s *trackerSpool) publish() (int, error) {
	s.close()
	total := 0
	for _, table := range trackerExportTables {
		shards, rows, err := s.tableShards(table)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", table, err)
		}
		if _, err := publishTableShards(osShardOps(), s.dir, table, shards, rows); err != nil {
			return 0, fmt.Errorf("%s: %w", table, err)
		}
		total += rows
	}
	return total, nil
}

// tableShards packs one table's staged rows into the shards published for it
// and reports how many rows they hold. A row an ephemeral issue owns is left
// out here rather than when it was staged, so the flag decides for the rows
// that arrived before their issue's record. A table the export had no record
// for still writes its (empty) first shard, as the Dolt export does.
func (s *trackerSpool) tableShards(table string) ([]*bytes.Buffer, int, error) {
	builder := newShardBuilder(jsonlShardMaxBytes)
	f, err := os.Open(filepath.Join(s.dir, trackerSpoolName(table)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return builder.shards(), 0, nil
		}
		return nil, 0, fmt.Errorf("reading the staged export: %w", err)
	}
	defer f.Close()

	rows := 0
	reader := bufio.NewReader(f)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			row, owner, err := stagedRow(table, line)
			if err != nil {
				return nil, 0, err
			}
			if !s.wispOwned(owner) {
				builder.add(row)
				rows++
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, 0, fmt.Errorf("reading the staged export: %w", readErr)
		}
	}
	return builder.shards(), rows, nil
}

// stagedRow splits a staged line into the row the backup writes and the issue
// that owns it; the issues table's lines are issue records, which no link owns.
// A link row staged with no owner is a spool the export cannot trust, so it
// fails the publish rather than write a row it cannot place.
func stagedRow(table string, line []byte) (row []byte, owner string, err error) {
	if table == "issues" {
		return line, "", nil
	}
	id, row, ok := bytes.Cut(line, []byte{'\t'})
	if !ok {
		return nil, "", fmt.Errorf("a staged %s row names no owning issue", table)
	}
	return row, string(id), nil
}

// close closes the staging files so publish can read them. A later call is a
// no-op, so it is safe from both publish and discard. A close that fails loses
// nothing silently: publish hands publishTableShards the number of rows it read
// back, and verifyJsonlShards reads the published files again.
func (s *trackerSpool) close() {
	for table, f := range s.files {
		_ = f.Close()
		delete(s.files, table)
	}
}

// discard removes the staging files an unpublished export left. The rows of a
// published table are already cleared with the table's temps; this is what
// removes the ones a stream that broke part way left behind, and a removal
// that fails is not reported because the export's own failure is the one to
// report.
func (s *trackerSpool) discard() {
	s.close()
	for _, table := range trackerExportTables {
		_ = os.Remove(filepath.Join(s.dir, trackerSpoolName(table)))
	}
}

// trackerExportRow maps one record of a tracker export to the table it belongs
// in and the row the backup writes there; a record with no row is one the
// backup leaves out, and an empty table says so. The row's keys are the column
// names of the Dolt export the backup used to write, so a rig's files keep
// their shape when it moves to the tracker.
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

// trackerLine encodes one row of the backup as a file line. The row types hold
// only strings, numbers and bools, so encoding cannot fail. The encoder leaves
// <, > and & as they are: Dolt's JSON output does not escape them, and an
// escaped line would rewrite an issue that never changed.
func trackerLine(row any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(row)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
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
