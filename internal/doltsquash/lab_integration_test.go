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
	"syscall"
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
	// labStartAttempts bounds one start's port race: the port is picked before
	// the start runs, so anything on the host can take it in between.
	labStartAttempts = 3
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
	// dial and removeAll are the seams a test drives the teardown's checks
	// through. nil means the real net.DialTimeout and os.RemoveAll.
	dial      labDialer
	removeAll func(path string) error
}

// startLabServer starts a dolt sql-server over a fresh data directory on a
// scratch port, with the database the lab squashes in it, and registers its
// teardown.
func startLabServer(t *testing.T) *labServer {
	t.Helper()
	srv, err := labStartOnFreePort(labFreePort, func(port int) (*labServer, error) {
		return tryStartLabServer(t, port)
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// labPortRaceErr reports whether err is dolt refusing the port it was handed,
// rather than failing for a reason a retry cannot help. Dolt writes "Port 49575
// already in use." to its log, and the readiness poll carries the log's tail, so
// the race reads through the start error (gt-8dbyw).
func labPortRaceErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already in use")
}

// labStartOnFreePort starts a server on a port nextPort picks. A start refused
// because that port is already in use is retried on a fresh port, at most
// labStartAttempts times; any other failure comes back at once, and so does the
// last port race once the attempts run out.
//
// nextPort and start are parameters rather than the real pick and server so the
// retry is drivable: a test hands in the ports and the failures it wants,
// without a dolt binary.
func labStartOnFreePort(nextPort func() (int, error), start func(port int) (*labServer, error)) (*labServer, error) {
	var lastErr error
	for attempt := 1; attempt <= labStartAttempts; attempt++ {
		port, err := nextPort()
		if err != nil {
			return nil, err
		}
		var srv *labServer
		srv, lastErr = start(port)
		if lastErr == nil {
			return srv, nil
		}
		if !labPortRaceErr(lastErr) {
			return nil, lastErr
		}
	}
	return nil, fmt.Errorf("no Dolt server started after %d attempts: %w", labStartAttempts, lastErr)
}

// tryStartLabServer runs one start attempt on port. Its caller retries a port
// another process took between the pick and the bind on a fresh one, rather than
// failing the test.
func tryStartLabServer(t *testing.T, port int) (*labServer, error) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		return nil, fmt.Errorf("resolving the scratch directory: %w", err)
	}
	dataDir := filepath.Join(parent, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the scratch data directory: %w", err)
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

// labReporter is the part of *testing.T a teardown reports a failed check
// through, so a test can drive a failure branch and read what it reported.
type labReporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// labDialer is the dial a teardown's port check makes: the real net.DialTimeout,
// or a test's.
type labDialer func(network, address string, timeout time.Duration) (net.Conn, error)

// teardown stops the server, removes its data directory, and checks both: the
// process is gone, the port refuses a connection, and the directory no longer
// exists. It runs once, from the test's end or from the cleanup, whichever
// comes first.
func (s *labServer) teardown(t labReporter) {
	t.Helper()
	s.once.Do(func() {
		for _, db := range []*sql.DB{s.db, s.adminDB} {
			if db != nil {
				db.Close()
			}
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(os.Interrupt)
			select {
			case <-s.done:
			case <-time.After(labStopTimeout):
				_ = s.cmd.Process.Kill()
				<-s.done
			}
		}
		if err := s.remove(); err != nil {
			t.Errorf("removing the scratch data directory %s: %v", s.dataDir, err)
		}
		if _, err := os.Stat(s.dataDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the scratch data directory %s is still there after the teardown: %v", s.dataDir, err)
		}
		if err := s.portRefused(); err != nil {
			t.Errorf("the throwaway server's port: %v", err)
		}
	})
}

// remove removes the server's data directory, through the test's seam when it
// set one.
func (s *labServer) remove() error {
	if s.removeAll != nil {
		return s.removeAll(s.dataDir)
	}
	return os.RemoveAll(s.dataDir)
}

// portRefused checks the stopped server's port, through the test's dialer when
// it set one.
func (s *labServer) portRefused() error {
	dial := s.dial
	if dial == nil {
		dial = net.DialTimeout
	}
	return labPortRefused(dial, s.port)
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
// a stopped server's port reads. Only a refused connection reads that way: a
// dial that failed for another reason — a timeout, a host with no route, a
// process out of descriptors — leaves the port unread, and the check reports
// that rather than passing on a port it never saw refuse.
func labPortRefused(dial labDialer, port int) error {
	conn, err := dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil
		}
		return fmt.Errorf("checking that port %d is refused: %w", port, err)
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

// The failure branches below report through a labReporter, so a test drives one
// and reads what it reported instead of failing itself.

// labRecorder is a labReporter that keeps what a teardown reported.
type labRecorder struct {
	reported []string
}

// Helper does nothing: the recorder is not a testing.T.
func (*labRecorder) Helper() {}

// Errorf records one reported failure.
func (r *labRecorder) Errorf(format string, args ...any) {
	r.reported = append(r.reported, fmt.Sprintf(format, args...))
}

// count returns how many reported messages hold want.
func (r *labRecorder) count(want string) int {
	n := 0
	for _, msg := range r.reported {
		if strings.Contains(msg, want) {
			n++
		}
	}
	return n
}

// refusedDialer is the dial a port nothing listens on makes: the kernel refuses
// the connection.
func refusedDialer(string, string, time.Duration) (net.Conn, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

// cleanServer is a server whose teardown has nothing to report: a data
// directory that is removed, and a port that refuses.
func cleanServer(t *testing.T) *labServer {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("creating the scratch data directory: %v", err)
	}
	return &labServer{dataDir: dataDir, dial: refusedDialer}
}

// A teardown of a server that stopped and left nothing behind reports nothing.
// The rows below each change one thing, so this row is what says a report means
// a failure rather than every teardown reporting.
func TestIntegrationLabTeardownReportsNothingOnACleanServer(t *testing.T) {
	rec := &labRecorder{}
	cleanServer(t).teardown(rec)
	if len(rec.reported) != 0 {
		t.Errorf("the teardown reported %q, want nothing", rec.reported)
	}
}

// A data directory the removal could not remove is reported: a teardown that
// passed would hide the scratch tree the check exists for.
func TestIntegrationLabTeardownReportsADirectoryItCouldNotRemove(t *testing.T) {
	srv := cleanServer(t)
	srv.removeAll = func(string) error { return errors.New("permission denied") }
	rec := &labRecorder{}
	srv.teardown(rec)
	if got := rec.count("removing the scratch data directory"); got != 1 {
		t.Errorf("the teardown reported %q, want one message about the directory it could not remove", rec.reported)
	}
}

// A removal that returns without removing is reported: RemoveAll's nil is not
// evidence the directory is gone, so the check reads the filesystem itself.
func TestIntegrationLabTeardownReportsADirectoryStillThere(t *testing.T) {
	srv := cleanServer(t)
	srv.removeAll = func(string) error { return nil }
	rec := &labRecorder{}
	srv.teardown(rec)
	if got := rec.count("is still there after the teardown"); got != 1 {
		t.Errorf("the teardown reported %q, want one message that the directory survived its removal", rec.reported)
	}
	if got := rec.count("removing the scratch data directory"); got != 0 {
		t.Errorf("the teardown reported a removal failure it did not have: %q", rec.reported)
	}
}

// A port that still accepts a connection is reported: the stopped server's port
// must refuse, or the throwaway server outlived the test.
func TestIntegrationLabTeardownReportsAPortStillAccepting(t *testing.T) {
	srv := cleanServer(t)
	conn, peer := net.Pipe()
	defer peer.Close()
	srv.dial = func(string, string, time.Duration) (net.Conn, error) { return conn, nil }
	rec := &labRecorder{}
	srv.teardown(rec)
	if got := rec.count("still accepts connections after the server stopped"); got != 1 {
		t.Errorf("the teardown reported %q, want one message that the port still accepts", rec.reported)
	}
}

// The port check passes only on a refused connection. A dial that failed for
// another reason leaves the port unread, and the teardown reports that with the
// cause rather than reading it as a stopped server (om finding 23f23127b898).
func TestIntegrationLabTeardownReportsAnUnansweredPortCheck(t *testing.T) {
	srv := cleanServer(t)
	unanswered := errors.New("too many open files")
	srv.dial = func(string, string, time.Duration) (net.Conn, error) { return nil, unanswered }
	rec := &labRecorder{}
	srv.teardown(rec)
	if got := rec.count("checking that port"); got != 1 {
		t.Errorf("the teardown reported %q, want one message that the port check went unanswered", rec.reported)
	}
	if got := rec.count(unanswered.Error()); got != 1 {
		t.Errorf("the report does not carry the dial's cause %q: %q", unanswered, rec.reported)
	}
	if got := rec.count("still accepts connections"); got != 0 {
		t.Errorf("an unanswered dial was read as an accepting port: %q", rec.reported)
	}
}

// A start refused for the port it was handed comes back on a fresh one, and the
// server it returns is the one that started.
func TestIntegrationLabStartOnFreePortRetriesOnPortRace(t *testing.T) {
	var tried []int
	picks := 0
	srv, err := labStartOnFreePort(
		func() (int, error) { picks++; return 41000 + picks, nil },
		func(port int) (*labServer, error) {
			tried = append(tried, port)
			if len(tried) == 1 {
				return nil, errors.New("dolt sql-server exited before it answered (wait: exit status 1)\nPort 41001 already in use.")
			}
			return &labServer{port: port}, nil
		})
	if err != nil {
		t.Fatalf("labStartOnFreePort: %v", err)
	}
	if len(tried) != 2 {
		t.Fatalf("start attempts on ports %v, want the refused port and a fresh one", tried)
	}
	if tried[0] == tried[1] {
		t.Errorf("the retry reused port %d", tried[1])
	}
	if srv == nil || srv.port != tried[1] {
		t.Errorf("the returned server is %+v, want the one that started on port %d", srv, tried[1])
	}
}

// A start that failed for another reason is reported at once: a retry would
// spend two more ports on a host that is not handing ports out twice, and hide
// the first failure behind the last.
func TestIntegrationLabStartOnFreePortReportsOtherFailuresWithoutRetrying(t *testing.T) {
	want := errors.New("starting dolt sql-server: executable file not found in $PATH")
	picks, calls := 0, 0
	srv, err := labStartOnFreePort(
		func() (int, error) { picks++; return 41000, nil },
		func(int) (*labServer, error) { calls++; return nil, want })
	if srv != nil {
		t.Errorf("server = %+v, want nil when no start succeeded", srv)
	}
	if !errors.Is(err, want) {
		t.Errorf("error = %v, want %v", err, want)
	}
	if picks != 1 || calls != 1 {
		t.Errorf("picked %d ports and started %d times, want one of each", picks, calls)
	}
}

// A picker that cannot pick a port is reported, not retried: there is no port to
// start on, so another attempt would fail the same way.
func TestIntegrationLabStartOnFreePortReportsAPortItCouldNotPick(t *testing.T) {
	want := errors.New("picking a scratch port: address already in use")
	picks := 0
	srv, err := labStartOnFreePort(
		func() (int, error) { picks++; return 0, want },
		func(int) (*labServer, error) {
			t.Error("a start ran without a port to run on")
			return nil, nil
		})
	if srv != nil {
		t.Errorf("server = %+v, want nil when no port was picked", srv)
	}
	if !errors.Is(err, want) {
		t.Errorf("error = %v, want %v", err, want)
	}
	if picks != 1 {
		t.Errorf("picked %d ports, want one attempt", picks)
	}
}

// The retry runs out: every attempt gets a fresh port, and the last failure is
// what comes back rather than a bare attempt count.
func TestIntegrationLabStartOnFreePortGivesUpAfterThreeStarts(t *testing.T) {
	race := errors.New("Port 41000 already in use.")
	picks, calls := 0, 0
	srv, err := labStartOnFreePort(
		func() (int, error) { picks++; return 41000 + picks, nil },
		func(int) (*labServer, error) { calls++; return nil, race })
	if srv != nil {
		t.Errorf("server = %+v, want nil when no start succeeded", srv)
	}
	if !errors.Is(err, race) {
		t.Errorf("error = %v, want the last port race %v", err, race)
	}
	if picks != labStartAttempts || calls != labStartAttempts {
		t.Errorf("picked %d ports and started %d times, want %d of each", picks, calls, labStartAttempts)
	}
}
