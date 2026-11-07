package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// stubStatusReads is `gt dolt status`'s reads with every server call faked: the
// server answers, serves no database, the archive reader counts nothing and the
// beads config read prints nothing. A test replaces the field it exercises, so
// no status test reaches a live Dolt, a Docker container, a host process scan
// or the beads config on disk (gt-w1ekf).
func stubStatusReads(townRoot string) doltStatusReads {
	return doltStatusReads{
		townRoot:           func() (string, error) { return townRoot, nil },
		isRunning:          func(string) (bool, int, error) { return true, 0, nil },
		healthMetrics:      func(string) *doltserver.HealthMetrics { return &doltserver.HealthMetrics{} },
		verifyDatabases:    func(string) ([]string, []string, error) { return nil, nil, nil },
		servedDatabases:    func(*doltserver.Config) ([]string, error) { return nil, nil },
		archiveReader:      func(string, *doltserver.Config, []string) archiveBehindReader { return stubArchiveBehindReader{} },
		beadsRuntimeConfig: func(io.Writer, string) {},
		strayWarning:       func(string) {},
	}
}

// runStatusIntoBuffer runs the status report into a buffer, the writer the command takes
// instead of the process stdout, and returns what it printed.
func runStatusIntoBuffer(t *testing.T, reads doltStatusReads) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := runDoltStatusWith(&buf, reads)
	return buf.String(), err
}

// writeStatusFile writes one file under the town's daemon directory, where the
// status reads its state and its archive watermarks.
func writeStatusFile(t *testing.T, townRoot, name, body string) {
	t.Helper()
	dir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir daemon dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// refusingConnector is a pool whose every connection fails, so the status sees
// a server it cannot reach without opening a socket.
type refusingConnector struct{ err error }

func (c refusingConnector) Connect(context.Context) (driver.Conn, error) { return nil, c.err }
func (refusingConnector) Driver() driver.Driver                          { return archiveCountDriver{} }

// plainTownRoot writes a town root whose Dolt is the town's own host server,
// with a state file saying it started at startedAt and serves databases.
func plainTownRoot(t *testing.T, startedAt string, databases ...string) string {
	t.Helper()
	townRoot := t.TempDir()
	mayor := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayor, 0o755); err != nil {
		t.Fatalf("mkdir mayor dir: %v", err)
	}
	town := `{"type":"town","version":2,"name":"t","dolt":{"port":3307}}`
	if err := os.WriteFile(filepath.Join(mayor, "town.json"), []byte(town), 0o600); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	if databases == nil {
		databases = []string{}
	}
	state, err := json.Marshal(map[string]any{
		"running":    true,
		"pid":        1,
		"port":       3307,
		"started_at": startedAt,
		"data_dir":   filepath.Join(townRoot, ".dolt-data"),
		"databases":  databases,
	})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	writeStatusFile(t, townRoot, "dolt-state.json", string(state))
	return townRoot
}

// remoteTownRoot writes a town root whose Dolt is a remote server, which the
// status reports but never lists databases for.
func remoteTownRoot(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	mayor := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayor, 0o755); err != nil {
		t.Fatalf("mkdir mayor dir: %v", err)
	}
	// TEST-NET-3, so IsRemote resolves an address literal and never the
	// network.
	town := `{"type":"town","version":2,"name":"t","dolt":{"host":"203.0.113.5","port":3307}}`
	if err := os.WriteFile(filepath.Join(mayor, "town.json"), []byte(town), 0o600); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	return townRoot
}

// TestDoltStatusContainerPrintsArchiveLines: a container town's status lists
// the databases the container serves and each one's archive note, so the
// flatten runbook's "0 commits behind" check can be read in the town's own mode
// (gt-w1ekf).
func TestDoltStatusContainerPrintsArchiveLines(t *testing.T) {
	t.Parallel()
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	writeStatusFile(t, townRoot, "maintenance_state.json", `{
  "archive_watermarks": {
    "gt": {"date": "2026-11-02T09:30:00Z", "hashes": ["abc"]},
    "hq": {"date": "2026-11-01T23:00:00Z", "hashes": ["def"]}
  },
  "archive_events_watermarks": {
    "gt": {"created_at": "2026-11-02 09:15:00", "archived": [{"created_at": "2026-11-02 09:15:00", "id": "99"}]}
  }
}`)

	// The served list is read over SQL, as the container branch does: the
	// fake driver answers SHOW DATABASES with a system catalog the listing
	// must drop.
	served := &archiveCountServer{
		columns: []string{"Database"},
		rows:    [][]driver.Value{{"information_schema"}, {"gt"}, {"hq"}},
	}
	reads := stubStatusReads(townRoot)
	reads.servedDatabases = func(config *doltserver.Config) ([]string, error) {
		return servedDatabasesOverSQLWith(config, archiveCountOpener(t, served))
	}
	reads.archiveReader = func(string, *doltserver.Config, []string) archiveBehindReader {
		return stubArchiveBehindReader{
			commits: map[string]int64{"gt": 0, "hq": 3},
			events:  map[string]int64{"gt": 0, "hq": 0},
		}
	}

	out, err := runStatusIntoBuffer(t, reads)
	if err != nil {
		t.Fatalf("gt dolt status in container mode: %v", err)
	}
	for _, want := range []string{
		"Databases (served by the container):",
		"archived through 2026-11-02, 0 commits behind",
		"archived through 2026-11-01, 3 commits behind",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output does not carry %q:\n%s", want, out)
		}
	}
	// One archive line per database, and no reading that a database is
	// missing from the list.
	if got := strings.Count(out, " commits behind"); got != 2 {
		t.Errorf("status output carries %d commit archive lines, want one per database (2):\n%s", got, out)
	}
	if got := served.query; got != "SHOW DATABASES" {
		t.Errorf("the served list asked the server %q, want SHOW DATABASES", got)
	}
	if strings.Contains(out, "ARCHIVE CHECK UNAVAILABLE") {
		t.Errorf("status output says the archive check is unavailable although it listed two databases:\n%s", out)
	}
}

// TestDoltStatusContainerUnreadableWatermarks: a watermark file the status
// cannot read still prints the header and gives every database line the
// unreadable state, so no database reads as caught up (gt-w1ekf).
func TestDoltStatusContainerUnreadableWatermarks(t *testing.T) {
	t.Parallel()
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	writeStatusFile(t, townRoot, "maintenance_state.json", "{not json")

	reads := stubStatusReads(townRoot)
	reads.servedDatabases = func(*doltserver.Config) ([]string, error) {
		return []string{"gt", "hq"}, nil
	}

	out, err := runStatusIntoBuffer(t, reads)
	if err != nil {
		t.Fatalf("gt dolt status in container mode: %v", err)
	}
	if want := "Archive watermarks unreadable:"; !strings.Contains(out, want) {
		t.Errorf("status output does not carry the header %q:\n%s", want, out)
	}
	if want := "Databases (served by the container):"; !strings.Contains(out, want) {
		t.Errorf("status output does not carry the database header %q:\n%s", want, out)
	}
	if got := strings.Count(out, "archive state unreadable"); got != 2 {
		t.Errorf("status output carries %d unreadable archive lines, want one per database (2):\n%s", got, out)
	}
	if strings.Contains(out, "0 commits behind") {
		t.Errorf("status output reads a database as caught up although the watermarks are unreadable:\n%s", out)
	}
}

// TestDoltStatusUnavailableOnContainerPingFailure: a container whose SQL ping
// fails prints the exact unavailable line naming the ping, so a flatten's
// pre-check cannot pass over the failure (gt-w1ekf).
func TestDoltStatusUnavailableOnContainerPingFailure(t *testing.T) {
	t.Parallel()
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	reads := stubStatusReads(townRoot)
	reads.isRunning = func(string) (bool, int, error) {
		return false, 0, errors.New("dial tcp 127.0.0.1:3307: connect: connection refused")
	}

	out, err := runStatusIntoBuffer(t, reads)
	if err != nil {
		t.Fatalf("gt dolt status in container mode: %v", err)
	}
	want := "ARCHIVE CHECK UNAVAILABLE: container ping failed"
	if !strings.Contains(out, want) {
		t.Errorf("status output does not carry %q when the container ping fails:\n%s", want, out)
	}
	if !strings.Contains(out, "Ping: ") {
		t.Errorf("status output dropped the existing ping alarm:\n%s", out)
	}
}

// TestDoltStatusUnavailableOnContainerNotRunning: a stopped container prints
// the exact unavailable line naming it (gt-w1ekf).
func TestDoltStatusUnavailableOnContainerNotRunning(t *testing.T) {
	t.Parallel()
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	reads := stubStatusReads(townRoot)
	reads.isRunning = func(string) (bool, int, error) { return false, 0, nil }

	out, err := runStatusIntoBuffer(t, reads)
	if err != nil {
		t.Fatalf("gt dolt status in container mode: %v", err)
	}
	want := "ARCHIVE CHECK UNAVAILABLE: container not running"
	if !strings.Contains(out, want) {
		t.Errorf("status output does not carry %q when the container is stopped:\n%s", want, out)
	}
}

// TestDoltStatusUnavailableOnHostNotRunning: a stopped host server prints the
// exact unavailable line naming it (gt-w1ekf).
func TestDoltStatusUnavailableOnHostNotRunning(t *testing.T) {
	t.Parallel()
	townRoot := plainTownRoot(t, "2026-11-02T09:30:00Z")
	reads := stubStatusReads(townRoot)
	reads.isRunning = func(string) (bool, int, error) { return false, 0, nil }

	out, err := runStatusIntoBuffer(t, reads)
	if err != nil {
		t.Fatalf("gt dolt status on the host: %v", err)
	}
	want := "ARCHIVE CHECK UNAVAILABLE: host server not running"
	if !strings.Contains(out, want) {
		t.Errorf("status output does not carry %q when the host server is stopped:\n%s", want, out)
	}
}

// TestDoltStatusUnavailableOnEmptyDatabaseList: every path that lists no
// database, in container and host modes, prints the exact no-databases line
// rather than nothing (gt-w1ekf).
func TestDoltStatusUnavailableOnEmptyDatabaseList(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// wantErr is the failure the status must name beside the unavailable
		// line when it could not read the list at all.
		wantErr string
		reads   func(t *testing.T) doltStatusReads
	}{
		{
			name: "container serves no database",
			reads: func(t *testing.T) doltStatusReads {
				reads := stubStatusReads(externalContainerTownRoot(t, "gt-dolt"))
				served := &archiveCountServer{empty: true}
				reads.servedDatabases = func(config *doltserver.Config) ([]string, error) {
					return servedDatabasesOverSQLWith(config, archiveCountOpener(t, served))
				}
				return reads
			},
		},
		{
			name:    "container's served list cannot be read",
			wantErr: "listing the served databases",
			reads: func(t *testing.T) doltStatusReads {
				reads := stubStatusReads(externalContainerTownRoot(t, "gt-dolt"))
				served := &archiveCountServer{queryErr: errors.New("connection refused")}
				reads.servedDatabases = func(config *doltserver.Config) ([]string, error) {
					return servedDatabasesOverSQLWith(config, archiveCountOpener(t, served))
				}
				return reads
			},
		},
		{
			name:    "container's SQL connection cannot be opened",
			wantErr: "opening the SQL connection",
			reads: func(t *testing.T) doltStatusReads {
				reads := stubStatusReads(externalContainerTownRoot(t, "gt-dolt"))
				reads.servedDatabases = func(config *doltserver.Config) ([]string, error) {
					return servedDatabasesOverSQLWith(config, func(string) (*sql.DB, error) {
						return nil, errors.New("no connection pool")
					})
				}
				return reads
			},
		},
		{
			name:    "container's SQL connection cannot be made",
			wantErr: "connecting to the Dolt server",
			reads: func(t *testing.T) doltStatusReads {
				reads := stubStatusReads(externalContainerTownRoot(t, "gt-dolt"))
				reads.servedDatabases = func(config *doltserver.Config) ([]string, error) {
					return servedDatabasesOverSQLWith(config, func(string) (*sql.DB, error) {
						return sql.OpenDB(refusingConnector{err: errors.New("no listener")}), nil
					})
				}
				return reads
			},
		},
		{
			name: "host's live query serves no database",
			reads: func(t *testing.T) doltStatusReads {
				// The state names a database, but the live query is
				// authoritative and it serves none (gt-cs6).
				return stubStatusReads(plainTownRoot(t, "2026-11-02T09:30:00Z", "gt"))
			},
		},
		{
			name:    "host's live query fails with an empty cached list",
			wantErr: "Database verification failed",
			reads: func(t *testing.T) doltStatusReads {
				reads := stubStatusReads(plainTownRoot(t, "2026-11-02T09:30:00Z"))
				reads.verifyDatabases = func(string) ([]string, []string, error) {
					return nil, nil, errors.New("SHOW DATABASES: connection refused")
				}
				return reads
			},
		},
		{
			name: "host's state file is unreadable",
			reads: func(t *testing.T) doltStatusReads {
				townRoot := plainTownRoot(t, "2026-11-02T09:30:00Z", "gt")
				if err := os.Remove(filepath.Join(townRoot, "daemon", "dolt-state.json")); err != nil {
					t.Fatalf("remove state file: %v", err)
				}
				return stubStatusReads(townRoot)
			},
		},
		{
			name: "remote server lists no database",
			reads: func(t *testing.T) doltStatusReads {
				return stubStatusReads(remoteTownRoot(t))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reads := tt.reads(t)
			out, err := runStatusIntoBuffer(t, reads)
			if err != nil {
				t.Fatalf("gt dolt status: %v", err)
			}
			want := "ARCHIVE CHECK UNAVAILABLE: no databases listed"
			if !strings.Contains(out, want) {
				t.Errorf("status output does not carry %q when it lists no database:\n%s", want, out)
			}
			if strings.Contains(out, "commits behind") {
				t.Errorf("status output carries an archive line although it listed no database:\n%s", out)
			}
			if tt.wantErr != "" && !strings.Contains(out, tt.wantErr) {
				t.Errorf("status output does not name the failure %q:\n%s", tt.wantErr, out)
			}
		})
	}
}

// TestDoltHistoryDocStep2RefusesWithoutArchiveLines: the flatten runbook's
// step 2 stops on any unavailable line and on a status that shows no
// database's archive line, so the procedure cannot be read as satisfied by an
// output it did not check (gt-w1ekf).
func TestDoltHistoryDocStep2RefusesWithoutArchiveLines(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "dolt-history-offline.md"))
	if err != nil {
		t.Fatalf("reading docs/dolt-history-offline.md: %v", err)
	}
	doc := string(body)
	head := strings.Index(doc, "## Before any rewrite")
	if head < 0 {
		t.Fatal(`docs/dolt-history-offline.md has no "## Before any rewrite" section`)
	}
	section := doc[head:]
	end := strings.Index(section, "## Flatten:")
	if end < 0 {
		t.Fatal(`"Before any rewrite" does not end at the "## Flatten:" section`)
	}
	section = section[:end]
	start, stop := strings.Index(section, "2. "), strings.Index(section, "3. ")
	if start < 0 || stop < 0 || start >= stop {
		t.Fatalf(`"Before any rewrite" does not hold steps 2 and 3 in order:\n%s`, section)
	}
	step2 := section[start:stop]

	for _, want := range []string{
		"ARCHIVE CHECK UNAVAILABLE",
		"no database line shows an archive line",
		"archive state unreadable",
	} {
		if !strings.Contains(step2, want) {
			t.Errorf("step 2 of \"Before any rewrite\" does not carry %q:\n%s", want, step2)
		}
	}
}

// historyDocSection returns the flatten runbook from heading up to the next
// "## " heading, so a test can assert the text of the procedure that carries
// the rule rather than a phrase anywhere in the document.
func historyDocSection(t *testing.T, heading string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "dolt-history-offline.md"))
	if err != nil {
		t.Fatalf("reading docs/dolt-history-offline.md: %v", err)
	}
	doc := string(body)
	start := strings.Index(doc, heading)
	if start < 0 {
		t.Fatalf("docs/dolt-history-offline.md has no %q heading", heading)
	}
	section := doc[start:]
	if end := strings.Index(section[len(heading):], "\n## "); end >= 0 {
		section = section[:len(heading)+end]
	}
	return section
}

// TestDoltHistoryDocNamesArchiveNow: the flatten runbook runs the history
// archive pass under the overseer's dispatch hold and re-reads the status
// until every database is caught up, so the pre-flatten check cannot be read
// as satisfied by an archive nothing brought up to date (gt-xr2byt.3).
func TestDoltHistoryDocNamesArchiveNow(t *testing.T) {
	t.Parallel()
	section := historyDocSection(t, "## Before any rewrite")

	for _, want := range []string{
		"gt dolt archive-now",
		"The overseer creates the dispatch hold",
		"~/gt/seat-refill.hold",
		"no bird and no operator session creates or lifts",
		"With the hold set and the town parked, run",
		"read `gt dolt status` again until",
		"every database's commit line and events line read",
		"0 commits behind",
		"0 events behind",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("\"Before any rewrite\" does not carry %q:\n%s", want, section)
		}
	}

	archive := strings.Index(section, "gt dolt archive-now")
	check := strings.Index(section, "Check that every database's archive line")
	if archive < 0 || check < 0 || archive > check {
		t.Errorf("\"Before any rewrite\" does not run the archive pass before the 0-commits-behind check:\n%s", section)
	}
}

// TestDoltHistoryDocStatesTheArchiveCheckOnce: gt dolt status prints the commit
// and events archive counts on one line per database, so step 2 of "Before any
// rewrite" states the 0-commits-behind reading once. A second paragraph that
// restates it is the same check said twice (gt-37bxs3).
func TestDoltHistoryDocStatesTheArchiveCheckOnce(t *testing.T) {
	t.Parallel()
	step2 := historyDocStep(t, "## Before any rewrite", "\n2. ", "\n3. ")

	if n := strings.Count(step2, "`0 commits behind`"); n != 1 {
		t.Errorf("step 2 of \"Before any rewrite\" states the 0-commits-behind reading %d times, want once:\n%s", n, step2)
	}
}

// TestDoltHistoryDocExitOneSaysWhatItMeans: exit 1 from archive-now is a failed
// pass that leaves the archive not caught up, so step 2 says what the operator
// does before running it again, not only that the pass failed (gt-37bxs3).
func TestDoltHistoryDocExitOneSaysWhatItMeans(t *testing.T) {
	t.Parallel()
	step2 := strings.Join(strings.Fields(historyDocStep(t, "## Before any rewrite", "\n2. ", "\n3. ")), " ")

	for _, want := range []string{
		"1 is a failed pass",
		"the archive is not caught up",
		"before you run it again",
	} {
		if !strings.Contains(step2, want) {
			t.Errorf("step 2 of \"Before any rewrite\" does not carry %q:\n%s", want, step2)
		}
	}
}

// historyDocStep returns the part of the heading's section from the line that
// starts with from up to the line that starts with to, so a test reads one
// numbered step of the runbook.
func historyDocStep(t *testing.T, heading, from, to string) string {
	t.Helper()
	section := historyDocSection(t, heading)
	start, stop := strings.Index(section, from), strings.Index(section, to)
	if start < 0 || stop < 0 || start >= stop {
		t.Fatalf("%q does not hold the steps %q to %q in order:\n%s", heading, from, to, section)
	}
	return section[start:stop]
}

// TestDoltHistoryDocArchivesBeforeTheBackup: the runbook brings the archive to
// 0 behind before it stops the server and copies the database directory, and
// says the copy is taken only then, so the copy and the archive describe the
// same moment and the copy is not taken before the archive pass has run
// (gt-oqoedu).
func TestDoltHistoryDocArchivesBeforeTheBackup(t *testing.T) {
	t.Parallel()
	section := historyDocSection(t, "## Before any rewrite")

	archive := strings.Index(section, "gt dolt archive-now")
	behind := strings.Index(section, "Check that every database's archive line")
	stop := strings.Index(section, "Stop the server, then take the backup")
	if archive < 0 || behind < 0 || stop < 0 {
		t.Fatalf("\"Before any rewrite\" does not carry the archive pass, the 0-commits-behind check and the backup step in that form:\n%s", section)
	}
	if archive > stop {
		t.Errorf("\"Before any rewrite\" stops the server and takes the backup before it runs the archive pass:\n%s", section)
	}
	if behind > stop {
		t.Errorf("\"Before any rewrite\" stops the server and takes the backup before the 0-commits-behind check:\n%s", section)
	}

	const sequenced = "Take the copy only after step 2's checks pass"
	if !strings.Contains(section, sequenced) {
		t.Errorf("\"Before any rewrite\" does not say the copy is taken only after step 2's checks pass; it does not carry %q:\n%s", sequenced, section)
	}
}

// TestDoltHistoryDocStep3PointsAtStep2: step 2 states the 0-commits-behind
// check, so step 3 points back at it rather than restating it — a second copy
// drifts, and the reader who finds the stale one cannot tell which is live
// (gt-mlmkxz).
func TestDoltHistoryDocStep3PointsAtStep2(t *testing.T) {
	t.Parallel()
	step3 := historyDocStep(t, "## Before any rewrite", "\n3. ", "\n4. ")

	if n := strings.Count(step3, "0 commits behind"); n != 0 {
		t.Errorf("step 3 of \"Before any rewrite\" restates the 0-commits-behind reading %d times, want a pointer to step 2:\n%s", n, step3)
	}
	if !strings.Contains(step3, "step 2's checks pass") {
		t.Errorf("step 3 of \"Before any rewrite\" does not point at step 2's checks:\n%s", step3)
	}
}

// TestDoltHistoryDocBackupNamesTheStoppedServer: the backup instruction still
// copies the database directory with the server stopped and still points at
// the nightly backups and docs/dolt-restore.md, so reordering the steps does
// not turn the copy into one taken against a running server or lose the
// other copy (gt-oqoedu).
func TestDoltHistoryDocBackupNamesTheStoppedServer(t *testing.T) {
	t.Parallel()
	section := historyDocSection(t, "## Before any rewrite")

	// The runbook wraps its lines, so match the unwrapped prose.
	prose := strings.Join(strings.Fields(section), " ")
	const backupCopy = "copy the database directory while the server is stopped"
	at := strings.Index(prose, backupCopy)
	if at < 0 {
		t.Fatalf("\"Before any rewrite\" does not carry the backup copy taken while the server is stopped:\n%s", section)
	}
	step := prose[at:]
	if end := strings.Index(step, "Record row counts"); end >= 0 {
		step = step[:end]
	}

	for _, want := range []string{
		backupCopy,
		"nightly backups under",
		"~/gt-backups/dolt",
		"docs/dolt-restore.md",
	} {
		if !strings.Contains(step, want) {
			t.Errorf("the backup step does not carry %q:\n%s", want, step)
		}
	}
}

// TestDoltHistoryDocArchiveNowExitCodes: the runbook says to stop on each of
// archive-now's non-zero exits and reads each one as what happened, so an
// operator who stops does not read a pending or refused pass as a done one
// (gt-xr2byt.3).
func TestDoltHistoryDocArchiveNowExitCodes(t *testing.T) {
	t.Parallel()
	section := historyDocSection(t, "## Before any rewrite")

	for _, want := range []string{
		"Stop if it exits 1, 2 or 3",
		"1 is a failed pass",
		"2 is still pending, so run `gt dolt archive-now`",
		"3 is a refusal",
		"for want of the dispatch hold",
		"archive request is already pending",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("\"Before any rewrite\" does not carry %q:\n%s", want, section)
		}
	}
}

// TestDoltHistoryDocPlansAfterAWindow: the runbook plans the flatten for the
// morning after a maintenance window, names the daemon's clock and how to
// confirm it, and waits for the window's cycle to finish, so the archive the
// pre-flatten check reads is the window's pass plus archive-now (gt-xr2byt.3).
func TestDoltHistoryDocPlansAfterAWindow(t *testing.T) {
	t.Parallel()
	section := historyDocSection(t, "## Plan the flatten for the morning after a window")

	for _, want := range []string{
		"morning after a maintenance window",
		"gt dolt archive-now",
		"daemon's clock",
		"in window HH:MM",
		"daemon container's own timezone",
		"until gt-gg9hv.34",
		"confirm the clock with `date`",
		"docker exec <daemon container> date",
		"cycle completed",
		"before parking the town",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the planning section does not carry %q:\n%s", want, section)
		}
	}
}

// TestDoltHistoryDocChecksArchiveLevelZero: the flatten runbook checks the
// server's archive_level before the town is parked, names the line that
// passes, stops on anything else, and says the overseer owns the restart that
// rewrites the config, so the operator does not edit it (gt-dqdcj5).
func TestDoltHistoryDocChecksArchiveLevelZero(t *testing.T) {
	t.Parallel()
	section := historyDocSection(t, "## Before any rewrite")
	prose := strings.Join(strings.Fields(section), " ")

	for _, want := range []string{
		"grep -n -B1 'archive_level' ~/gt/.dolt-data/config.yaml",
		"`archive_level: 0`",
		"`enable: true`",
		"Anything else means stop",
		"The overseer owns the restart window that rewrites that file",
	} {
		if !strings.Contains(prose, want) {
			t.Errorf("\"Before any rewrite\" does not carry %q:\n%s", want, section)
		}
	}

	check := strings.Index(prose, "grep -n -B1 'archive_level'")
	park := strings.Index(prose, "Park the town")
	if check < 0 || park < 0 || check > park {
		t.Errorf("\"Before any rewrite\" does not run the archive_level check before it parks the town:\n%s", section)
	}
}

// TestDoltHistoryDocStartsTheServerAfterTheCopy: after the backup stops the
// server, the runbook starts it again and reads gt dolt status until every
// database is listed, before the row counts and the flatten SQL run against
// the live server (gt-dqdcj5).
func TestDoltHistoryDocStartsTheServerAfterTheCopy(t *testing.T) {
	t.Parallel()
	section := historyDocSection(t, "## Before any rewrite")
	prose := strings.Join(strings.Fields(section), " ")

	copyAt := strings.Index(prose, "copy the database directory while the server is stopped")
	if copyAt < 0 {
		t.Fatalf("\"Before any rewrite\" does not carry the backup copy taken while the server is stopped:\n%s", section)
	}
	// Step 2 already reads gt dolt status, so search for the restart only
	// after the copy.
	after := prose[copyAt:]
	start := strings.Index(after, "`gt dolt start`")
	status := strings.Index(after, "read `gt dolt status`")
	rows := strings.Index(after, "Record row counts")
	if start < 0 || status < 0 || rows < 0 {
		t.Fatalf("\"Before any rewrite\" does not start the server, read status and count rows after the copy:\n%s", section)
	}
	if status < start || rows < status {
		t.Errorf("\"Before any rewrite\" does not start the server and read status after the copy, before the row counts:\n%s", section)
	}
	if !strings.Contains(after[status:rows], "every database") {
		t.Errorf("the status read after `gt dolt start` does not name every database:\n%s", after[status:rows])
	}

	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "dolt-history-offline.md"))
	if err != nil {
		t.Fatalf("reading docs/dolt-history-offline.md: %v", err)
	}
	doc := string(body)
	flatten := strings.Index(doc, "CALL DOLT_RESET")
	if flatten < 0 {
		t.Fatal("docs/dolt-history-offline.md has no CALL DOLT_RESET flatten SQL")
	}
	if strings.Index(doc, "`gt dolt start`") > flatten {
		t.Errorf("the flatten SQL runs before the runbook starts the server again")
	}
	if strings.Index(doc, "`gt dolt start`") < 0 {
		t.Fatal("docs/dolt-history-offline.md has no `gt dolt start` step, so the flatten order cannot be checked")
	}
}

// TestDoltHistoryDocVerifiesAfterTheFlatten: after the flatten the runbook
// reads each database's 1-commit-behind archive state, runs archive-now again
// and re-reads status until every database is 0 behind, then re-counts the
// tables against the counts recorded before the flatten (gt-dqdcj5).
func TestDoltHistoryDocVerifiesAfterTheFlatten(t *testing.T) {
	t.Parallel()
	section := historyDocSection(t, "## Flatten:")
	prose := strings.Join(strings.Fields(section), " ")

	for _, want := range []string{
		"`1 commits behind`",
		"`gt dolt archive-now`",
		"until every database reads `0 commits behind`",
		"Re-count every table against the counts recorded in step 4",
	} {
		if !strings.Contains(prose, want) {
			t.Errorf("the flatten section does not carry %q:\n%s", want, section)
		}
	}

	behind := strings.Index(prose, "`1 commits behind`")
	archive := strings.Index(prose, "`gt dolt archive-now`")
	recount := strings.Index(prose, "Re-count every table")
	if behind < 0 || archive < 0 || recount < 0 || behind > archive || archive > recount {
		t.Errorf("the flatten section does not verify in order: archive state, archive-now, then re-count:\n%s", section)
	}
}

// TestDoltHistoryDocNamesTheRestoreSection: the flatten runbook's stop-and-restore
// rule points at docs/dolt-restore.md section 3, and that section heading is
// the one the restore doc really carries, so the pointer cannot go stale
// (gt-dqdcj5).
func TestDoltHistoryDocNamesTheRestoreSection(t *testing.T) {
	t.Parallel()
	section := historyDocSection(t, "## Flatten:")
	prose := strings.Join(strings.Fields(section), " ")

	const rule = "A table that lost rows or disappeared means stop"
	at := strings.Index(prose, rule)
	if at < 0 {
		t.Fatalf("the flatten section does not carry the lost-rows stop rule:\n%s", section)
	}
	paragraph := prose[at:]
	if end := strings.Index(paragraph, "A table that gained rows"); end >= 0 {
		paragraph = paragraph[:end]
	}

	for _, want := range []string{
		"docs/dolt-restore.md",
		"section 3",
		"\"Replace a live database (operator only)\"",
	} {
		if !strings.Contains(paragraph, want) {
			t.Errorf("the lost-rows stop rule does not carry %q:\n%s", want, paragraph)
		}
	}

	restore, err := os.ReadFile(filepath.Join("..", "..", "docs", "dolt-restore.md"))
	if err != nil {
		t.Fatalf("reading docs/dolt-restore.md: %v", err)
	}
	if !strings.Contains(string(restore), "## 3. Replace a live database (operator only)") {
		t.Errorf("docs/dolt-restore.md no longer has the section 3 heading the runbook names")
	}
}
