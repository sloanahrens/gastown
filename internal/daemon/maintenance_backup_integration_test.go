//go:build integration

package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// TestIntegrationNightlyBackupRestores is the restore procedure of
// docs/dolt-restore.md run end to end: the daemon's real backup path
// (maintenanceBackup -> CALL dolt_backup('sync-url')) against a private
// dolt sql-server on a temp data dir, then `dolt backup restore` into a new
// dir, a second server on that copy, and a query. It needs the dolt binary,
// not Docker; it never touches the shared container or the live server.
func TestIntegrationNightlyBackupRestores(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not on PATH")
	}
	tmp := t.TempDir()
	live, restored, backups := filepath.Join(tmp, "live"), filepath.Join(tmp, "restored"), filepath.Join(tmp, "backups")
	for _, dir := range []string{live, restored} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	port := startTestDoltServer(t, live)
	conn := openPrivateDolt(t, port, "")
	ctx, cancel := context.WithTimeout(context.Background(), testDoltSQLTimeout)
	defer cancel()
	run := func(q string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// A committed row, an uncommitted one, and a dolt_ignored table: the
	// backup must carry the working set, not just the commits.
	run("CREATE DATABASE demo")
	run("USE demo")
	run("CREATE TABLE issues (id VARCHAR(20) PRIMARY KEY, title TEXT)")
	run("INSERT INTO issues VALUES ('gt-1', 'committed')")
	run("INSERT INTO dolt_ignore VALUES ('events', true)")
	run("CALL DOLT_COMMIT('-Am', 'init')")
	run("CREATE TABLE events (id INT PRIMARY KEY, v TEXT)")
	run("INSERT INTO events VALUES (1, 'ignored')")
	run("INSERT INTO issues VALUES ('gt-2', 'uncommitted')")

	// The daemon reaches the server through the town's endpoint only.
	town := filepath.Join(tmp, "town")
	writeManagedDoltConfig(t, town, fmt.Sprintf("listener:\n  port: %d\n", port))
	var logs bytes.Buffer
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(&logs, "", 0),
	}
	d.maint.backupRoot = func() (string, error) { return backups, nil }
	d.maint.escalate = func(_ *Daemon, _, msg string) { t.Errorf("escalated: %s", msg) }

	if res := d.maintenanceBackup([]string{"demo"}); res.outcome != gcOutcomeCompleted {
		t.Fatalf("maintenanceBackup = %v (%s)\n%s", res.outcome, res.reason, logs.String())
	}
	night, ok, err := doltbackup.Newest(backups)
	if err != nil || !ok || !night.Has("demo") {
		t.Fatalf("no committed night holding demo: %+v %v\n%s", night, err, logs.String())
	}
	t.Logf("backup: %s\n%s", night.Path, logs.String())

	// The CLI needs an identity; the hermetic HOME has none, so give it a
	// private dolt root.
	doltRoot := filepath.Join(tmp, "doltroot")
	osexec(t, doltRoot, tmp, "dolt", "config", "--global", "--add", "user.name", "gt-test")
	osexec(t, doltRoot, tmp, "dolt", "config", "--global", "--add", "user.email", "gt-test@example.invalid")
	restore := osexec(t, doltRoot, restored, "dolt", "backup", "restore", "file://"+filepath.Join(night.Path, "demo"), "demo")
	t.Logf("dolt backup restore: %s", restore)

	rport := startTestDoltServer(t, restored)
	rconn := openPrivateDolt(t, rport, "demo")
	var title string
	if err := rconn.QueryRowContext(ctx, "SELECT title FROM issues WHERE id = 'gt-2'").Scan(&title); err != nil {
		t.Fatalf("restored copy has no uncommitted row: %v", err)
	}
	var issues, events, commits int
	for q, dst := range map[string]*int{
		"SELECT COUNT(*) FROM issues":   &issues,
		"SELECT COUNT(*) FROM events":   &events,
		"SELECT COUNT(*) FROM dolt_log": &commits,
	} {
		if err := rconn.QueryRowContext(ctx, q).Scan(dst); err != nil {
			t.Fatalf("%s on the restored copy: %v", q, err)
		}
	}
	if title != "uncommitted" || issues != 2 || events != 1 || commits < 2 {
		t.Errorf("restored copy: gt-2=%q issues=%d events=%d commits=%d; want uncommitted, 2, 1, >=2",
			title, issues, events, commits)
	}
	t.Logf("restored copy served on :%d: gt-2=%q issues=%d events=%d commits=%d", rport, title, issues, events, commits)
}

// startTestDoltServer serves dataDir on a free loopback port until the test
// ends and returns the port. It refuses the live server's port.
func startTestDoltServer(t *testing.T, dataDir string) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if port == doltserver.DefaultPort {
		t.Fatalf("free port %d is the live server's", port)
	}
	cmd := exec.Command("dolt", "sql-server", "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--data-dir", dataDir)
	cmd.Dir = dataDir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			c.Close()
			return port
		}
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server on :%d never accepted: %v\n%s", port, err, out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func openPrivateDolt(t *testing.T, port int, db string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s?timeout=5s", port, db))
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1) // USE must stick to the one connection
	t.Cleanup(func() { conn.Close() })
	return conn
}

// osexec runs a dolt CLI command in dir with DOLT_ROOT_PATH=doltRoot.
func osexec(t *testing.T, doltRoot, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DOLT_ROOT_PATH="+doltRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}
