//go:build integration

package pgtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/internal/leakcheck"
)

// Bounds on every wait this package makes. The measured cost of each step is
// well under a second (initdb 0.6 s, ready 0.2 s, clone 0.1 s); these are the
// hang detectors, not budgets.
const (
	initdbTimeout = 60 * time.Second
	readyTimeout  = 30 * time.Second
	stopTimeout   = 15 * time.Second
	opTimeout     = 30 * time.Second
)

// readyLine is what postgres logs once it accepts connections.
const readyLine = "database system is ready to accept connections"

// maxSocketDir is the longest directory a unix socket can live in: the path
// plus ".s.PGSQL.5432" must fit the 104-byte sockaddr_un limit on macOS.
const maxSocketDir = 104 - len("/.s.PGSQL.5432") - 1

// Cluster is one private PostgreSQL server on a unix socket.
type Cluster struct {
	dir     string
	cmd     *exec.Cmd
	log     *lineWatcher
	done    chan struct{} // closed when postgres has exited
	waitErr error         // cmd.Wait's result; valid once done is closed
	admin   *pgxpool.Pool
	seq     atomic.Int64

	stopOnce sync.Once
	stopErr  error
}

// lineWatcher is the postmaster's stderr: it keeps the output for error
// messages and closes ready when the ready line appears. Using cmd.Stderr
// (not StderrPipe) lets Wait own, and finish, the copying goroutine.
type lineWatcher struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *lineWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	seen := strings.Contains(w.buf.String(), readyLine)
	w.mu.Unlock()
	if seen {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func (w *lineWatcher) tail() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.buf.String()
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return s
}

// Start initializes and starts a cluster in a fresh temp directory and
// returns once it accepts connections. Call Stop when done.
func Start(ctx context.Context) (*Cluster, error) {
	bin, err := Locate(os.Getenv, fileExists, exec.LookPath)
	if err != nil {
		return nil, err
	}
	versionCmd := exec.CommandContext(ctx, filepath.Join(bin, "postgres"), "--version") //nolint:gosec // G204: bin is $TRACKER_PG_BIN, PATH or a fixed install directory, chosen by whoever runs the tests
	versionCmd.Env = childEnv(os.Environ())
	ver, err := versionCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pgtest: postgres --version: %w", err)
	}
	if err := requireMajor17(string(ver)); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "trkpg")
	if err != nil {
		return nil, fmt.Errorf("pgtest: temp dir: %w", err)
	}
	if len(dir) > maxSocketDir {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("pgtest: temp dir %q is too long for a unix socket; set TMPDIR to a shorter path", dir)
	}
	c := &Cluster{dir: dir, done: make(chan struct{}), log: &lineWatcher{ready: make(chan struct{})}}
	if err := c.start(ctx, bin); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return c, nil
}

func (c *Cluster) start(ctx context.Context, bin string) error {
	data := filepath.Join(c.dir, "data")
	initCtx, cancel := context.WithTimeout(ctx, initdbTimeout)
	defer cancel()
	//nolint:gosec // G204: bin is $TRACKER_PG_BIN, PATH or a fixed install directory, chosen by whoever runs the tests
	initdb := exec.CommandContext(initCtx, filepath.Join(bin, "initdb"), "-D", data, "-U", "postgres", "-A", "trust", "--no-sync", "-E", "UTF8", "--locale=C")
	initdb.Env = childEnv(os.Environ())
	if out, err := initdb.CombinedOutput(); err != nil {
		return fmt.Errorf("pgtest: initdb: %w\n%s", err, out)
	}
	//nolint:gosec // G204: bin is $TRACKER_PG_BIN, PATH or a fixed install directory, chosen by whoever runs the tests
	c.cmd = exec.Command(filepath.Join(bin, "postgres"), "-D", data,
		"-c", "listen_addresses=", "-c", "unix_socket_directories="+c.dir,
		"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off",
		"-c", "shared_buffers=64MB", "-c", "max_connections=100",
		// The ready line is matched as English text at LOG level.
		"-c", "lc_messages=C", "-c", "log_min_messages=warning")
	c.cmd.Env = childEnv(os.Environ())
	c.cmd.Stderr = c.log
	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("pgtest: start postgres: %w", err)
	}
	go func() {
		c.waitErr = c.cmd.Wait()
		close(c.done)
	}()

	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()
	select {
	case <-c.log.ready:
	case <-c.done:
		return fmt.Errorf("pgtest: postgres exited before it was ready: %v\n%s", c.waitErr, c.log.tail())
	case <-timer.C:
		_ = c.cmd.Process.Kill()
		<-c.done
		return fmt.Errorf("pgtest: postgres not ready after %v\n%s", readyTimeout, c.log.tail())
	case <-ctx.Done():
		_ = c.cmd.Process.Kill()
		<-c.done
		return ctx.Err()
	}
	admin, err := pgxpool.New(ctx, c.DSN("postgres"))
	if err != nil {
		_ = c.Stop()
		return fmt.Errorf("pgtest: admin pool: %w", err)
	}
	c.admin = admin
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// DSN is a connection string for database db on this cluster.
func (c *Cluster) DSN(db string) string {
	return fmt.Sprintf("host=%s user=postgres dbname=%s sslmode=disable", c.dir, db)
}

// Stop shuts the server down (a fast shutdown, then a kill after stopTimeout),
// waits for it to exit, and removes its directory. It is safe to call more
// than once; later calls return the first call's result.
func (c *Cluster) Stop() error {
	c.stopOnce.Do(func() { c.stopErr = c.stop() })
	return c.stopErr
}

func (c *Cluster) stop() error {
	if c.admin != nil {
		c.admin.Close()
		c.admin = nil
	}
	var stopErr error
	_ = c.cmd.Process.Signal(syscall.SIGINT)
	timer := time.NewTimer(stopTimeout)
	defer timer.Stop()
	select {
	case <-c.done:
		if err := c.waitErr; err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 0 {
				stopErr = fmt.Errorf("pgtest: postgres exited uncleanly: %v\n%s", err, c.log.tail())
			}
		}
	case <-timer.C:
		_ = c.cmd.Process.Kill()
		<-c.done
		stopErr = fmt.Errorf("pgtest: postgres did not stop within %v and was killed\n%s", stopTimeout, c.log.tail())
	}
	if err := os.RemoveAll(c.dir); err != nil && stopErr == nil {
		stopErr = fmt.Errorf("pgtest: remove %s: %w", c.dir, err)
	}
	return stopErr
}

// Template creates database name, runs setup against it, and marks it a
// template. setup must close any connection it opens before returning:
// CREATE DATABASE ... TEMPLATE needs the template idle.
func (c *Cluster) Template(ctx context.Context, name string, setup func(context.Context, *pgxpool.Pool) error) error {
	if _, err := c.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("pgtest: create template %s: %w", name, err)
	}
	pool, err := pgxpool.New(ctx, c.DSN(name))
	if err != nil {
		return fmt.Errorf("pgtest: connect to template %s: %w", name, err)
	}
	err = setup(ctx, pool)
	pool.Close()
	if err != nil {
		return fmt.Errorf("pgtest: set up template %s: %w", name, err)
	}
	if _, err := c.admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH IS_TEMPLATE true"); err != nil {
		return fmt.Errorf("pgtest: mark %s a template: %w", name, err)
	}
	return nil
}

// Clone creates a database from template and returns a pool on it. The pool
// is closed and the database dropped when t ends.
func (c *Cluster) Clone(t testing.TB, template string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	name := fmt.Sprintf("t%d", c.seq.Add(1))
	if _, err := c.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{template}.Sanitize()); err != nil {
		t.Fatalf("pgtest: clone %s from %s: %v", name, template, err)
	}
	pool, err := pgxpool.New(ctx, c.DSN(name))
	if err != nil {
		t.Fatalf("pgtest: connect to %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		dctx, dcancel := context.WithTimeout(context.Background(), opTimeout)
		defer dcancel()
		if _, err := c.admin.Exec(dctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("pgtest: drop %s: %v", name, err)
		}
	})
	return pool
}

// Databases lists the cluster's non-template databases, for tests that check
// clones are dropped.
func (c *Cluster) Databases(ctx context.Context) ([]string, error) {
	rows, err := c.admin.Query(ctx, "SELECT datname FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres' ORDER BY 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

var shared *Cluster

// Main is a package's TestMain body: it starts the shared cluster, builds the
// templates with setup, runs the tests under the goroutine-leak check, stops
// the cluster, and returns the exit code for os.Exit.
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Main(m, setup)) }
func Main(m *testing.M, setup func(context.Context, *Cluster) error) int {
	ctx := context.Background()
	c, err := Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	shared = c
	code := 1
	if err := setup(ctx, c); err != nil {
		fmt.Fprintln(os.Stderr, err)
	} else {
		code = leakcheck.Main(m)
	}
	if err := c.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	shared = nil
	return code
}

// New returns a pool on a fresh clone of template from the package's shared
// cluster, which Main started.
func New(t testing.TB, template string) *pgxpool.Pool {
	t.Helper()
	if shared == nil {
		t.Fatal("pgtest.New: no shared cluster; the package's TestMain must run pgtest.Main")
	}
	return shared.Clone(t, template)
}
