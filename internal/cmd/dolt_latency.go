package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/workspace"
)

// doltLatencyStep is one read in the fixed mix.
type doltLatencyStep struct {
	name  string
	query beadsql.Query
}

// doltLatencyProbeID is the id the lookup step asks for. No issue carries it,
// so the lookup returns no row and still makes the full round trip bd makes
// for an id it does not find.
const doltLatencyProbeID = "gt-latency-probe"

// doltLatencyMix is the fixed query mix, in the order each round runs it: the
// reads bd issues, a list, a lookup by id, and a count. Every statement is
// read-only; the command never writes. The statements come from beadsql, so
// they pass the same read-only guard as every other bd read.
var doltLatencyMix = []doltLatencyStep{
	{name: "list", query: beadsql.InProgressByAge()},
	{name: "lookup", query: beadsql.LabelPresent(doltLatencyProbeID, "latency")},
	{name: "count", query: beadsql.IssueCount()},
}

// doltLatencyQueryTimeout bounds one step. A step that runs past it fails the
// run; it is never dropped from the samples, which would flatter the result.
const doltLatencyQueryTimeout = 5 * time.Second

// errNoDoltLatencySamples is returned when a run has no samples. An empty
// series has no percentile, so it is an error and never reported as zero.
var errNoDoltLatencySamples = errors.New("no samples: the run recorded no query round trips")

// doltLatencyPercentile returns the nearest-rank percentile of sorted: the
// sample at rank ceil(percent*N/100), counting from 1. The rank is integer
// arithmetic, so float rounding never moves it by one sample. sorted must be
// ascending and non-empty; summarizeDoltLatency checks that first.
func doltLatencyPercentile(sorted []time.Duration, percent int) time.Duration {
	rank := (percent*len(sorted) + 99) / 100
	return sorted[rank-1]
}

// doltLatencySummary is the result of one run.
type doltLatencySummary struct {
	Samples int
	P50     time.Duration
	P95     time.Duration
	Max     time.Duration
	// Slowest is the step whose sample was the run's max, the first such step
	// in mix order. summarizeDoltLatency leaves it empty: a list of durations
	// carries no step names.
	Slowest string
}

// summarizeDoltLatency sorts a copy of samples and reads p50, p95 and max by
// nearest rank. An empty series is an error.
func summarizeDoltLatency(samples []time.Duration) (doltLatencySummary, error) {
	if len(samples) == 0 {
		return doltLatencySummary{}, errNoDoltLatencySamples
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return doltLatencySummary{
		Samples: len(sorted),
		P50:     doltLatencyPercentile(sorted, 50),
		P95:     doltLatencyPercentile(sorted, 95),
		Max:     doltLatencyPercentile(sorted, 100),
	}, nil
}

// doltLatencyConn is the connection the probe runs its mix through.
type doltLatencyConn interface {
	// Run executes q and reads its whole result, so the time it takes
	// includes the rows, as bd's does.
	Run(ctx context.Context, q beadsql.Query) error
	Close() error
}

// doltLatencyDialer opens a connection to database on host:port. It is where
// the connection is established and the schema level checked.
type doltLatencyDialer func(ctx context.Context, host string, port int, database string) (doltLatencyConn, error)

// doltLatencyRun is one probe: where to connect and how many rounds of the mix
// to run. A round is one pass over doltLatencyMix.
type doltLatencyRun struct {
	Host     string
	Port     int
	Database string
	Rounds   int
}

// runDoltLatencyProbe runs the mix Rounds times and returns the summary. now is
// the clock each step is timed on. A connection failure, a failed step and an
// empty run each return an error and no summary.
func runDoltLatencyProbe(ctx context.Context, dial doltLatencyDialer, now func() time.Time, run doltLatencyRun) (doltLatencySummary, error) {
	addr := fmt.Sprintf("%s:%d", run.Host, run.Port)
	// Opening a connection also reads the schema level, so a server that
	// accepts the socket and then stalls must not hang the probe. The dial
	// context is canceled as soon as dial returns; each later step runs under
	// its own context.
	dialCtx, cancelDial := context.WithTimeout(ctx, doltLatencyQueryTimeout)
	conn, err := dial(dialCtx, run.Host, run.Port, run.Database)
	cancelDial()
	if err != nil {
		return doltLatencySummary{}, fmt.Errorf("cannot connect to %s (database %s): %w", addr, run.Database, err)
	}
	// The summary is already decided by the time the connection closes, so a
	// close error does not change the result.
	defer func() { _ = conn.Close() }()

	// A count below one runs no rounds, so the empty series is refused below. The
	// clamp keeps a negative count from making the buffer negative.
	samples := make([]time.Duration, 0, max(run.Rounds, 0)*len(doltLatencyMix))
	var slowest string
	var slowestAt time.Duration
	for round := 0; round < run.Rounds; round++ {
		for _, step := range doltLatencyMix {
			stepCtx, cancel := context.WithTimeout(ctx, doltLatencyQueryTimeout)
			start := now()
			err := conn.Run(stepCtx, step.query)
			elapsed := now().Sub(start)
			cancel()
			if err != nil {
				return doltLatencySummary{}, fmt.Errorf("query %s failed on %s: %w", step.name, addr, err)
			}
			samples = append(samples, elapsed)
			// Strictly greater keeps the first step at the max, so two equally
			// slow steps always name the earlier one.
			if slowest == "" || elapsed > slowestAt {
				slowest, slowestAt = step.name, elapsed
			}
		}
	}

	sum, err := summarizeDoltLatency(samples)
	if err != nil {
		return doltLatencySummary{}, err
	}
	sum.Slowest = slowest
	return sum, nil
}

// printDoltLatencySummary writes the run and its summary in the command's fixed
// text form.
func printDoltLatencySummary(w io.Writer, run doltLatencyRun, sum doltLatencySummary) {
	fmt.Fprintf(w, "host %s\n", run.Host)
	fmt.Fprintf(w, "port %d\n", run.Port)
	fmt.Fprintf(w, "samples %d\n", sum.Samples)
	fmt.Fprintf(w, "p50 %s\n", sum.P50)
	fmt.Fprintf(w, "p95 %s\n", sum.P95)
	fmt.Fprintf(w, "max %s\n", sum.Max)
}

// doltLatencyTo runs the probe and prints its summary to w.
func doltLatencyTo(w io.Writer, dial doltLatencyDialer, now func() time.Time, run doltLatencyRun) error {
	sum, err := runDoltLatencyProbe(context.Background(), dial, now, run)
	if err != nil {
		return err
	}
	printDoltLatencySummary(w, run, sum)
	return nil
}

// rowsIter is the part of *sql.Rows the probe reads, so drainRows can be
// tested without a server.
type rowsIter interface {
	Next() bool
	Err() error
	Close() error
}

// drainRows opens a result with open and reads every row of it, so the time
// the caller measures includes the rows. It returns the first error from
// opening or from reading, and closes the result.
func drainRows(open func() (rowsIter, error)) error {
	rows, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
	}
	return rows.Err()
}

// beadsLatencyConn is the real connection: a beadsql handle on one bd database.
type beadsLatencyConn struct {
	db *beadsql.DB
}

func (c beadsLatencyConn) Run(ctx context.Context, q beadsql.Query) error {
	return drainRows(func() (rowsIter, error) { return c.db.Query(ctx, q) })
}

func (c beadsLatencyConn) Close() error { return c.db.Close() }

// wrapBeadsLatencyConn turns a beadsql.Open result into the probe's connection,
// or returns the Open error.
func wrapBeadsLatencyConn(db *beadsql.DB, err error) (doltLatencyConn, error) {
	if err != nil {
		return nil, err
	}
	return beadsLatencyConn{db: db}, nil
}

// doltLatencyDSN is the DSN the probe opens database with on host:port. The
// user and password come from creds, the town's Dolt config; the host and port
// are the command's. The driver's formatter escapes the database name, so a
// reserved character in it cannot change the target. An empty creds.User is the
// default user, the rule Config.UserDSN applies; the DSN is never printed.
func doltLatencyDSN(host string, port int, database string, creds *doltserver.Config) string {
	cfg := doltserver.Config{Host: host, Port: port}
	m := mysql.NewConfig()
	m.User, m.Passwd = creds.User, creds.Password
	if m.User == "" {
		m.User = doltserver.DefaultUser
	}
	m.Net = "tcp"
	m.Addr = cfg.HostPort()
	m.DBName = database
	return m.FormatDSN()
}

// dialBeadsLatency opens a beadsql handle as the default user, for the
// dashboard's latency panel.
func dialBeadsLatency(ctx context.Context, host string, port int, database string) (doltLatencyConn, error) {
	return dialBeadsLatencyWithCredentials(ctx, host, port, database, &doltserver.Config{User: doltserver.DefaultUser})
}

// dialBeadsLatencyWithCredentials opens a beadsql handle on database at
// host:port as creds, the town's Dolt config. Its errors name only the
// database, from beadsql, and the driver's own dial error.
func dialBeadsLatencyWithCredentials(ctx context.Context, host string, port int, database string, creds *doltserver.Config) (doltLatencyConn, error) {
	return wrapBeadsLatencyConn(beadsql.Open(ctx, doltLatencyDSN(host, port, database, creds), database))
}

// doltLatencyCredentials returns the town's Dolt config, whose User and
// Password the latency DSN carries: the DefaultConfig every gt client reads.
func doltLatencyCredentials(findTownRoot func() (string, error)) (*doltserver.Config, error) {
	townRoot, err := findTownRoot()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return doltserver.DefaultConfig(townRoot), nil
}

// latencyDialerFor returns the probe's dialer, which opens as the town's Dolt
// user; outside a town it refuses rather than connecting as the default user.
func latencyDialerFor(findTownRoot func() (string, error)) doltLatencyDialer {
	return func(ctx context.Context, host string, port int, database string) (doltLatencyConn, error) {
		creds, err := doltLatencyCredentials(findTownRoot)
		if err != nil {
			return nil, err
		}
		return dialBeadsLatencyWithCredentials(ctx, host, port, database, creds)
	}
}

// `gt dolt latency` measures bd-style round trips against a Dolt SQL server and
// prints p50, p95 and max. It is the measurement behind the Dolt-in-Kubernetes
// gate: a bd round trip from the Mac, p95 under 20 ms. The query mix and the
// percentile method are fixed here. The connection and the clock are injected,
// so the arithmetic and the refusals are tested without a server.
var doltLatencyCmd = &cobra.Command{
	Use:   "latency",
	Short: "Measure bd-style query latency against a Dolt server",
	Long: `Run a fixed mix of read-only bd-style queries against a Dolt SQL server and
print the p50, p95 and max round-trip time.

Each round runs a list, a lookup by id and a count, in that order, on one
connection. Percentiles use the nearest-rank method. The command never writes,
and it exits non-zero when it cannot connect or records no samples.`,
	Args: cobra.NoArgs,
	RunE: runDoltLatency,
}

var (
	doltLatencyHost     string
	doltLatencyPort     int
	doltLatencyDatabase string
	doltLatencyRounds   int
)

func init() {
	doltLatencyCmd.Flags().StringVar(&doltLatencyHost, "host", "127.0.0.1", "Dolt server host")
	doltLatencyCmd.Flags().IntVar(&doltLatencyPort, "port", doltserver.DefaultPort, "Dolt server port")
	doltLatencyCmd.Flags().StringVar(&doltLatencyDatabase, "database", "", "bd database to query (required)")
	doltLatencyCmd.Flags().IntVar(&doltLatencyRounds, "rounds", 50, "rounds of the query mix to run")
	_ = doltLatencyCmd.MarkFlagRequired("database")
}

func runDoltLatency(cmd *cobra.Command, args []string) error {
	return doltLatencyTo(cmd.OutOrStdout(), latencyDialerFor(workspace.FindFromCwdOrError), time.Now, doltLatencyRun{
		Host:     doltLatencyHost,
		Port:     doltLatencyPort,
		Database: doltLatencyDatabase,
		Rounds:   doltLatencyRounds,
	})
}
