//go:build integration

package restorecheck

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// pgBin is the directory holding the PostgreSQL client commands.
func pgBin(t *testing.T) string {
	t.Helper()
	exists := func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && !st.IsDir()
	}
	bin, err := pgtest.Locate(os.Getenv, exists, exec.LookPath)
	if err != nil {
		t.Fatalf("locate PostgreSQL binaries: %v", err)
	}
	return bin
}

// dumpDatabase writes one pg_dump of dsn to path, the way trackerbackup does.
func dumpDatabase(t *testing.T, bin, dsn, path string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(bin, "pg_dump"), "-Fc", "-f", path, "-d", dsn)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pg_dump: %v\n%s", err, out)
	}
}

// scratchDatabases lists the databases a check would have created, so a test
// can show the drop happened on a real cluster.
func scratchDatabases(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT datname FROM pg_database WHERE datname LIKE $1", scratchPrefix+"%")
	if err != nil {
		t.Fatalf("list databases: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan database name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list databases: %v", err)
	}
	return names
}

// A dump of a tracker database restores into a scratch database, the contract
// subset passes against the copy, and the copy is dropped (gt-p85pw.72).
func TestADumpRestoresAndReadsBack(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.New(t, "migrated")
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown')"); err != nil {
		t.Fatalf("insert rig: %v", err)
	}
	st := store.NewPostgres(pool)
	created, err := st.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: "restore me", Type: "task", Priority: 2})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	bin := pgBin(t)
	dsn := pool.Config().ConnString()
	dump := filepath.Join(t.TempDir(), "2026-10-28.dump")
	dumpDatabase(t, bin, dsn, dump)

	err = Check(ctx, Exec{}, Config{DSN: dsn, Dump: dump, PGBin: bin})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if left := scratchDatabases(t, ctx, pool); len(left) != 0 {
		t.Errorf("scratch databases left behind: %v, want none (issue %s)", left, created.ID)
	}
}

// A dump that restores but holds no issue fails the subset, and the copy is
// dropped anyway: a refused check leaves the cluster as it found it
// (gt-p85pw.72).
func TestTheCheckDropsTheCopyOfAnEmptyDump(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.New(t, "migrated")
	bin := pgBin(t)
	dsn := pool.Config().ConnString()
	dump := filepath.Join(t.TempDir(), "2026-10-28.dump")
	dumpDatabase(t, bin, dsn, dump)

	err := Check(ctx, Exec{}, Config{DSN: dsn, Dump: dump, PGBin: bin})
	if err == nil || !strings.Contains(err.Error(), "holds no issue") {
		t.Fatalf("Check = %v, want the subset to refuse an empty copy", err)
	}
	if left := scratchDatabases(t, ctx, pool); len(left) != 0 {
		t.Errorf("scratch databases left behind: %v, want none", left)
	}
}
