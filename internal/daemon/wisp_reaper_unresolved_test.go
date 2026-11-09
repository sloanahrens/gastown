package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/reaper"
)

// TestWispReaperUnresolvedDatabaseStaysLoud drives two cycles over one
// database the town names and one no beads directory names. The operator keeps
// the no-beads-directory error loud — it is how unexpected databases such as
// probe, rdp and zz were noticed — so the line is logged on every cycle rather
// than latched or skipped, the unresolved database is never given a writer, and
// the other database in the same cycle is still reaped (gt-33etw).
func TestWispReaperUnresolvedDatabaseStaysLoud(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)
	// The route table reaches beads/mayor/rig; naming beads_db there is what
	// makes it resolvable, so ghost_db is the only database with no writer.
	beadsRig := filepath.Join(p.d.config.TownRoot, "beads", "mayor", "rig", ".beads")
	if err := os.MkdirAll(beadsRig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsRig, "metadata.json"), []byte(`{"dolt_database":"beads_db"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p.d.reaperOpenDBFn = func(string, time.Duration, time.Duration) (*beadsql.DB, error) {
		return openEmptySweepDB(t), nil
	}
	// A database the sweep can write to resolves a bd writer; ghost_db resolves
	// none, which is the no-beads-directory failure under test. Recording the
	// production resolution watches for a sweep that stopped before reaching
	// the resolvable database without reading either database's query text.
	var swept []string
	p.d.reaperWriterForFn = func(townRoot, dbName string) (reaper.Writer, error) {
		w, err := reaper.WriterForDatabase(townRoot, dbName)
		if err != nil {
			return nil, err
		}
		swept = append(swept, dbName)
		return w, nil
	}

	// The unresolved database is swept first, so a pass that gave up at the
	// first failure would never reach beads_db.
	var cycleLogs []string
	for i := 0; i < 2; i++ {
		start := p.log.Len()
		p.d.reapWispsInline(&WispReaperConfig{Databases: []string{"ghost_db", "beads_db"}}, 0, 0, 0,
			p.d.startDogCycle("wisp_reaper"))
		// What the log grew by while this cycle ran is this cycle's section.
		cycleLogs = append(cycleLogs, p.log.String()[start:])
	}

	for i, cycleLog := range cycleLogs {
		if !strings.Contains(cycleLog, "no beads directory") || !strings.Contains(cycleLog, "ghost_db") {
			t.Errorf("cycle %d log = %q, want the no-beads-directory error for the unresolved database", i+1, cycleLog)
		}
		if strings.Contains(cycleLog, "beads_db: reap error") || strings.Contains(cycleLog, "beads_db: connect error") {
			t.Errorf("cycle %d log = %q, want the resolvable database reaped without an error", i+1, cycleLog)
		}
	}
	if !slices.Contains(swept, "beads_db") {
		t.Errorf("the unresolvable database stopped the sweep before beads_db was reached: swept %v", swept)
	}
}

// TestWispReaperRefusesAnUnusableExtraBeadsRoot: a set but relative
// extra_beads_roots fails the scan step and sweeps nothing, rather than being
// read as "search the town only" and silently leaving the databases the
// setting was added for unresolved (gt-33etw).
func TestWispReaperRefusesAnUnusableExtraBeadsRoot(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)
	opened := false
	p.d.reaperOpenDBFn = func(string, time.Duration, time.Duration) (*beadsql.DB, error) {
		opened = true
		return openEmptySweepDB(t), nil
	}

	p.d.reapWispsInline(&WispReaperConfig{
		Databases:       []string{"beads_db"},
		ExtraBeadsRoots: []string{"~/.claude"},
	}, 0, 0, 0, p.cycle)

	if s := p.scanStep(t); s.result != dogStepFailed {
		t.Errorf("scan step = %+v, want it failed on the unusable extra root", s)
	}
	if !strings.Contains(p.log.String(), "extra_beads_roots") {
		t.Errorf("log = %q, want the refusal to name the setting", p.log.String())
	}
	if opened {
		t.Error("the pass opened a database after refusing its extra_beads_roots")
	}
}

// TestWispReaperSkipsMissingExtraRoot: an absolute extra_beads_roots entry whose
// directory is absent on this host does not fail the scan step, and the database
// the town names is still reaped without an error. A pod that does not mount the
// host's claude beads directory relies on this (gt-nr548n).
func TestWispReaperSkipsMissingExtraRoot(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)
	beadsRig := filepath.Join(p.d.config.TownRoot, "beads", "mayor", "rig", ".beads")
	if err := os.MkdirAll(beadsRig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsRig, "metadata.json"), []byte(`{"dolt_database":"beads_db"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "claude")
	p.d.reaperOpenDBFn = func(string, time.Duration, time.Duration) (*beadsql.DB, error) {
		return openEmptySweepDB(t), nil
	}

	p.d.reapWispsInline(&WispReaperConfig{
		Databases:       []string{"beads_db"},
		ExtraBeadsRoots: []string{missing},
	}, 0, 0, 0, p.cycle)

	if s := p.scanStep(t); s.result != dogStepDone {
		t.Errorf("scan step = %+v, want done: a missing extra root is not a refusal", s)
	}
	if strings.Contains(p.log.String(), "beads_db: reap error") {
		t.Errorf("log = %q, want the town's database reaped without a reap error", p.log.String())
	}
}

// openEmptySweepDB returns a database handle that answers the questions
// HasReaperSchema asks the way a beads database does and returns no rows for
// everything else, so a whole sweep runs over it without a Dolt server. It
// refuses every Exec: the reaper reads with SQL and writes only through bd.
func openEmptySweepDB(t *testing.T) *beadsql.DB {
	t.Helper()
	driverName := fmt.Sprintf("fake_empty_sweep_%d", atomic.AddUint64(&emptySweepDriverID, 1))
	sql.Register(driverName, &emptySweepDriver{})
	raw, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open empty sweep db: %v", err)
	}
	db, err := beadsql.New(context.Background(), raw, "beads_db")
	if err != nil {
		t.Fatalf("open empty sweep db: %v", err)
	}
	return db
}

var emptySweepDriverID uint64

type emptySweepDriver struct{}

func (*emptySweepDriver) Open(string) (driver.Conn, error) {
	return emptySweepConn{}, nil
}

type emptySweepConn struct{}

func (emptySweepConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepare not implemented")
}
func (emptySweepConn) Close() error              { return nil }
func (emptySweepConn) Begin() (driver.Tx, error) { return emptySweepTx{}, nil }

func (emptySweepConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (emptySweepConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	normalized := strings.Join(strings.Fields(strings.ToUpper(query)), " ")
	switch {
	case query == beadsql.SchemaLevelQuery:
		return &countSweepRows{n: int64(beadsql.SchemaVersion)}, nil
	// The schema gate asks whether all three tables exist and whether each
	// table has its three expected columns; 3 answers both the "at least"
	// form and the "all of them" form.
	case strings.Contains(normalized, "INFORMATION_SCHEMA.TABLES"), strings.Contains(normalized, "INFORMATION_SCHEMA.COLUMNS"):
		return &countSweepRows{n: 3}, nil
	case strings.Contains(normalized, "SELECT COUNT("):
		return &countSweepRows{n: 0}, nil
	}
	return &emptySweepRows{}, nil
}

func (emptySweepConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	return nil, fmt.Errorf("the reaper must write only through bd, but issued: %s", query)
}

type emptySweepTx struct{}

func (emptySweepTx) Commit() error   { return nil }
func (emptySweepTx) Rollback() error { return nil }

// countSweepRows answers a one-column COUNT-style query with n.
type countSweepRows struct {
	n    int64
	done bool
}

func (r *countSweepRows) Columns() []string { return []string{"n"} }
func (r *countSweepRows) Close() error      { return nil }
func (r *countSweepRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.n
	return nil
}

type emptySweepRows struct{}

func (r *emptySweepRows) Columns() []string         { return []string{"id"} }
func (r *emptySweepRows) Close() error              { return nil }
func (r *emptySweepRows) Next([]driver.Value) error { return io.EOF }
