//go:build integration

package pgtest

import (
	"context"
	"errors"
	"os"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) {
	os.Exit(Main(m, func(ctx context.Context, c *Cluster) error {
		return c.Template(ctx, "tpl", func(ctx context.Context, p *pgxpool.Pool) error {
			_, err := p.Exec(ctx, "CREATE TABLE t(id int PRIMARY KEY, v text); INSERT INTO t VALUES (0, 'seed')")
			return err
		})
	}))
}

func count(t *testing.T, p *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := p.QueryRow(context.Background(), "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Integration tier: a real postgres, ~100 ms per case.
func TestCloneStartsFromTheTemplateAndIsIsolated(t *testing.T) {
	t.Parallel()
	a, b := New(t, "tpl"), New(t, "tpl")
	if got := count(t, a); got != 1 {
		t.Fatalf("fresh clone has %d rows, want the template's 1", got)
	}
	if _, err := a.Exec(context.Background(), "INSERT INTO t VALUES (1, 'only in a')"); err != nil {
		t.Fatal(err)
	}
	if got := count(t, a); got != 2 {
		t.Errorf("clone a has %d rows, want 2 after its own insert", got)
	}
	if got := count(t, b); got != 1 {
		t.Errorf("clone b has %d rows, want 1: a write to a clone must not reach another", got)
	}
}

func TestCloneIsDroppedWhenItsTestEnds(t *testing.T) {
	t.Parallel()
	var name string
	t.Run("inner", func(t *testing.T) {
		name = New(t, "tpl").Config().ConnConfig.Database
	})
	dbs, err := shared.Databases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(dbs, name) {
		t.Errorf("database %s still exists after its test ended; databases: %v", name, dbs)
	}
}

func TestStopLeavesNoProcessAndNoDirectory(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pid, dir := c.cmd.Process.Pid, c.dir
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("postgres pid %d still exists after Stop (kill 0: %v)", pid, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cluster dir %s still exists after Stop (stat: %v)", dir, err)
	}
}

// BenchmarkClone is how the per-clone cost is measured, outside the test run
// (go test -tags integration -bench Clone ./store/pgtest). It is not an
// assertion: a wall-clock bound in a test depends on host load. Measured
// 2026-10-25 with postgresql@17 17.11 on the dev Mac: about 70 ms per clone.
func BenchmarkClone(b *testing.B) {
	for i := 0; i < b.N; i++ {
		shared.Clone(b, "tpl")
	}
}
