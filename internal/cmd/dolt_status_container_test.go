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
