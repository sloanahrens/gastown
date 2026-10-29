package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/doltserver"
)

const (
	// doltCompareDefaultPort is where a Dolt server listens when an endpoint
	// names no port.
	doltCompareDefaultPort = 3307
	// doltCompareSourcePasswordEnv and doltCompareTargetPasswordEnv name the
	// environment variables a password comes from. It is not a flag: a flag
	// value is in the process list, where any local user reads it.
	doltCompareSourcePasswordEnv = "GT_DOLT_COMPARE_SOURCE_PASSWORD" //nolint:gosec // G101: the variable's name, not a credential
	doltCompareTargetPasswordEnv = "GT_DOLT_COMPARE_TARGET_PASSWORD" //nolint:gosec // G101: the variable's name, not a credential
	// doltCompareContentHashQuery is the content hash both servers answer
	// with. DOLT_HASHOF_DB() hashes the database's working set, committed and
	// uncommitted, so two copies of the same content hash equal even when
	// their commit histories differ.
	doltCompareContentHashQuery = "SELECT DOLT_HASHOF_DB()"
	// doltCompareTableListQuery names the tables whose rows the count sums.
	// Views are excluded: their rows are already counted in their base tables.
	doltCompareTableListQuery = "SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE'"
)

// compareIdentifierRE is the whole namespace a database or table name may use
// here. A name outside it is refused, not spliced into SQL with escaping rules
// this command would have to get right.
var compareIdentifierRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

// doltCompareDeps is the command's contact with the outside world: a reader
// for each server, the environment passwords come from, and the stream the
// comparison is written to. A test supplies fakes, so no unit test opens a
// connection or reads the environment (docs/testing.md, "Seams for external
// tools").
type doltCompareDeps struct {
	newReader      func(doltCompareEndpoint, time.Duration) snapshotReader
	lookupPassword func(string) string
	out            io.Writer
}

// doltCompareRealDeps is doltCompareDeps on the real machine.
func doltCompareRealDeps() doltCompareDeps {
	return doltCompareDeps{
		newReader: func(ep doltCompareEndpoint, timeout time.Duration) snapshotReader {
			return &mysqlSnapshotReader{dsn: ep.dsn(timeout), label: ep.label(), timeout: timeout}
		},
		lookupPassword: os.Getenv,
		out:            os.Stdout,
	}
}

// newDoltCompareCmd builds `gt dolt compare`, the server-to-server comparison
// a restore proof runs: the restored copy must match its source database by
// database.
func newDoltCompareCmd(deps doltCompareDeps) *cobra.Command {
	var user string
	var timeout time.Duration
	var useTLS bool
	cmd := &cobra.Command{
		Use:   "compare <source> <target>",
		Short: "Compare per-database row counts and content hashes between two Dolt servers",
		// A difference is the command's answer, not a misuse of it: printing
		// the usage text under the finding buries it.
		SilenceUsage: true,
		Long: `Compare two Dolt servers database by database, for a restore proof: the copy
a restore produced must match its source.

Each endpoint is host:port, with port 3307 when the port is omitted and an
IPv6 address bracketed ([::1]:3307). The command reads, for every database on
either server, the total row count and the content hash of the database's
working set (DOLT_HASHOF_DB()), and reports each database as equal, as
differing in row count (with both numbers), as differing in content hash (with
both hashes), or as present on one side only.

The Dolt/MySQL internal databases (information_schema, mysql, dolt_cluster)
are skipped: they are not content either side is asked to match.

It only reads: nothing is written to either server, and no credential is
printed. Passwords come from the environment, one per side:
  GT_DOLT_COMPARE_SOURCE_PASSWORD   password for the source server
  GT_DOLT_COMPARE_TARGET_PASSWORD   password for the target server

--tls connects to both servers over TLS and verifies their certificates
against the system roots, so a server with a self-signed certificate is
reached only once its CA is in that store. Without --tls the passwords cross
the network in cleartext.

The exit status is non-zero when any database differs or is missing on one
side, and zero only when both servers serve the same databases with equal row
counts and content hashes. Two servers that serve no databases are refused:
there is nothing to prove.

A server still taking writes answers from different moments at each read, so
compare a source that is quiesced when the result is a proof.

Example:
  gt dolt compare 127.0.0.1:3307 127.0.0.1:3401`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoltCompare(cmd.Context(), deps, args, user, timeout, useTLS)
		},
	}
	cmd.Flags().StringVar(&user, "user", "root", "MySQL user to connect as to both servers")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "Deadline for reading each server")
	cmd.Flags().BoolVar(&useTLS, "tls", false, "Connect to both servers over TLS, verifying each certificate against the system roots")
	return cmd
}

func runDoltCompare(ctx context.Context, deps doltCompareDeps, args []string, user string, timeout time.Duration, useTLS bool) error {
	source, err := parseDoltCompareEndpoint(args[0], user, deps.lookupPassword(doltCompareSourcePasswordEnv))
	if err != nil {
		return fmt.Errorf("source endpoint %q: %w", args[0], err)
	}
	source.TLS = useTLS
	target, err := parseDoltCompareEndpoint(args[1], user, deps.lookupPassword(doltCompareTargetPasswordEnv))
	if err != nil {
		return fmt.Errorf("target endpoint %q: %w", args[1], err)
	}
	target.TLS = useTLS
	return compareServers(ctx, deps.out,
		deps.newReader(source, timeout), deps.newReader(target, timeout))
}

// compareServers reads both servers, writes the comparison to w and returns
// the command's exit status: an error naming every database that differs, or
// the absence of any database to compare.
func compareServers(ctx context.Context, w io.Writer, source, target snapshotReader) error {
	src, err := source.Read(ctx)
	if err != nil {
		return fmt.Errorf("reading source server: %w", err)
	}
	tgt, err := target.Read(ctx)
	if err != nil {
		return fmt.Errorf("reading target server: %w", err)
	}

	diffs := compareSnapshots(src, tgt)
	if len(diffs) == 0 {
		return errNothingToCompare
	}
	if err := printComparison(w, diffs); err != nil {
		return fmt.Errorf("writing comparison: %w", err)
	}
	return compareExitError(diffs)
}

// errNothingToCompare is the refusal for two servers that serve no databases.
// Equal-by-emptiness is not a match a restore proof can rest on, so it exits
// non-zero rather than reading as a pass.
var errNothingToCompare = errors.New("neither server serves a database, so there is nothing to compare")

// doltCompareEndpoint is one server a comparison reads.
type doltCompareEndpoint struct {
	Host     string
	Port     int
	User     string
	Password string
	TLS      bool
}

// parseDoltCompareEndpoint reads an endpoint's host:port, defaulting the port
// to doltCompareDefaultPort when it names none.
func parseDoltCompareEndpoint(raw, user, password string) (doltCompareEndpoint, error) {
	if strings.TrimSpace(raw) == "" {
		return doltCompareEndpoint{}, fmt.Errorf("empty endpoint")
	}
	host, portText, err := net.SplitHostPort(raw)
	switch {
	case err == nil:
		if portText == "" {
			return doltCompareEndpoint{}, fmt.Errorf("%q names no port after the colon; omit the colon to take port %d", raw, doltCompareDefaultPort)
		}
	case strings.HasPrefix(raw, "["):
		// A bracket that does not close would be trimmed to a host that dials
		// some other address, so a bracketed endpoint is either bracketed in
		// full — [::1] takes the default port — or refused.
		if !strings.HasSuffix(raw, "]") {
			return doltCompareEndpoint{}, fmt.Errorf("bracketed address %q does not close its bracket; write [address]:port", raw)
		}
		inner := raw[1 : len(raw)-1]
		if net.ParseIP(inner) == nil {
			return doltCompareEndpoint{}, fmt.Errorf("%q is not an IP address in brackets", raw)
		}
		host, portText = inner, ""
	case strings.ContainsAny(raw, "[]"):
		return doltCompareEndpoint{}, fmt.Errorf("stray bracket in %q; write [address]:port", raw)
	case strings.Count(raw, ":") > 1:
		// An IPv6 literal with more than one colon and no brackets is refused:
		// "fe80::1:3307" is a valid address with no port, so guessing which
		// reading was meant would connect to the wrong server without a word.
		return doltCompareEndpoint{}, fmt.Errorf("IPv6 address %q must be bracketed, e.g. [::1]:3307", raw)
	default:
		// A bare host has no port to split.
		host, portText = raw, ""
	}
	if host == "" {
		return doltCompareEndpoint{}, fmt.Errorf("no host in %q", raw)
	}
	port := doltCompareDefaultPort
	if portText != "" {
		if port, err = strconv.Atoi(portText); err != nil {
			return doltCompareEndpoint{}, fmt.Errorf("port %q is not a number", portText)
		}
		if port < 1 || port > 65535 {
			return doltCompareEndpoint{}, fmt.Errorf("port %d is outside 1-65535", port)
		}
	}
	return doltCompareEndpoint{Host: host, Port: port, User: user, Password: password}, nil
}

// label is the endpoint as output shows it: host:port, never the credential.
func (e doltCompareEndpoint) label() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// dsn is the MySQL DSN that reaches this endpoint over TCP. It carries the
// password, so it is passed to the driver and never printed or returned in an
// error.
func (e doltCompareEndpoint) dsn(timeout time.Duration) string {
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = e.label()
	cfg.User = e.User
	cfg.Passwd = e.Password
	cfg.Timeout = timeout
	cfg.ReadTimeout = timeout
	cfg.WriteTimeout = timeout
	if e.TLS {
		// "true" verifies the server certificate against the system roots.
		cfg.TLSConfig = "true"
	}
	return cfg.FormatDSN()
}

// dbSnapshot is what one server reports about one database.
type dbSnapshot struct {
	Name string
	Rows int64
	Hash string
}

// snapshotReader reads one server's per-database snapshots.
type snapshotReader interface {
	Read(ctx context.Context) ([]dbSnapshot, error)
}

// mysqlSnapshotReader reads a server over the MySQL protocol, one pinned
// connection for the whole read so USE and the queries after it see one
// session.
type mysqlSnapshotReader struct {
	dsn     string
	label   string
	timeout time.Duration
	// openDB opens the connection pool; nil opens it with the MySQL driver. A
	// test supplies a driver that answers in process, so every failure path of
	// Read is driven without a server (docs/testing.md, "Seams for external
	// tools").
	openDB func(dsn string) (*sql.DB, error)
}

func (r *mysqlSnapshotReader) open() (*sql.DB, error) {
	if r.openDB != nil {
		return r.openDB(r.dsn)
	}
	return sql.Open("mysql", r.dsn)
}

func (r *mysqlSnapshotReader) Read(ctx context.Context) ([]dbSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	db, err := r.open()
	if err != nil {
		return nil, fmt.Errorf("%s: opening connection: %w", r.label, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: connecting: %w", r.label, err)
	}
	defer conn.Close()

	names, err := readDatabaseNames(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("%s: listing databases: %w", r.label, err)
	}

	snapshots := make([]dbSnapshot, 0, len(names))
	for _, name := range names {
		quoted, err := quoteCompareIdentifier(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.label, err)
		}
		if _, err := conn.ExecContext(ctx, "USE "+quoted); err != nil {
			return nil, fmt.Errorf("%s: using database %s: %w", r.label, name, err)
		}
		rows, err := countDatabaseRows(ctx, conn, name)
		if err != nil {
			return nil, fmt.Errorf("%s: counting rows in %s: %w", r.label, name, err)
		}
		var hash string
		if err := conn.QueryRowContext(ctx, doltCompareContentHashQuery).Scan(&hash); err != nil {
			return nil, fmt.Errorf("%s: reading the content hash of %s: %w", r.label, name, err)
		}
		snapshots = append(snapshots, dbSnapshot{Name: name, Rows: rows, Hash: hash})
	}
	return snapshots, nil
}

// readDatabaseNames lists the databases a server serves, without the
// Dolt/MySQL internal ones neither side is asked to match.
func readDatabaseNames(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if !doltserver.IsSystemDatabase(name) {
			names = append(names, name)
		}
	}
	return names, rows.Err()
}

// countDatabaseRows sums COUNT(*) over the database's base tables, read from
// the connection already switched to it. Every view resolves to base tables
// that are counted already, so counting views too would count their rows
// twice.
func countDatabaseRows(ctx context.Context, conn *sql.Conn, database string) (int64, error) {
	tables, err := listBaseTables(ctx, conn, database)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, table := range tables {
		quoted, err := quoteCompareIdentifier(table)
		if err != nil {
			return 0, err
		}
		var count int64
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoted).Scan(&count); err != nil {
			return 0, fmt.Errorf("table %s: %w", table, err)
		}
		total += count
	}
	return total, nil
}

// listBaseTables names the tables of one database that hold rows.
func listBaseTables(ctx context.Context, conn *sql.Conn, database string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, doltCompareTableListQuery, database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

// quoteCompareIdentifier renders a database or table name for splicing into
// SQL, refusing a name outside the plain-identifier namespace.
func quoteCompareIdentifier(name string) (string, error) {
	if !compareIdentifierRE.MatchString(name) {
		return "", fmt.Errorf("refusing identifier %q: only letters, digits, _ and - are accepted", name)
	}
	return "`" + name + "`", nil
}

// dbDiff is one database's place in the comparison. Source and Target are nil
// when the database is absent from that server.
type dbDiff struct {
	Name   string
	Source *dbSnapshot
	Target *dbSnapshot
}

// equal reports whether both servers serve this database with the same row
// count and content hash.
func (d dbDiff) equal() bool {
	return d.Source != nil && d.Target != nil &&
		d.Source.Rows == d.Target.Rows && d.Source.Hash == d.Target.Hash
}

// status is the difference in one line, naming both values whenever the two
// sides disagree.
func (d dbDiff) status() string {
	switch {
	case d.Source == nil:
		return "present on target only"
	case d.Target == nil:
		return "present on source only"
	}
	var parts []string
	if d.Source.Rows != d.Target.Rows {
		parts = append(parts, fmt.Sprintf("row count differs (source %d, target %d)", d.Source.Rows, d.Target.Rows))
	}
	if d.Source.Hash != d.Target.Hash {
		parts = append(parts, fmt.Sprintf("content hash differs (source %s, target %s)", d.Source.Hash, d.Target.Hash))
	}
	if len(parts) == 0 {
		return "equal"
	}
	return strings.Join(parts, "; ")
}

// compareSnapshots pairs the two servers' databases by name, ordered by name,
// and reports every database either side serves. It reads and writes nothing.
func compareSnapshots(source, target []dbSnapshot) []dbDiff {
	byName := make(map[string]*dbDiff, len(source)+len(target))
	var names []string
	diffFor := func(name string) *dbDiff {
		if d, ok := byName[name]; ok {
			return d
		}
		d := &dbDiff{Name: name}
		byName[name] = d
		names = append(names, name)
		return d
	}
	for _, snapshot := range source {
		diffFor(snapshot.Name).Source = &snapshot
	}
	for _, snapshot := range target {
		diffFor(snapshot.Name).Target = &snapshot
	}

	sort.Strings(names)
	diffs := make([]dbDiff, 0, len(names))
	for _, name := range names {
		diffs = append(diffs, *byName[name])
	}
	return diffs
}

// printComparison writes the per-database table, one row per database.
func printComparison(w io.Writer, diffs []dbDiff) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DATABASE\tSOURCE ROWS\tTARGET ROWS\tSTATUS")
	for _, d := range diffs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Name, rowCountText(d.Source), rowCountText(d.Target), d.status())
	}
	return tw.Flush()
}

// rowCountText is a row count as the table prints it: a dash for a database
// the side does not serve.
func rowCountText(snapshot *dbSnapshot) string {
	if snapshot == nil {
		return "-"
	}
	return strconv.FormatInt(snapshot.Rows, 10)
}

// compareExitError is the command's exit status: nil only when every database
// on both sides is equal and the two servers serve the same set of databases.
func compareExitError(diffs []dbDiff) error {
	var differing []string
	for _, d := range diffs {
		if !d.equal() {
			differing = append(differing, d.Name)
		}
	}
	if len(differing) == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d database(s) differ: %s", len(differing), len(diffs), strings.Join(differing, ", "))
}
