//go:build integration && !windows

package doltsquash

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // the driver the run's statements go over

	"github.com/steveyegge/gastown/internal/templates"
)

// The lab of 2026-11-15, run as a test (bead gt-dmxf57): a throwaway Dolt
// server on a scratch port and a scratch data directory, 400 commits over 14
// days, and one writer committing on its own connection at 25 commits a second
// while the run squashes.
//
// The server is this host's dolt binary, checked to be the version the town's
// Dolt server runs (internal/templates.DoltVersion). Nothing here reaches the
// town's server: the port and the data directory are the test's own, and both
// are gone when the test ends.

const (
	// labSeedCommits is the history the run squashes: 400 commits over
	// labHistoryDays, one row per commit appended to the table.
	labSeedCommits = 400
	// labHistoryDays is how far back the seeded history starts. The cut is
	// labKeepDays before the test's start, so part of the history is squashed
	// and part is kept.
	labHistoryDays = 14
	// labKeepDays is the keep_days setting the design defaults to.
	labKeepDays = 7
	// labSeedStep spaces the seeded commits so no two share an author date,
	// which is how verification tells one commit from another across the
	// rebase.
	labSeedStep = 3000 * time.Second
	// labWriterPace is one writer commit every 40ms: the 25 commits a second
	// the design measures the lab at.
	labWriterPace = 40 * time.Millisecond
	// labWriterCommits bounds the writer, so the catch-up sees a quiet tip and
	// the run finishes. A writer that never stops keeps main's tip moving, and
	// the catch-up has no tip to stop at.
	labWriterCommits = 150
	// labStartTimeout bounds one server start, and labStopTimeout one
	// shutdown. Both are deadlines a poll checks, not waits.
	labStartTimeout = time.Minute
	labStopTimeout  = 10 * time.Second
	// labPollInterval is how often a poll tries again.
	labPollInterval = 25 * time.Millisecond
	// labPingTimeout bounds one readiness probe.
	labPingTimeout = time.Second
)

// TestIntegrationLabSquash reproduces the lab end to end: it seeds a history,
// starts a writer beside it, runs the squash through Run, and checks the facts
// the design relies on — every id the writer committed is on the new main, the
// commit count falls to the one the squash implies, the table hashes are equal
// at the verified tip, and the throwaway server and its data directory are gone
// when the test ends.
func TestIntegrationLabSquash(t *testing.T) {
	requireLabDolt(t)
	srv := startLabServer(t)

	now := time.Now().UTC().Truncate(time.Second)
	cut := now.AddDate(0, 0, -labKeepDays)
	oldSeeds := seedLabHistory(t, srv.pin(t), now)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The writer commits on its own connection from before the run starts and
	// through it: the run records main while the writer is writing, the catch-up
	// picks what lands after the record, and anything left at the swap is the
	// reconcile's.
	writer := startLabWriter(t, srv.pin(t))
	result, err := Run(ctx, RunInput{
		Exec:    labExec{conn: srv.pin(t)},
		Marker:  &labMarker{},
		Clock:   labClock{},
		Sleeper: labSleeper{},
		Cut:     cut,
	})
	writer.stop(t)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Recovered != nil {
		t.Errorf("the run found a dead run's branches on a fresh database: %+v", result.Recovered)
	}
	if result.Swap.Conflict != nil {
		t.Errorf("the swap left a conflict on a history that only appends rows: %s", result.Swap.Conflict)
	}
	if !result.Verify.OK() {
		t.Errorf("compact-work did not verify against main at %s: %s",
			result.Verified.Tip, describeDifferences(result.Verify.Differences))
	}
	if !result.Summary.AfterRead {
		t.Errorf("the run reported no commit count after its swap")
	}
	t.Logf("run: before=%d after=%d tail=%d catchup picks=%d marked=%d recovered=%v",
		result.Summary.CommitsBefore, result.Summary.CommitsAfter, result.Summary.Tail,
		result.Catchup.Picks, result.Rebase.Marked, result.Recovered != nil)

	ids, err := writer.committed()
	if err != nil {
		t.Fatalf("the writer stopped early: %v", err)
	}
	if len(ids) == 0 {
		t.Fatalf("the writer committed no id, so the test proved nothing about concurrent writes")
	}
	// The writer's commits reached the squashed branch one of two ways: the
	// catch-up picked them, or they landed on the old main after the last picked
	// tip and the swap's reconcile replayed them. Either proves the writer ran
	// beside the run; neither means the test squashed a history nobody was
	// writing to.
	if result.Catchup.Picks+result.Summary.Tail == 0 {
		t.Errorf("the run picked and replayed no commit, so the writer never committed beside it")
	}

	// Every id the writer committed is on the new main. The writer records an id
	// only after its commit returned, so a missing id is history the running
	// squash dropped.
	onMain := labIssueIDs(t, srv.pin(t))
	for _, id := range ids {
		if !onMain[id] {
			t.Errorf("the writer committed %s and it is not on the new main", id)
		}
	}

	// The commit count falls as the squash says: the seeded commits at or before
	// the cut collapse into the one commit the base's branch keeps, the newer
	// seeded commits and every writer commit stay one for one, and the root and
	// the base stay as the history they are.
	after := labCount(t, srv.pin(t), "SELECT COUNT(*) FROM dolt_log('main')")
	wantAfter := int64(3) + int64(labSeedCommits-oldSeeds) + int64(len(ids))
	if after != wantAfter {
		t.Errorf("main holds %d commits after the squash, want %d: the root, the base, one commit for the %d seeded commits at or before the cut, the %d newer ones and the %d writer commits",
			after, wantAfter, oldSeeds, labSeedCommits-oldSeeds, len(ids))
	}
	if after >= result.Summary.CommitsBefore {
		t.Errorf("main holds %d commits after the squash and %d before, so the count did not fall",
			after, result.Summary.CommitsBefore)
	}
	old := labCount(t, srv.pin(t), fmt.Sprintf(
		"SELECT COUNT(*) FROM dolt_log('main') WHERE author_date <= %s AND commit_hash <> (SELECT hash FROM dolt_branches WHERE name = 'compact-base')",
		quoteLiteral(formatCut(cut))))
	if old != 1 {
		t.Errorf("%d commits at or before the cut are on main beyond compact-base, want exactly one: the squashed range", old)
	}

	// The table hashes at the verified tip are the record's, read back from the
	// history itself rather than taken from the run that wrote them.
	for _, table := range result.Verified.Tables {
		got := labTableHashAt(t, srv.pin(t), table.Table, result.Verified.Tip)
		if got != table.Hash {
			t.Errorf("table %s hashes %s at the verified tip %s, want the record's %s",
				table.Table, got, result.Verified.Tip, table.Hash)
		}
	}

	// The acceptance's last box: the throwaway server and its data directory are
	// gone. teardown is idempotent, so the cleanup registered at the start does
	// it again only when a check above failed first.
	srv.teardown(t)
}

// requireLabDolt refuses a host whose dolt is not the version the town's Dolt
// server runs. The lab stands in for that server, so a different Dolt would
// measure a different experiment.
func requireLabDolt(t *testing.T) {
	t.Helper()
	cmd := exec.Command("dolt", "version")
	cmd.Env = labDoltEnv(t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the lab runs the host's dolt binary: %v", err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[2] != templates.DoltVersion {
		t.Fatalf("the host's dolt reports %q, want version %s: the version the town's Dolt server runs",
			strings.TrimSpace(line), templates.DoltVersion)
	}
}

// labDoltEnv is os.Environ() with DOLT_ROOT_PATH pointed inside dir. Dolt keeps
// its global config and event log under DOLT_ROOT_PATH (HOME's ~/.dolt by
// default), so a dolt child started with this environment writes nothing outside
// the test's scratch tree.
func labDoltEnv(dir string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DOLT_ROOT_PATH=") {
			env = append(env, kv)
		}
	}
	return append(env, "DOLT_ROOT_PATH="+filepath.Join(dir, "dolt-root"))
}

// labServer is one throwaway Dolt server: the process, the data directory it
// serves, and the port it listens on.
type labServer struct {
	dataDir string
	logPath string
	port    int
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
	adminDB *sql.DB
	db      *sql.DB
	once    sync.Once
}

// startLabServer starts a dolt sql-server over a fresh data directory on a
// scratch port, with the database the lab squashes in it, and registers its
// teardown.
func startLabServer(t *testing.T) *labServer {
	t.Helper()
	for attempt := 1; attempt <= 3; attempt++ {
		srv, err := tryStartLabServer(t)
		if err == nil {
			return srv
		}
		t.Logf("start attempt %d: %v", attempt, err)
	}
	t.Fatalf("no Dolt server started after 3 attempts")
	return nil
}

// tryStartLabServer runs one start attempt, so a port another process took
// between the pick and the bind is retried instead of failing the test.
func tryStartLabServer(t *testing.T) (*labServer, error) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		return nil, fmt.Errorf("resolving the scratch directory: %w", err)
	}
	dataDir := filepath.Join(parent, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the scratch data directory: %w", err)
	}
	port, err := labFreePort()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(parent, "dolt-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("creating the server log: %w", err)
	}
	cmd := exec.Command("dolt", "sql-server",
		"--data-dir", dataDir,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port))
	cmd.Env = labDoltEnv(parent)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("starting dolt sql-server: %w", err)
	}
	srv := &labServer{
		dataDir: dataDir,
		logPath: logPath,
		port:    port,
		cmd:     cmd,
		done:    make(chan struct{}),
	}
	go func() {
		srv.waitErr = cmd.Wait()
		logFile.Close()
		close(srv.done)
	}()
	t.Cleanup(func() { srv.teardown(t) })

	dsn := func(database string) string {
		return fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s?timeout=10s&readTimeout=60s&writeTimeout=60s", port, database)
	}
	if srv.adminDB, err = sql.Open("mysql", dsn("")); err != nil {
		srv.teardown(t)
		return nil, fmt.Errorf("opening the admin connection: %w", err)
	}
	if srv.db, err = sql.Open("mysql", dsn("lab")); err != nil {
		srv.teardown(t)
		return nil, fmt.Errorf("opening the database connection: %w", err)
	}
	if err := srv.awaitReady(); err != nil {
		srv.teardown(t)
		return nil, err
	}
	if _, err := srv.adminDB.Exec("CREATE DATABASE lab"); err != nil {
		srv.teardown(t)
		return nil, fmt.Errorf("creating the lab database: %w", err)
	}
	return srv, nil
}

// awaitReady polls until the server answers a query, the process exits, or the
// deadline passes.
func (s *labServer) awaitReady() error {
	deadline := time.Now().Add(labStartTimeout)
	for {
		select {
		case <-s.done:
			return fmt.Errorf("dolt sql-server exited before it answered (wait: %v)\n%s", s.waitErr, s.logTail())
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), labPingTimeout)
		err := s.adminDB.PingContext(ctx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dolt sql-server did not answer within %s\n%s", labStartTimeout, s.logTail())
		}
		time.Sleep(labPollInterval)
	}
}

// pin returns a connection from db, closed when the test ends.
func (s *labServer) pin(t *testing.T) *sql.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), labStartTimeout)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("pinning a connection to the lab database: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// teardown stops the server, removes its data directory, and checks both: the
// process is gone, the port refuses a connection, and the directory no longer
// exists. It runs once, from the test's end or from the cleanup, whichever
// comes first.
func (s *labServer) teardown(t *testing.T) {
	t.Helper()
	s.once.Do(func() {
		for _, db := range []*sql.DB{s.db, s.adminDB} {
			if db != nil {
				db.Close()
			}
		}
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(os.Interrupt)
			select {
			case <-s.done:
			case <-time.After(labStopTimeout):
				_ = s.cmd.Process.Kill()
				<-s.done
			}
		}
		if err := os.RemoveAll(s.dataDir); err != nil {
			t.Errorf("removing the scratch data directory %s: %v", s.dataDir, err)
		}
		if _, err := os.Stat(s.dataDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the scratch data directory %s is still there after the teardown: %v", s.dataDir, err)
		}
		if err := labPortRefused(s.port); err != nil {
			t.Errorf("the throwaway server's port: %v", err)
		}
	})
}

// logTail returns the end of the server log for a failure message.
func (s *labServer) logTail() string {
	data, err := os.ReadFile(s.logPath)
	if err != nil {
		return fmt.Sprintf("(the server log could not be read: %v)", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}

// labFreePort picks a port nothing listens on. The server is started immediately
// afterwards, and a start that loses the race is retried.
func labFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("picking a scratch port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// labPortRefused reports that nothing accepts a connection on port, which is how
// a stopped server's port reads.
func labPortRefused(port int) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return nil
	}
	conn.Close()
	return fmt.Errorf("port %d still accepts connections after the server stopped", port)
}

// seedLabHistory writes labSeedCommits commits into the database, dated
// labSeedStep apart over labHistoryDays, and returns how many of them are at or
// before the cut labKeepDays back from now. Each commit appends one row, so the
// table's rows and its history move together.
func seedLabHistory(t *testing.T, conn *sql.Conn, now time.Time) int {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "CREATE TABLE issues (id VARCHAR(64) PRIMARY KEY, body VARCHAR(64))"); err != nil {
		t.Fatalf("creating the lab table: %v", err)
	}
	cut := now.AddDate(0, 0, -labKeepDays)
	start := now.AddDate(0, 0, -labHistoryDays)
	old := 0
	for i := 0; i < labSeedCommits; i++ {
		date := start.Add(time.Duration(i) * labSeedStep)
		id := fmt.Sprintf("seed-%03d", i)
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO issues VALUES ('%s', 'seed')", id)); err != nil {
			t.Fatalf("inserting %s: %v", id, err)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(
			"CALL DOLT_COMMIT('-Am', %s, '--author', 'Lab <lab@example.com>', '--date', %s)",
			quoteLiteral(fmt.Sprintf("seed %03d", i)), quoteLiteral(date.Format("2006-01-02T15:04:05")))); err != nil {
			t.Fatalf("committing %s: %v", id, err)
		}
		if !date.After(cut) {
			old++
		}
	}
	return old
}

// labWriter commits rows on its own connection while the run squashes, paced at
// the lab's 25 commits a second. An id is recorded only after its commit
// returned, so the record is exactly the ids that reached the history.
type labWriter struct {
	mu     sync.Mutex
	ids    []string
	err    error
	stopC  chan struct{}
	done   chan struct{}
	closed sync.Once
}

// startLabWriter starts a writer over conn. It commits labWriterCommits rows and
// stops, or stops earlier when the test asks.
func startLabWriter(t *testing.T, conn *sql.Conn) *labWriter {
	t.Helper()
	w := &labWriter{stopC: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ctx := context.Background()
		ticker := time.NewTicker(labWriterPace)
		defer ticker.Stop()
		for i := 0; i < labWriterCommits; i++ {
			select {
			case <-w.stopC:
				return
			case <-ticker.C:
			}
			id := fmt.Sprintf("writer-%04d", i)
			if err := labWrite(ctx, conn, id); err != nil {
				w.mu.Lock()
				w.err = fmt.Errorf("committing %s: %w", id, err)
				w.mu.Unlock()
				return
			}
			w.mu.Lock()
			w.ids = append(w.ids, id)
			w.mu.Unlock()
		}
	}()
	t.Cleanup(func() { w.stop(t) })
	return w
}

// labWrite inserts one row and commits it, retrying a write the swap's renames
// interrupted. The insert ignores a row an earlier attempt already wrote, and a
// commit that finds nothing to commit means that attempt's commit stands.
func labWrite(ctx context.Context, conn *sql.Conn, id string) error {
	insert := fmt.Sprintf("INSERT IGNORE INTO issues VALUES ('%s', 'writer')", id)
	commit := fmt.Sprintf("CALL DOLT_COMMIT('-Am', %s, '--author', 'Lab Writer <writer@example.com>')",
		quoteLiteral("writer "+id))
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := conn.ExecContext(ctx, insert); err != nil {
			last = err
			time.Sleep(labPollInterval)
			continue
		}
		if _, err := conn.ExecContext(ctx, commit); err != nil {
			if strings.Contains(err.Error(), "nothing to commit") {
				return nil
			}
			last = err
			time.Sleep(labPollInterval)
			continue
		}
		return nil
	}
	return last
}

// stop waits for the writer to finish. The writer's bound can end it without
// close(stopC), so the channel is closed at most once.
func (w *labWriter) stop(t *testing.T) {
	t.Helper()
	select {
	case <-w.done:
		return
	default:
	}
	w.closed.Do(func() { close(w.stopC) })
	select {
	case <-w.done:
	case <-time.After(labStopTimeout):
		t.Fatalf("the writer did not stop within %s", labStopTimeout)
	}
}

// committed is the ids the writer committed, in order.
func (w *labWriter) committed() ([]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return nil, w.err
	}
	return append([]string(nil), w.ids...), nil
}

// labExec runs the run's statements over one pinned connection, so the
// session's active branch and its rebase plan are the run's own.
type labExec struct {
	conn *sql.Conn
}

// Exec runs one statement and returns its first result set as strings.
func (e labExec) Exec(ctx context.Context, statement string) (Rows, error) {
	rs, err := e.conn.QueryContext(ctx, statement)
	if err != nil {
		return Rows{}, err
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return Rows{}, err
	}
	out := Rows{Columns: cols}
	values := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	for rs.Next() {
		if err := rs.Scan(ptrs...); err != nil {
			return Rows{}, err
		}
		record := make([]string, len(cols))
		for i, v := range values {
			if v.Valid {
				record[i] = v.String
			}
		}
		out.Data = append(out.Data, record)
	}
	if err := rs.Err(); err != nil {
		return Rows{}, err
	}
	return out, nil
}

// labMarker is the run marker for one run: it is never held by a second run in
// this test, so every Hold succeeds.
type labMarker struct {
	held bool
}

// Hold takes the marker, or fails the way a second run is refused.
func (m *labMarker) Hold() error {
	if m.held {
		return ErrRunMarkerHeld
	}
	m.held = true
	return nil
}

// Release gives the marker back.
func (m *labMarker) Release() error {
	m.held = false
	return nil
}

// labClock is the wall clock the run's quiet-tip rule reads.
type labClock struct{}

// Now is the real time.
func (labClock) Now() time.Time { return time.Now() }

// labSleeper waits in real time: the writer beside the run moves a real tip, so
// the window that sees it stand still is real too.
type labSleeper struct{}

// Sleep waits d.
func (labSleeper) Sleep(d time.Duration) { time.Sleep(d) }

// labIssueIDs returns the ids the issue table holds on main.
func labIssueIDs(t *testing.T, conn *sql.Conn) map[string]bool {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), "SELECT id FROM issues")
	if err != nil {
		t.Fatalf("reading the issue ids on main: %v", err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("reading an issue id: %v", err)
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the issue ids on main: %v", err)
	}
	return ids
}

// labCount runs a statement whose one value is a count.
func labCount(t *testing.T, conn *sql.Conn, statement string) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRowContext(context.Background(), statement).Scan(&n); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	return n
}

// labTableHashAt reads table's dolt_hashof_table at commit, by branching at the
// commit, reading the table's hash there, and dropping the branch.
func labTableHashAt(t *testing.T, conn *sql.Conn, table, commit string) string {
	t.Helper()
	ctx := context.Background()
	const branch = "lab-hash-check"
	for _, step := range []string{
		"CALL DOLT_BRANCH(" + quoteLiteral(branch) + ", " + quoteLiteral(commit) + ")",
		"CALL DOLT_CHECKOUT(" + quoteLiteral(branch) + ")",
	} {
		if _, err := conn.ExecContext(ctx, step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	var hash string
	if err := conn.QueryRowContext(ctx, fmt.Sprintf("SELECT dolt_hashof_table(%s)", quoteLiteral(table))).Scan(&hash); err != nil {
		t.Fatalf("reading %s's hash at %s: %v", table, commit, err)
	}
	for _, step := range []string{
		"CALL DOLT_CHECKOUT('main')",
		"CALL DOLT_BRANCH('-D', " + quoteLiteral(branch) + ")",
	} {
		if _, err := conn.ExecContext(ctx, step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	return hash
}
