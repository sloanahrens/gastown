package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// remoteDoltHost is a documentation address: not loopback, and an IP literal, so
// the local-host test answers without a name lookup.
const remoteDoltHost = "192.0.2.10"

// remoteServerFake is the Dolt server a backup reads. SHOW DATABASES answers
// with showOut or showErr; each export records the database and the directory
// it was handed.
type remoteServerFake struct {
	showOut   []byte
	showErr   error
	showCalls int
	exported  []string
	exportDir []string
}

func (s *remoteServerFake) run(_ context.Context, _ *exec.Cmd, table string, _ time.Duration) ([]byte, error) {
	if table != "SHOW DATABASES" {
		return nil, errors.New("unexpected query: " + table)
	}
	s.showCalls++
	return s.showOut, s.showErr
}

// newRemoteBackupDaemon wires a backup over a town whose data dir is absent, the
// way a pod sees it, with the Dolt server at host.
func newRemoteBackupDaemon(t *testing.T, townRoot, host string, server *remoteServerFake) (*Daemon, *notifyfake.Recorder, *strings.Builder) {
	t.Helper()
	var logs strings.Builder
	d, _ := backupTestDaemon(t)
	d.logger = log.New(&logs, "", 0)
	rec := notifyfake.New()
	d.notifier = rec
	d.config = &Config{TownRoot: townRoot}
	d.doltServer = &DoltServerManager{config: &DoltServerConfig{
		Enabled: true,
		Host:    host,
		Port:    3307,
		DataDir: filepath.Join(townRoot, ".dolt-data"),
	}}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, GitRepo: filepath.Join(t.TempDir(), "repo")},
	}}
	d.jsonlExportRunFn = server.run
	d.jsonlExportFn = func(db, repo, dir string, scrub bool) (int, error) {
		server.exported = append(server.exported, db)
		server.exportDir = append(server.exportDir, dir)
		dbDir := filepath.Join(repo, db)
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			return 0, err
		}
		writeNLines(t, filepath.Join(dbDir, "issues.jsonl"), 1)
		return 1, nil
	}
	return d, rec, &logs
}

// TestJSONLGitBackupRemoteServerExportsEveryServedDatabase: with a remote server
// and no local data dir the patrol lists the databases through the server and
// exports each one, from a directory that exists, and does not skip.
func TestJSONLGitBackupRemoteServerExportsEveryServedDatabase(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	server := &remoteServerFake{showOut: []byte(`{"rows":[` +
		`{"Database":"hq"},{"Database":"gt"},{"Database":"information_schema"},{"Database":"testdb_scratch"}]}`)}
	d, rec, logs := newRemoteBackupDaemon(t, townRoot, remoteDoltHost, server)

	d.syncJsonlGitBackup()

	if server.showCalls != 1 {
		t.Errorf("SHOW DATABASES ran %d time(s), want once through the server", server.showCalls)
	}
	if got := strings.Join(server.exported, ","); got != "gt,hq" {
		t.Errorf("exported %q, want the served production databases gt,hq", got)
	}
	for _, dir := range server.exportDir {
		if dir == filepath.Join(townRoot, ".dolt-data") {
			t.Errorf("export ran in the absent data dir %s", dir)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Errorf("export ran in %s, which is not an existing directory: %v", dir, err)
		}
	}
	if strings.Contains(logs.String(), "does not exist, skipping") {
		t.Errorf("remote server patrol skipped on the data dir; log:\n%s", logs.String())
	}
	if open := rec.OpenEscalations(alertKeyJSONLRemote); len(open) != 0 {
		t.Errorf("remote listing escalated on success: %+v", open)
	}
}

// TestJSONLGitBackupLocalServerMissingDataDirStillSkips: a local server with no
// data dir is the case that has always skipped, and still does.
func TestJSONLGitBackupLocalServerMissingDataDirStillSkips(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	server := &remoteServerFake{showOut: []byte(`{"rows":[{"Database":"hq"}]}`)}
	d, _, logs := newRemoteBackupDaemon(t, townRoot, "127.0.0.1", server)

	d.syncJsonlGitBackup()

	want := "data dir " + filepath.Join(townRoot, ".dolt-data") + " does not exist, skipping"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("local server with no data dir did not log the skip; log:\n%s", logs.String())
	}
	if server.showCalls != 0 || len(server.exported) != 0 {
		t.Errorf("local skip listed (%d) or exported %v, want neither", server.showCalls, server.exported)
	}
}

// TestJSONLGitBackupRemoteServerListFailureExportsNothing: a list the backup
// cannot read is a refusal, not an empty clean backup. Nothing is exported and
// the failure is raised.
func TestJSONLGitBackupRemoteServerListFailureExportsNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		showOut []byte
		showErr error
	}{
		{"the query fails", nil, errors.New("connection refused")},
		{"the output is not JSON", []byte("hq\ngt\n"), nil},
		{"the output has no rows", []byte(`{}`), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			server := &remoteServerFake{showOut: tt.showOut, showErr: tt.showErr}
			d, rec, logs := newRemoteBackupDaemon(t, townRoot, remoteDoltHost, server)

			d.syncJsonlGitBackup()

			if len(server.exported) != 0 {
				t.Errorf("exported %v after a failed list, want nothing", server.exported)
			}
			if open := rec.OpenEscalations(alertKeyJSONLRemote); len(open) != 1 {
				t.Errorf("open remote escalations = %+v, want one", open)
			}
			if !strings.Contains(logs.String(), "cannot list the databases on the Dolt server") {
				t.Errorf("failed list did not log the refusal; log:\n%s", logs.String())
			}
		})
	}
}

// TestJSONLGitBackupRemoteServerWithOnlySystemDatabasesHasNothingToExport: a
// server that serves only system and test databases reaches the existing
// no-databases refusal.
func TestJSONLGitBackupRemoteServerWithOnlySystemDatabasesHasNothingToExport(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	server := &remoteServerFake{showOut: []byte(`{"rows":[{"Database":"information_schema"},{"Database":"testdb_x"}]}`)}
	d, rec, _ := newRemoteBackupDaemon(t, townRoot, remoteDoltHost, server)

	d.syncJsonlGitBackup()

	if len(server.exported) != 0 {
		t.Errorf("exported %v, want nothing", server.exported)
	}
	if open := rec.OpenEscalations(alertKeyJSONLNoDBs); len(open) != 1 {
		t.Errorf("open no-databases escalations = %+v, want one", open)
	}
}

// TestJSONLGitBackupRemoteServerNoDoltClientDirExportsNothing: when the dolt
// client directory cannot be made, the commands cannot run, so nothing lists or
// exports and the failure is raised.
func TestJSONLGitBackupRemoteServerNoDoltClientDirExportsNothing(t *testing.T) {
	t.Parallel()
	townRoot := filepath.Join(t.TempDir(), "town-is-a-file")
	if err := os.WriteFile(townRoot, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := &remoteServerFake{showOut: []byte(`{"rows":[{"Database":"hq"}]}`)}
	d, rec, logs := newRemoteBackupDaemon(t, townRoot, remoteDoltHost, server)

	d.syncJsonlGitBackup()

	if server.showCalls != 0 || len(server.exported) != 0 {
		t.Errorf("listed (%d) or exported %v with no client directory, want neither", server.showCalls, server.exported)
	}
	if open := rec.OpenEscalations(alertKeyJSONLRemote); len(open) != 1 {
		t.Errorf("open remote escalations = %+v, want one", open)
	}
	if !strings.Contains(logs.String(), "creating the dolt client directory") {
		t.Errorf("client directory failure not logged; log:\n%s", logs.String())
	}
}

// TestJSONLGitBackupRemoteWorkDir: the client directory is private, under the
// town root, and a town root that is unset or not a directory is an error, never
// the current directory.
func TestJSONLGitBackupRemoteWorkDir(t *testing.T) {
	t.Parallel()
	fileRoot := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(fileRoot, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		townRoot string
		wantErr  bool
	}{
		{"an unset town root is refused", "", true},
		{"a town root that is a file is refused", fileRoot, true},
		{"a town root directory gets a private client directory", t.TempDir(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, err := jsonlRemoteWorkDir(tt.townRoot)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("jsonlRemoteWorkDir(%q) = %q, want an error", tt.townRoot, dir)
				}
				return
			}
			if err != nil {
				t.Fatalf("jsonlRemoteWorkDir(%q): %v", tt.townRoot, err)
			}
			if want := filepath.Join(tt.townRoot, ".runtime", "dolt-client"); dir != want {
				t.Errorf("jsonlRemoteWorkDir = %q, want %q", dir, want)
			}
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				t.Fatalf("client directory %s was not made: %v", dir, err)
			}
			if perm := info.Mode().Perm(); perm != 0o700 {
				t.Errorf("client directory mode = %o, want 0700", perm)
			}
		})
	}
}

// TestJSONLGitBackupParseServerDatabases: system and test databases are dropped,
// and output that is not the query's JSON is an error.
func TestJSONLGitBackupParseServerDatabases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		out     string
		want    string
		wantErr bool
	}{
		{"production databases are kept and sorted",
			`{"rows":[{"Database":"hq"},{"Database":"gt"}]}`, "gt,hq", false},
		{"system and test databases are dropped",
			`{"rows":[{"Database":"mysql"},{"Database":"dolt_cluster"},{"Database":"testdb_a"},{"Database":"hq"}]}`, "hq", false},
		{"an empty result is an empty list",
			`{"rows":[]}`, "", false},
		{"output that is not JSON is an error", `hq`, "", true},
		{"output without a rows key is an error", `{}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseServerJsonlDatabases([]byte(tt.out))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseServerJsonlDatabases(%q) = %v, want an error", tt.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseServerJsonlDatabases(%q): %v", tt.out, err)
			}
			if strings.Join(got, ",") != tt.want {
				t.Errorf("parseServerJsonlDatabases(%q) = %v, want %q", tt.out, got, tt.want)
			}
		})
	}
}
