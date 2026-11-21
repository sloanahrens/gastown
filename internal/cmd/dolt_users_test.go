package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/workspace"
)

// fakeUsersReader answers a read from a scripted list. With err set, every
// read fails instead.
type fakeUsersReader struct {
	accounts []doltUserAccount
	err      error
	closed   bool
}

var errUsersRead = errors.New("server went away")

func (r *fakeUsersReader) ReadAccounts(context.Context) ([]doltUserAccount, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.accounts, nil
}

func (r *fakeUsersReader) Close() error {
	r.closed = true
	return nil
}

// usersDeps builds deps around reader, so a test runs the command without a
// workspace or a server.
func usersDeps(out *bytes.Buffer, reader doltUsersReader) doltUsersDeps {
	return doltUsersDeps{
		open: func(context.Context) (doltUsersReader, error) { return reader, nil },
		out:  out,
	}
}

// The list names every account, marks the empty-password one, and closes with
// a summary.
func TestDoltUsersPrintsEachAccount(t *testing.T) {
	t.Parallel()
	reader := &fakeUsersReader{accounts: []doltUserAccount{
		{User: "root", Host: "%", EmptyPassword: true},
		{User: "app", Host: "10.0.0.1"},
	}}
	var out bytes.Buffer

	if err := runDoltUsers(context.Background(), usersDeps(&out, reader), false); err != nil {
		t.Fatalf("runDoltUsers: %v", err)
	}
	if !reader.closed {
		t.Error("the reader was not closed")
	}

	want := "root@%        password: empty\n" +
		"app@10.0.0.1  password: set\n" +
		"2 accounts, 1 with an empty password\n"
	if got := out.String(); got != want {
		t.Errorf("output =\n%q\nwant\n%q", got, want)
	}
}

// --check exits non-zero when an account has an empty password, and the error
// names that account.
func TestDoltUsersCheckFailsOnAnEmptyPassword(t *testing.T) {
	t.Parallel()
	reader := &fakeUsersReader{accounts: []doltUserAccount{
		{User: "root", Host: "%", EmptyPassword: true},
		{User: "app", Host: "10.0.0.1"},
	}}
	var out bytes.Buffer

	err := runDoltUsers(context.Background(), usersDeps(&out, reader), true)
	if err == nil {
		t.Fatal("--check accepted an account with an empty password")
	}
	if !strings.Contains(err.Error(), "root@%") {
		t.Errorf("--check error %q does not name the account root@%%", err)
	}
	if strings.Contains(err.Error(), "app@") {
		t.Errorf("--check error %q names app@10.0.0.1, which has a password", err)
	}
	if out.Len() == 0 {
		t.Error("--check printed no account list")
	}
}

// --check exits zero when every password is set.
func TestDoltUsersCheckPassesWhenEveryPasswordIsSet(t *testing.T) {
	t.Parallel()
	reader := &fakeUsersReader{accounts: []doltUserAccount{
		{User: "app", Host: "10.0.0.1"},
		{User: "root", Host: "%"},
	}}
	var out bytes.Buffer

	if err := runDoltUsers(context.Background(), usersDeps(&out, reader), true); err != nil {
		t.Fatalf("--check refused a server whose passwords are all set: %v", err)
	}
	if !strings.HasSuffix(out.String(), "2 accounts, none with an empty password\n") {
		t.Errorf("output = %q, want a summary saying no password is empty", out.String())
	}
}

// One account reads in the singular.
func TestDoltUsersSummaryCountsOneAccount(t *testing.T) {
	t.Parallel()
	reader := &fakeUsersReader{accounts: []doltUserAccount{{User: "root", Host: "%"}}}
	var out bytes.Buffer

	if err := runDoltUsers(context.Background(), usersDeps(&out, reader), false); err != nil {
		t.Fatalf("runDoltUsers: %v", err)
	}
	want := "root@%  password: set\n1 account, none with an empty password\n"
	if got := out.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// A read that fails is an error naming the cause, and no account list.
func TestDoltUsersReportsReadError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer

	err := runDoltUsers(context.Background(), usersDeps(&out, &fakeUsersReader{err: errUsersRead}), false)
	if !errors.Is(err, errUsersRead) {
		t.Fatalf("runDoltUsers error = %v, want it to wrap %v", err, errUsersRead)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q after a failed read, want nothing", out.String())
	}
}

// A server that lists no account is an error, never a green --check.
func TestDoltUsersReportsAnEmptyAccountList(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer

	err := runDoltUsers(context.Background(), usersDeps(&out, &fakeUsersReader{}), true)
	if err == nil {
		t.Fatal("--check passed on an account list that is empty")
	}
	if out.Len() != 0 {
		t.Errorf("printed %q for an empty account list, want nothing", out.String())
	}
}

// A server that will not open is an error, and no account list.
func TestDoltUsersReportsOpenFailure(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	deps := doltUsersDeps{
		open: func(context.Context) (doltUsersReader, error) { return nil, errUsersRead },
		out:  &out,
	}

	err := runDoltUsers(context.Background(), deps, false)
	if !errors.Is(err, errUsersRead) {
		t.Fatalf("runDoltUsers error = %v, want it to wrap %v", err, errUsersRead)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q when the server would not open, want nothing", out.String())
	}
}

// failingUsersWriter refuses every write, so the write-error branch runs.
type failingUsersWriter struct{}

var errUsersWrite = errors.New("broken pipe")

func (failingUsersWriter) Write([]byte) (int, error) { return 0, errUsersWrite }

func TestDoltUsersReportsWriteError(t *testing.T) {
	t.Parallel()
	reader := &fakeUsersReader{accounts: []doltUserAccount{{User: "root", Host: "%"}}}
	deps := doltUsersDeps{
		open: func(context.Context) (doltUsersReader, error) { return reader, nil },
		out:  failingUsersWriter{},
	}

	if err := runDoltUsers(context.Background(), deps, false); !errors.Is(err, errUsersWrite) {
		t.Errorf("runDoltUsers error = %v, want %v", err, errUsersWrite)
	}
}

// The fake SQL driver answers the accounts read from memory.
const usersFakeDriverName = "gt_dolt_users_fake"

var (
	usersFakeDriversMu sync.Mutex
	usersFakeDrivers   = make(map[string]*usersFakeServer)
	usersFakeDriverSeq atomic.Uint64
	usersFakeRegister  sync.Once
)

type usersFakeServer struct {
	columns    []string
	rows       [][]driver.Value
	connectErr error
	queryErr   error
	closed     bool
}

// registerFakeUsersServer records server under a DSN unique to this call, so
// tests that run in parallel never share a fake.
func registerFakeUsersServer(t *testing.T, server *usersFakeServer) string {
	t.Helper()
	usersFakeRegister.Do(func() { sql.Register(usersFakeDriverName, usersFakeDriver{}) })
	dsn := fmt.Sprintf("fake-users-%d", usersFakeDriverSeq.Add(1))
	usersFakeDriversMu.Lock()
	usersFakeDrivers[dsn] = server
	usersFakeDriversMu.Unlock()
	t.Cleanup(func() {
		usersFakeDriversMu.Lock()
		delete(usersFakeDrivers, dsn)
		usersFakeDriversMu.Unlock()
	})
	return dsn
}

func openFakeUsersReader(t *testing.T, server *usersFakeServer) *mysqlUsersReader {
	t.Helper()
	db, err := sql.Open(usersFakeDriverName, registerFakeUsersServer(t, server))
	if err != nil {
		t.Fatalf("sql.Open on the fake driver: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &mysqlUsersReader{db: db}
}

func fakeUsersDB(t *testing.T, server *usersFakeServer) func(string) (*sql.DB, error) {
	t.Helper()
	dsn := registerFakeUsersServer(t, server)
	return func(string) (*sql.DB, error) { return sql.Open(usersFakeDriverName, dsn) }
}

type usersFakeDriver struct{}

func (usersFakeDriver) Open(dsn string) (driver.Conn, error) {
	usersFakeDriversMu.Lock()
	server, ok := usersFakeDrivers[dsn]
	usersFakeDriversMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no fake server registered for %q", dsn)
	}
	if server.connectErr != nil {
		return nil, server.connectErr
	}
	return &usersFakeConn{server: server}, nil
}

type usersFakeConn struct{ server *usersFakeServer }

func (c *usersFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not used by the reader")
}

func (c *usersFakeConn) Close() error {
	c.server.closed = true
	return nil
}

func (c *usersFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not used by the reader")
}

func (c *usersFakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query != doltUsersQuery {
		return nil, fmt.Errorf("unexpected query: %s", query)
	}
	if c.server.queryErr != nil {
		return nil, c.server.queryErr
	}
	columns := c.server.columns
	if columns == nil {
		columns = []string{"user", "host", "authentication_string = ''"}
	}
	return &usersFakeRows{columns: columns, rows: c.server.rows}, nil
}

type usersFakeRows struct {
	columns []string
	rows    [][]driver.Value
	at      int
}

func (r *usersFakeRows) Columns() []string { return r.columns }
func (r *usersFakeRows) Close() error      { return nil }

func (r *usersFakeRows) Next(dest []driver.Value) error {
	if r.at >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.at])
	r.at++
	return nil
}

// The reader runs the one accounts statement and returns each row's user,
// host and whether its password is empty.
func TestDoltUsersReadsTheAccounts(t *testing.T) {
	t.Parallel()
	server := &usersFakeServer{rows: [][]driver.Value{
		{"app", "10.0.0.1", int64(0)},
		{"root", "%", int64(1)},
		{"repl", "10.0.0.2", uint64(1)},
	}}

	got, err := openFakeUsersReader(t, server).ReadAccounts(context.Background())
	if err != nil {
		t.Fatalf("ReadAccounts: %v", err)
	}

	want := []doltUserAccount{
		{User: "app", Host: "10.0.0.1", EmptyPassword: false},
		{User: "root", Host: "%", EmptyPassword: true},
		{User: "repl", Host: "10.0.0.2", EmptyPassword: true},
	}
	if len(got) != len(want) {
		t.Fatalf("ReadAccounts returned %d accounts, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("account %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The statement asks only whether the password is empty: it never selects the
// authentication string, so the string is never read, printed or logged.
func TestDoltUsersStatementAsksOnlyWhetherThePasswordIsEmpty(t *testing.T) {
	t.Parallel()
	if !strings.Contains(doltUsersQuery, "authentication_string = ''") {
		t.Errorf("the statement %q does not ask for the empty password predicate", doltUsersQuery)
	}
	if !strings.Contains(doltUsersQuery, "FROM mysql.user") {
		t.Errorf("the statement %q does not read mysql.user", doltUsersQuery)
	}
	if strings.Contains(doltUsersQuery, "plugin") {
		t.Errorf("the statement %q selects the authentication plugin", doltUsersQuery)
	}
}

// A row that carries the authentication string cannot be read, so a statement
// changed to select it fails to scan instead of printing the secret.
func TestDoltUsersRefusesAPasswordString(t *testing.T) {
	t.Parallel()
	server := &usersFakeServer{rows: [][]driver.Value{
		{"root", "%", "super-secret-hash"},
	}}

	got, err := openFakeUsersReader(t, server).ReadAccounts(context.Background())
	if err == nil {
		t.Fatalf("ReadAccounts accepted a password string: %+v", got)
	}
	if strings.Contains(err.Error(), "super-secret-hash") {
		t.Errorf("the scan error echoes the value it could not read: %v", err)
	}
}

// The empty-password column is read only in the driver's integer shape (int64,
// or uint64 for an unsigned BIGINT, valued 0 or 1). Anything else is refused,
// even when bool parsing would accept it, so a statement changed to select the
// authentication string fails to scan instead of reporting a weak or strong
// account; a []byte '0' is refused on purpose (the driver gives it only for a
// string-like column, never for the TINY comparison).
func TestDoltUsersScanGuard(t *testing.T) {
	t.Parallel()
	for _, answer := range []driver.Value{"1", "true", "0", []byte("0"), []byte("1"), []byte("2"), []byte(""), int64(2), int64(-1), uint64(2), float64(1)} {
		t.Run(fmt.Sprintf("%T %v", answer, answer), func(t *testing.T) {
			t.Parallel()
			server := &usersFakeServer{rows: [][]driver.Value{{"root", "%", answer}}}
			if got, err := openFakeUsersReader(t, server).ReadAccounts(context.Background()); err == nil {
				t.Fatalf("ReadAccounts accepted %T %v as the empty-password answer: %+v", answer, answer, got)
			}
		})
	}
}

// A failed query, a row that does not scan and an empty flag the server left
// NULL are each an error, never a guessed account.
func TestDoltUsersReportsReadFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		server *usersFakeServer
	}{
		{"the query fails", &usersFakeServer{queryErr: errUsersRead}},
		{"a row does not scan", &usersFakeServer{
			columns: []string{"user", "host"},
			rows:    [][]driver.Value{{"root", "%"}},
		}},
		{"the empty flag is NULL", &usersFakeServer{rows: [][]driver.Value{
			{"root", "%", nil},
		}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := openFakeUsersReader(t, tc.server).ReadAccounts(context.Background()); err == nil {
				t.Fatal("ReadAccounts reported no error for a read it could not trust")
			}
		})
	}
}

// The opener refuses a caller outside a workspace, a connection that will not
// open and a server that does not answer, and each error says where.
func TestDoltUsersOpensTheServer(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	findTown := func() (string, error) { return townRoot, nil }

	cases := []struct {
		name    string
		opener  doltUsersOpener
		wantMsg string
	}{
		{
			name: "not in a workspace",
			opener: doltUsersOpener{
				findTownRoot: func() (string, error) { return "", errUsersRead },
			},
			wantMsg: "not in a Gas Town workspace",
		},
		{
			name: "the connection will not open",
			opener: doltUsersOpener{
				findTownRoot: findTown,
				openDB:       func(string) (*sql.DB, error) { return nil, errUsersRead },
			},
			wantMsg: "opening a connection",
		},
		{
			name: "the server does not answer",
			opener: doltUsersOpener{
				findTownRoot: findTown,
				openDB:       fakeUsersDB(t, &usersFakeServer{connectErr: errUsersRead}),
			},
			wantMsg: "connecting to the Dolt server",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader, err := tc.opener.open(context.Background())
			if err == nil {
				_ = reader.Close()
				t.Fatal("open accepted a server it could not reach")
			}
			if !errors.Is(err, errUsersRead) {
				t.Errorf("open error = %v, want it to wrap %v", err, errUsersRead)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("open error %q does not say %q", err, tc.wantMsg)
			}
		})
	}
}

// The path a healthy run takes: the opener reaches a server that answers, and
// the reader it returns serves the accounts.
func TestDoltUsersOpensAWorkingServer(t *testing.T) {
	t.Parallel()
	server := &usersFakeServer{rows: [][]driver.Value{
		{"root", "%", int64(1)},
	}}
	opener := doltUsersOpener{
		findTownRoot: func() (string, error) { return t.TempDir(), nil },
		openDB:       fakeUsersDB(t, server),
	}

	reader, err := opener.open(context.Background())
	if err != nil {
		t.Fatalf("open on a server that answers: %v", err)
	}

	got, err := reader.ReadAccounts(context.Background())
	if err != nil {
		t.Fatalf("ReadAccounts: %v", err)
	}
	want := []doltUserAccount{{User: "root", Host: "%", EmptyPassword: true}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("ReadAccounts = %+v, want %+v", got, want)
	}
	if err := reader.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !server.closed {
		t.Error("Close did not close the server connection")
	}
}

// The command registers --check beside its name.
func TestDoltUsersRegistersTheCheckFlag(t *testing.T) {
	t.Parallel()
	f := doltUsersCmd.Flags().Lookup("check")
	if f == nil {
		t.Fatal("gt dolt users has no --check flag")
	}
	if f.DefValue != "false" {
		t.Errorf("--check defaults to %q, want false", f.DefValue)
	}
}

// capturingUsersDB is fakeUsersDB that also records the dsn the opener built,
// which fakeUsersDB discards.
func capturingUsersDB(t *testing.T, got *string) func(string) (*sql.DB, error) {
	t.Helper()
	open := fakeUsersDB(t, &usersFakeServer{})
	return func(dsn string) (*sql.DB, error) {
		*got = dsn
		return open(dsn)
	}
}

// TestDoltUsersBuildsTheRealDSN drives the production DSN builder over a temp
// town: the DSN the opener hands the driver names the town's Dolt user and
// port, over TCP or the socket the builder prefers, and carries the two
// timeouts the opener asks for. A failure never prints the DSN, which holds
// the credential: it prints the part after the credential's "@".
func TestDoltUsersBuildsTheRealDSN(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var dsn string
	opener := doltUsersOpener{
		findTownRoot: func() (string, error) { return town, nil },
		openDB:       capturingUsersDB(t, &dsn),
	}
	reader, err := opener.open(context.Background())
	if err != nil {
		t.Fatalf("open over the temp town: %v", err)
	}
	_ = reader.Close()

	port := doltserver.DefaultConfig(town).Port
	userPart, rest, found := strings.Cut(dsn, "@")
	if !found {
		t.Fatal("the opener's DSN has no user part")
	}
	if user, _, _ := strings.Cut(userPart, ":"); user != doltserver.DefaultUser {
		t.Errorf("the DSN's user is %q, want the town's %q", user, doltserver.DefaultUser)
	}
	tests := []struct {
		name string
		want *regexp.Regexp
	}{
		{"the user and the town's address", regexp.MustCompile(fmt.Sprintf(`^(tcp\(127\.0\.0\.1:%d\)|unix\(/tmp/mysql(\.%d)?\.sock\))/`, port, port))},
		{"the two timeouts", regexp.MustCompile(`\?timeout=5s&readTimeout=30s$`)},
	}
	for _, tc := range tests {
		if !tc.want.MatchString(rest) {
			t.Errorf("%s: the DSN after its user part is %q, want it to match %s", tc.name, rest, tc.want)
		}
	}
}

// TestDoltUsersFindsTheTownFromTheWorkingDirectory drives the opener through
// workspace.FindFromCwdOrError, the lookup production hands it: inside a town
// it reaches the driver, outside any it fails with the workspace error first.
func TestDoltUsersFindsTheTownFromTheWorkingDirectory(t *testing.T) { //testpolicy:allow parallel — the working directory is process-wide and is the input under test
	t.Setenv("GT_TOWN_ROOT", "") //testpolicy:allow no-env — the lookup falls back to GT_TOWN_ROOT, so the outside-a-town row needs it empty
	town, bare := t.TempDir(), t.TempDir()
	marker := filepath.Join(town, workspace.PrimaryMarker)
	mkdirErr := os.MkdirAll(filepath.Dir(marker), 0o700)
	if err := errors.Join(mkdirErr, os.WriteFile(marker, []byte("{}"), 0o600)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, dir string
		wantTown  bool
	}{{"inside a town", town, true}, {"outside any town", bare, false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.dir) //testpolicy:allow no-chdir — the working directory is the input under test: the lookup walks up from it
			var dsn, found string
			opener := doltUsersOpener{
				findTownRoot: func() (string, error) {
					root, err := workspace.FindFromCwdOrError()
					found = root
					return root, err
				},
				openDB: capturingUsersDB(t, &dsn),
			}
			reader, err := opener.open(context.Background())
			if err == nil {
				_ = reader.Close()
			}
			want, _ := filepath.EvalSymlinks(town)
			got, _ := filepath.EvalSymlinks(found)
			// What the opener derives from the root it was given; never printed (the DSN holds the credential).
			var wantDSN string
			if found != "" {
				wantDSN = buildDoltDSNFromConfig(doltserver.DefaultConfig(found), "", dsnOpts{Timeout: "5s", ReadTimeout: "30s"})
			}
			switch {
			case tc.wantTown && (err != nil || got != want || dsn != wantDSN):
				t.Errorf("inside the town: err = %v, found %q, DSN is the one derived from it = %v; want the town %q and its DSN", err, found, dsn == wantDSN, town)
			case !tc.wantTown && (!errors.Is(err, workspace.ErrNotFound) || dsn != ""):
				t.Errorf("outside a town: err = %v, reached the driver = %v; want %v and no driver (the walk climbs to /: no ancestor of the temp directory may hold mayor/town.json)", err, dsn != "", workspace.ErrNotFound)
			}
		})
	}
}
