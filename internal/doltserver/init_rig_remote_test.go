package doltserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	configpkg "github.com/steveyegge/gastown/internal/config"
)

// remoteFakeServer answers the two statements initRemoteRig issues, over the
// Go MySQL driver's interface, so the remote path runs with no server.
type remoteFakeServer struct {
	mu         sync.Mutex
	databases  []string
	statements []string
	opens      int
	openErr    error
	// failOpenAfter fails every open after the first failOpenAfter ones, so a
	// test can let the existence read through and refuse the create.
	failOpenAfter int
	queryErr      error
	execErr       error
}

// open is the host's openDB seam for server: the connector it hands back
// answers this server alone, so parallel tests share no registry.
func (s *remoteFakeServer) open(string) (*sql.DB, error) {
	s.mu.Lock()
	s.opens++
	err := s.openErr
	if s.failOpenAfter > 0 && s.opens > s.failOpenAfter {
		err = errors.New("dial tcp: connect: connection refused")
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(remoteFakeConnector{server: s}), nil
}

func (s *remoteFakeServer) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.statements...)
}

func (s *remoteFakeServer) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

// remoteFakeConnector is database/sql's way in without a driver name: a
// test's pool is built from its own server and never from a package registry.
type remoteFakeConnector struct{ server *remoteFakeServer }

func (c remoteFakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &remoteFakeConn{server: c.server}, nil
}

func (remoteFakeConnector) Driver() driver.Driver { return remoteFakeDriverType{} }

type remoteFakeDriverType struct{}

func (remoteFakeDriverType) Open(string) (driver.Conn, error) {
	return nil, errors.New("the fake connects through remoteFakeConnector, not a driver name")
}

type remoteFakeConn struct{ server *remoteFakeServer }

func (c *remoteFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not used by initRemoteRig")
}

func (c *remoteFakeConn) Close() error { return nil }

func (c *remoteFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used by initRemoteRig")
}

func (c *remoteFakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	s := c.server
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statements = append(s.statements, query)
	if s.execErr != nil {
		return nil, s.execErr
	}
	return driver.RowsAffected(1), nil
}

func (c *remoteFakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	s := c.server
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statements = append(s.statements, query)
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	name, _ := args[0].Value.(string)
	listed := int64(0)
	for _, db := range s.databases {
		if db == name {
			listed = 1
		}
	}
	return &remoteFakeRows{values: [][]driver.Value{{listed}}}, nil
}

type remoteFakeRows struct {
	values [][]driver.Value
	next   int
}

func (r *remoteFakeRows) Columns() []string { return []string{"COUNT(*)"} }
func (r *remoteFakeRows) Close() error      { return nil }

func (r *remoteFakeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

// remoteRigHost is a fake machine whose town names a Dolt server on another
// host, with server answering that town's SQL.
func remoteRigHost(t *testing.T, server *remoteFakeServer) (*fakeHost, *host, string) {
	t.Helper()
	f := newFakeHost()
	h := f.host()
	h.doltEndpoint = func(string) (configpkg.DoltEndpoint, bool) {
		return configpkg.DoltEndpoint{Host: "scratch-dolt", Port: 3307}, true
	}
	h.openDB = server.open
	return f, h, t.TempDir()
}

// startedCommands is every process the fake machine started.
func (f *fakeHost) startedCommands() []*exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*exec.Cmd(nil), f.started...)
}

// A remote InitRig creates the database on the server over SQL and leaves no
// local data directory and no local process: the server is somebody else's
// (gt-q0ppq).
func TestInitRigRemoteCreatesTheDatabaseOnTheServer(t *testing.T) {
	t.Parallel()
	server := &remoteFakeServer{}
	f, h, townRoot := remoteRigHost(t, server)
	h.readIssuePrefix = func(string, string) (string, error) { return "hq", nil }

	result, err := h.InitRig(townRoot, "hq")
	if err != nil {
		t.Fatalf("InitRig over a remote Dolt: %v", err)
	}
	if !result.Created || !result.ServerWasRunning {
		t.Errorf("InitRig result = %+v, want a created database on a running server", result)
	}

	got := server.snapshot()
	want := []string{
		"SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?",
		"CREATE DATABASE IF NOT EXISTS `hq`",
	}
	if len(got) != len(want) {
		t.Fatalf("statements = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d = %q, want %q", i, got[i], want[i])
		}
	}

	if starts := f.startedCommands(); len(starts) != 0 {
		t.Errorf("InitRig started %d process(es) for a remote server, want none: %v", len(starts), starts)
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".dolt-data")); !os.IsNotExist(err) {
		t.Errorf("InitRig made a local .dolt-data/ for a remote server (err = %v)", err)
	}
}

// A re-run leaves the database the server already has alone: the create is
// skipped, and the result says so (gt-q0ppq).
func TestInitRigRemoteIsIdempotent(t *testing.T) {
	t.Parallel()
	server := &remoteFakeServer{databases: []string{"hq"}}
	_, h, townRoot := remoteRigHost(t, server)
	h.readIssuePrefix = func(string, string) (string, error) { return "hq", nil }

	result, err := h.InitRig(townRoot, "hq")
	if err != nil {
		t.Fatalf("InitRig over a database the server already has: %v", err)
	}
	if result.Created {
		t.Errorf("InitRig result = %+v, want Created=false for a database the server already serves", result)
	}
	for _, stmt := range server.snapshot() {
		if strings.HasPrefix(stmt, "CREATE DATABASE") {
			t.Errorf("InitRig issued %q for a database the server already serves", stmt)
		}
	}
}

// A server that cannot be reached fails the rig init with the host and port
// named, and creates nothing locally (gt-q0ppq).
func TestInitRigRemoteNamesAnUnreachableServer(t *testing.T) {
	t.Parallel()
	server := &remoteFakeServer{openErr: errors.New("dial tcp 10.0.0.5:3307: connect: connection refused")}
	f, h, townRoot := remoteRigHost(t, server)

	_, err := h.InitRig(townRoot, "hq")
	if err == nil {
		t.Fatal("InitRig over an unreachable server succeeded, want an error")
	}
	for _, want := range []string{"scratch-dolt:3307", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if starts := f.startedCommands(); len(starts) != 0 {
		t.Errorf("InitRig started a local Dolt after the remote one refused: %v", starts)
	}
	if _, statErr := os.Stat(filepath.Join(townRoot, ".dolt-data")); !os.IsNotExist(statErr) {
		t.Errorf("InitRig left a .dolt-data/ after the remote one refused (err = %v)", statErr)
	}
}

// Every way the server can refuse the work is an error the caller sees, named
// with the host and port, and none of them falls back to a local Dolt
// (gt-q0ppq).
func TestInitRigRemoteReportsServerFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		server *remoteFakeServer
		prefix func(*testing.T) func(string, string) (string, error)
		want   string
	}{
		{"the existence read fails", &remoteFakeServer{queryErr: errors.New("Unknown database 'information_schema'")}, nil, "reading the database list"},
		{"the create fails", &remoteFakeServer{execErr: errors.New("Access denied for user")}, nil, "creating database"},
		{"the server refuses the second connection", &remoteFakeServer{failOpenAfter: 1}, nil, "creating database"},
		{"the issue prefix cannot be written", &remoteFakeServer{}, func(t *testing.T) func(string, string) (string, error) {
			return func(string, string) (string, error) { return "", errors.New("bd config get failed") }
		}, "ensuring issue_prefix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, h, townRoot := remoteRigHost(t, tt.server)
			if tt.prefix != nil {
				h.readIssuePrefix = tt.prefix(t)
				h.writeIssuePrefix = func(string, string, string, string) error {
					return errors.New("bd rename-prefix failed")
				}
			}

			_, err := h.InitRig(townRoot, "hq")
			if err == nil {
				t.Fatalf("InitRig over a server that refused the work succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %q", err, tt.want)
			}
			if starts := f.startedCommands(); len(starts) != 0 {
				t.Errorf("InitRig started a local Dolt after the remote refused: %v", starts)
			}
			if _, statErr := os.Stat(filepath.Join(townRoot, ".dolt-data")); !os.IsNotExist(statErr) {
				t.Errorf("InitRig left a .dolt-data/ after the remote refused (err = %v)", statErr)
			}
		})
	}
}

// A name that is not in the allowlist reaches no statement: CREATE DATABASE is
// built by concatenation, so a name is the one part of it that cannot be a
// bind parameter (gt-q0ppq).
func TestInitRigRemoteRefusesAnUnsafeDatabaseName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rigName string
	}{
		{"a space", "my rig"},
		{"a semicolon statement", "hq; drop database be"},
		{"a backtick", "hq`"},
		{"a single quote", "hq'"},
		{"a double quote", `hq"`},
		{"an uppercase letter", "HQ"},
		{"a leading digit", "1hq"},
		{"a hyphen", "my-rig"},
		{"a dot", "hq.be"},
		{"empty", ""},
		{"65 characters", "a" + strings.Repeat("b", 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := &remoteFakeServer{}
			_, h, townRoot := remoteRigHost(t, server)

			if _, err := h.InitRig(townRoot, tt.rigName); err == nil {
				t.Fatalf("InitRig(%q) succeeded, want the name refused", tt.rigName)
			}
			if got := server.snapshot(); len(got) != 0 {
				t.Errorf("refused name %q still issued %q", tt.rigName, got)
			}
			if opens := server.openCount(); opens != 0 {
				t.Errorf("refused name %q opened %d connection(s), want none", tt.rigName, opens)
			}
		})
	}
}

// Every name in the allowlist is created, quoted: the validation refuses what
// it must without narrowing what a rig may be called (gt-q0ppq).
func TestInitRigRemoteCreatesEveryAllowedName(t *testing.T) {
	t.Parallel()
	names := []string{"hq", "my_rig_2", "a", "a" + strings.Repeat("b", 63)}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := &remoteFakeServer{}
			_, h, townRoot := remoteRigHost(t, server)
			h.readIssuePrefix = func(string, string) (string, error) { return name, nil }

			if _, err := h.InitRig(townRoot, name); err != nil {
				t.Fatalf("InitRig(%q): %v", name, err)
			}
			want := "CREATE DATABASE IF NOT EXISTS `" + name + "`"
			var created bool
			for _, stmt := range server.snapshot() {
				if stmt == want {
					created = true
				}
			}
			if !created {
				t.Errorf("statements = %q, want %q", server.snapshot(), want)
			}
		})
	}
}
