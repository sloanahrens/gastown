package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/workspace"
)

// doltTopProcessListQuery is the one statement the command issues. It is a
// read of the server's own connection table, so it names no database and
// writes nothing. Every row carries ID, and CONNECTION_ID() repeats the id of
// the connection that ran the query: this query is itself in the process list
// while it runs, so the reader drops the row whose ID equals that value, or the
// command would rank its own read as the busiest statement.
const doltTopProcessListQuery = "SELECT `ID`, `USER`, `HOST`, `TIME`, `INFO`, CONNECTION_ID() FROM information_schema.processlist"

// doltTopMinInterval is the shortest gap between two reads. A shorter gap
// samples the same statements again rather than a busier moment, and a zero
// or negative one spins the loop against the server being measured.
const doltTopMinInterval = 100 * time.Millisecond

// doltTopMinStatementWidth is the room the statement column keeps while the
// user and host columns are cut to fit a narrow output: enough to see the shape
// of a query. When even that does not fit, each line is cut to the width.
const doltTopMinStatementWidth = 20

// doltTopMinNameWidth is the narrowest the user and host columns are cut to,
// the width of their headers.
const doltTopMinNameWidth = 4

// doltTopLiteralListRE matches a run of placeholders separated by commas. An
// IN list and a multi-row VALUES list are one shape whatever their length, so
// the run collapses to a single placeholder.
var doltTopLiteralListRE = regexp.MustCompile(`\?(?:\s*,\s*\?)+`)

// doltTopSample is one row of one process-list read.
type doltTopSample struct {
	User      string
	Host      string
	Statement string // the raw INFO column, empty for an idle connection
	// Time is whole seconds, so a statement that runs in milliseconds reads 0
	// however often it runs. It is carried for fidelity with the server's row
	// and never ranked; aggregateSamples counts reads instead (gt-tesr3).
	Time float64
}

// doltTopRow is one statement shape as one client host ran it, counted over
// the reads it appeared in.
type doltTopRow struct {
	User      string
	Host      string
	Statement string // normalized by normalizeStatement
	Samples   int    // reads in which this shape was running on this host
}

// normalizeStatement reduces a statement to its shape: string and number
// literals become "?", a comma-separated run of placeholders collapses to one,
// whitespace folds to a single space and comments drop. Two statements of one
// shape therefore print as one row, which is what makes the ranking read as
// "this query" rather than "this call".
func normalizeStatement(stmt string) string {
	r := []rune(stmt)
	var b strings.Builder
	var prev rune
	write := func(c rune) {
		b.WriteRune(c)
		prev = c
	}
	writeSpace := func() {
		if b.Len() > 0 && prev != ' ' {
			write(' ')
		}
	}

	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			writeSpace()
			i++
		case c == '-' && i+1 < len(r) && r[i+1] == '-' && (i+2 == len(r) || unicode.IsSpace(r[i+2])):
			for i < len(r) && r[i] != '\n' {
				i++
			}
		case c == '#':
			for i < len(r) && r[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(r) && r[i+1] == '*':
			i += 2
			for i < len(r) && !(r[i] == '*' && i+1 < len(r) && r[i+1] == '/') {
				i++
			}
			i += 2
		case c == '\'' || c == '"':
			i = skipDoltTopQuoted(r, i)
			write('?')
		case c == '`':
			end := skipDoltTopQuoted(r, i)
			for _, id := range r[i:end] {
				write(id)
			}
			i = end
		case unicode.IsDigit(c) && !isDoltTopIdentRune(prev):
			i = scanDoltTopNumber(r, i)
			write('?')
		default:
			write(c)
			i++
		}
	}

	out := strings.TrimSpace(b.String())
	return doltTopLiteralListRE.ReplaceAllString(out, "?")
}

// skipDoltTopQuoted returns the index just past the literal that opens at r[i].
// In a quoted string a backslash escapes the next rune; in a quoted identifier
// it is literal and only a doubled backtick escapes a backtick, so an
// identifier ending in a backslash still closes. An unterminated literal runs
// to the end, so a truncated INFO column still normalizes.
func skipDoltTopQuoted(r []rune, i int) int {
	quote := r[i]
	for j := i + 1; j < len(r); j++ {
		switch r[j] {
		case '\\':
			if quote != '`' {
				j++
			}
		case quote:
			if j+1 < len(r) && r[j+1] == quote {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(r)
}

// scanDoltTopNumber returns the index just past the numeric literal at r[i]:
// decimal with an optional fraction and exponent, or a 0x hex literal.
func scanDoltTopNumber(r []rune, i int) int {
	if r[i] == '0' && i+1 < len(r) && (r[i+1] == 'x' || r[i+1] == 'X') {
		j := i + 2
		for j < len(r) && isDoltTopHexRune(r[j]) {
			j++
		}
		if j > i+2 {
			return j
		}
	}
	j := i
	for j < len(r) && unicode.IsDigit(r[j]) {
		j++
	}
	if j+1 < len(r) && r[j] == '.' && unicode.IsDigit(r[j+1]) {
		j++
		for j < len(r) && unicode.IsDigit(r[j]) {
			j++
		}
	}
	if j < len(r) && (r[j] == 'e' || r[j] == 'E') {
		k := j + 1
		if k < len(r) && (r[k] == '+' || r[k] == '-') {
			k++
		}
		if k < len(r) && unicode.IsDigit(r[k]) {
			for k < len(r) && unicode.IsDigit(r[k]) {
				k++
			}
			j = k
		}
	}
	return j
}

func isDoltTopIdentRune(c rune) bool {
	return c == '_' || c == '$' || unicode.IsLetter(c) || unicode.IsDigit(c)
}

func isDoltTopHexRune(c rune) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// aggregateSamples ranks statement shapes by the number of reads in which they
// were running. Process-list TIME is whole seconds and restarts with each
// statement, so a short statement that runs often reads as zero however much
// server time it spends; the count of reads is what a caller's busy time looks
// like from outside (gt-tesr3). A read counts once per shape, host and user
// even when that host runs the shape on several connections, so a row's count
// is out of the reads taken. Keyword case does not split a shape: "select 1"
// and "SELECT 1" group together, and the row prints the spelling seen first.
// Rows report the count, then user, host and statement, so two runs over the
// same readings print the same order. An idle connection has no statement to
// rank and is left out.
func aggregateSamples(readings [][]doltTopSample) []doltTopRow {
	type groupKey struct{ user, host, statement string }
	groups := make(map[groupKey]*doltTopRow)

	for _, reading := range readings {
		counted := make(map[groupKey]bool, len(reading))
		for _, s := range reading {
			stmt := normalizeStatement(s.Statement)
			if stmt == "" {
				continue
			}
			k := groupKey{user: s.User, host: s.Host, statement: strings.ToLower(stmt)}
			if counted[k] {
				continue
			}
			counted[k] = true
			row, ok := groups[k]
			if !ok {
				row = &doltTopRow{User: s.User, Host: s.Host, Statement: stmt}
				groups[k] = row
			}
			row.Samples++
		}
	}

	rows := make([]doltTopRow, 0, len(groups))
	for _, row := range groups {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Samples != b.Samples {
			return a.Samples > b.Samples
		}
		if a.User != b.User {
			return a.User < b.User
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.Statement < b.Statement
	})
	return rows
}

// renderTop writes the sample window and then rows as a table whose last
// column ends at width. Each row's SEEN cell says how many of the reads saw
// it, and EST is that count times the interval, so the table never claims more
// precision than whole-second sampling gives. No line is wider than width: a
// statement too long for its column is cut and marked with an ellipsis, the
// host and user columns give up room when the statement would get under
// doltTopMinStatementWidth, and a line that still does not fit is cut.
func renderTop(w io.Writer, rows []doltTopRow, width int, samples int, interval time.Duration) error {
	clean := make([]doltTopRow, len(rows))
	for i, row := range rows {
		clean[i] = doltTopRow{
			User:      sanitizeDoltTopCell(row.User),
			Host:      sanitizeDoltTopCell(row.Host),
			Statement: sanitizeDoltTopCell(row.Statement),
			Samples:   row.Samples,
		}
	}

	seenWidth := len("SEEN")
	estWidth := len("EST")
	userWidth, hostWidth := len("USER"), len("HOST")
	for _, row := range clean {
		if n := len([]rune(row.User)); n > userWidth {
			userWidth = n
		}
		if n := len([]rune(row.Host)); n > hostWidth {
			hostWidth = n
		}
		if n := len([]rune(seenText(row.Samples, samples))); n > seenWidth {
			seenWidth = n
		}
		if n := len([]rune(estText(row.Samples, interval))); n > estWidth {
			estWidth = n
		}
	}
	if width < 1 {
		width = 80
	}
	// The five columns are separated by two spaces. The statement takes what is
	// left; when that is under doltTopMinStatementWidth the host column, then the
	// user column, give up room down to the width of their headers, and a cell
	// that no longer fits is cut with an ellipsis.
	const separators = 8
	statementWidth := width - (userWidth + hostWidth + seenWidth + estWidth + separators)
	if statementWidth < doltTopMinStatementWidth {
		need := doltTopMinStatementWidth - statementWidth
		cut := min(need, hostWidth-doltTopMinNameWidth)
		hostWidth -= cut
		need -= cut
		cut = min(need, userWidth-doltTopMinNameWidth)
		userWidth -= cut
		statementWidth = width - (userWidth + hostWidth + seenWidth + estWidth + separators)
	}
	if statementWidth < 1 {
		statementWidth = 1
	}

	// Every line is cut to the width as a last step, so a terminal narrower
	// than the fixed columns still gets lines that fit.
	writeLine := func(user, host, seen, est, statement string) error {
		line := fmt.Sprintf("%s  %s  %s  %s  %s",
			padDoltTopCell(cutToWidth(user, userWidth), userWidth, false),
			padDoltTopCell(cutToWidth(host, hostWidth), hostWidth, false),
			padDoltTopCell(seen, seenWidth, true), padDoltTopCell(est, estWidth, true),
			cutToWidth(statement, statementWidth))
		_, err := fmt.Fprintln(w, cutToWidth(line, width))
		return err
	}

	if _, err := fmt.Fprintln(w, cutToWidth(fmt.Sprintf("sampled %d times at %s; EST is reads seen x interval",
		samples, interval), width)); err != nil {
		return err
	}
	if err := writeLine("USER", "HOST", "SEEN", "EST", "STATEMENT"); err != nil {
		return err
	}
	for _, row := range clean {
		if err := writeLine(row.User, row.Host,
			seenText(row.Samples, samples), estText(row.Samples, interval), row.Statement); err != nil {
			return err
		}
	}
	return nil
}

// seenText and estText are the cells the width is measured on and printed
// from, so a count too wide for its header still lines up.
func seenText(samples, total int) string { return fmt.Sprintf("%d/%d", samples, total) }

func estText(samples int, interval time.Duration) string {
	return (time.Duration(samples) * interval).String()
}

// padDoltTopCell pads s to width runes, on the left when right is true. The
// fmt verbs pad by byte, which misaligns a column the moment a user or host
// holds a non-ASCII rune, and a table whose columns move cannot be read.
func padDoltTopCell(s string, width int, right bool) string {
	pad := width - len([]rune(s))
	if pad <= 0 {
		return s
	}
	if right {
		return strings.Repeat(" ", pad) + s
	}
	return s + strings.Repeat(" ", pad)
}

// sanitizeDoltTopCell replaces control characters with a space. The statement
// text, user and host are written by the client, not by this command, so an
// escape sequence in them would act on the terminal that prints the table.
func sanitizeDoltTopCell(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
}

// cutToWidth returns s cut to at most width runes, ending in an ellipsis when
// it was cut.
func cutToWidth(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width < 1 {
		return ""
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// doltTopReader is one snapshot of the server's process list. ReadProcessList
// returns every connection the server lists except the reader's own, which is
// removed here because the query that reads the list is itself running and
// would rank first. Idle connections come back with an empty Statement and are
// left out later, by the aggregation. Close releases the connection.
type doltTopReader interface {
	ReadProcessList(ctx context.Context) ([]doltTopSample, error)
	Close() error
}

// mysqlTopReader reads information_schema.processlist over one MySQL
// connection.
type mysqlTopReader struct {
	db *sql.DB
}

func (r *mysqlTopReader) ReadProcessList(ctx context.Context) ([]doltTopSample, error) {
	rows, err := r.db.QueryContext(ctx, doltTopProcessListQuery)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var samples []doltTopSample
	for rows.Next() {
		var user, host, info sql.NullString
		var elapsed sql.NullFloat64
		var id, self sql.NullInt64
		if err := rows.Scan(&id, &user, &host, &elapsed, &info, &self); err != nil {
			return nil, err
		}
		if isOwnConnection(id, self) {
			continue
		}
		samples = append(samples, doltTopSample{
			User:      user.String,
			Host:      host.String,
			Statement: info.String,
			Time:      elapsed.Float64,
		})
	}
	return samples, rows.Err()
}

// isOwnConnection reports whether a process-list row belongs to the connection
// that ran the query. A row whose ID or CONNECTION_ID() the server left NULL
// is kept: dropping a row it cannot name would hide a real statement.
func isOwnConnection(id, self sql.NullInt64) bool {
	return id.Valid && self.Valid && id.Int64 == self.Int64
}

func (r *mysqlTopReader) Close() error { return r.db.Close() }

// doltTopOpener finds the town and opens the connection to its Dolt server.
// The town root lookup and the driver are fields, so a test drives the
// refusals and the successful open without a workspace or a server.
type doltTopOpener struct {
	findTownRoot func() (string, error)
	openDB       func(dsn string) (*sql.DB, error)
}

// open connects to the town's Dolt server. The DSN comes from
// buildDoltDSNFromConfig, so the command reaches the server the same way the
// other gt commands do, and an error names the address but never the
// credential.
func (o doltTopOpener) open(ctx context.Context) (doltTopReader, error) {
	townRoot, err := o.findTownRoot()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	config := doltserver.DefaultConfig(townRoot)
	dsn := buildDoltDSNFromConfig(config, "", dsnOpts{Timeout: "5s", ReadTimeout: "30s"})
	db, err := o.openDB(dsn)
	if err != nil {
		return nil, fmt.Errorf("opening a connection to %s: %w", config.HostPort(), err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connecting to the Dolt server at %s: %w", config.HostPort(), err)
	}
	return &mysqlTopReader{db: db}, nil
}

// doltTopDeps is the command's contact with the outside world: the reader the
// samples come from, the clock the wait between two reads runs on, and the
// stream the table is written to. A test supplies fakes, so no unit test opens
// a connection or spends real time.
type doltTopDeps struct {
	open  func(ctx context.Context) (doltTopReader, error)
	sleep func(ctx context.Context, d time.Duration) error
	out   io.Writer
}

// runDoltTop reads the process list samples times, waiting interval between
// reads, and prints the limit busiest statement shapes. A flag out of range, a
// server it cannot reach and a read that fails each return an error and print
// no table: a partial table would read as a complete one.
func runDoltTop(ctx context.Context, deps doltTopDeps, samples int, interval time.Duration, limit int) error {
	if samples < 1 {
		return fmt.Errorf("--samples must be at least 1, got %d", samples)
	}
	if interval < doltTopMinInterval {
		return fmt.Errorf("--interval must be at least %s, got %s", doltTopMinInterval, interval)
	}
	if limit < 1 {
		return fmt.Errorf("--limit must be at least 1, got %d", limit)
	}

	reader, err := deps.open(ctx)
	if err != nil {
		return err
	}
	// The summary is decided before the connection closes, so a close error
	// cannot change it.
	defer func() { _ = reader.Close() }()

	var readings [][]doltTopSample
	for i := 0; i < samples; i++ {
		if i > 0 {
			if err := deps.sleep(ctx, interval); err != nil {
				return err
			}
		}
		got, err := reader.ReadProcessList(ctx)
		if err != nil {
			return fmt.Errorf("reading the process list: %w", err)
		}
		readings = append(readings, got)
	}

	rows := aggregateSamples(readings)
	if len(rows) == 0 {
		_, err := fmt.Fprintf(deps.out, "no statements sampled in %d process-list reads at %s\n", samples, interval)
		return err
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return renderTop(deps.out, rows, tailDisplayWidth(deps.out), samples, interval)
}

// doltTopRealDeps is doltTopDeps on the real machine.
func doltTopRealDeps(out io.Writer) doltTopDeps {
	opener := doltTopOpener{
		findTownRoot: workspace.FindFromCwdOrError,
		openDB:       func(dsn string) (*sql.DB, error) { return sql.Open("mysql", dsn) },
	}
	return doltTopDeps{
		open:  opener.open,
		sleep: sleepDoltTop,
		out:   out,
	}
}

// sleepDoltTop waits d or until ctx is done, whichever comes first.
func sleepDoltTop(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var (
	doltTopSamples  int
	doltTopInterval time.Duration
	doltTopLimit    int
)

// `gt dolt top` ranks what keeps the town's Dolt server busy. The server, not
// a caller, is the subject: the process list says which statement shapes are
// running and which client host is running them, so the first caller to cut is
// the one at the top.
var doltTopCmd = &cobra.Command{
	Use:   "top",
	Short: "Rank the statements that keep the Dolt server busy",
	Long: `Sample the Dolt server's process list and rank the statements that keep it busy.

Each read is one query of information_schema.processlist; the command reads it
--samples times, waiting --interval between reads, and prints the --limit
busiest rows as user, client host, reads seen, an estimate and the statement.

A row ranks by the number of reads that saw it, and its EST is that count times
the interval: a rough measure of how long the shape held the server. The
process list's TIME column is not used, because it is whole seconds and starts
again with each statement, so a statement that runs in a few milliseconds reads
zero however often it runs.

Statements are grouped by shape, so two calls that differ only in their
literals are one row: literals become '?', an IN list collapses to one
placeholder and comments drop.

When no read sees a running statement the command prints one line saying so.

The command only reads. It never kills a query, never writes and never touches
the server's configuration.

A row's host is the client's address:port from the server's point of view, so
one process's several connections appear as several hosts.

Example:
  gt dolt top --samples 60 --interval 1s --limit 20`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runDoltTop(cmd.Context(), doltTopRealDeps(cmd.OutOrStdout()), doltTopSamples, doltTopInterval, doltTopLimit)
	},
}

func init() {
	doltTopCmd.Flags().IntVar(&doltTopSamples, "samples", 30, "number of process-list reads to take")
	doltTopCmd.Flags().DurationVar(&doltTopInterval, "interval", time.Second, "gap between two process-list reads (minimum 100ms)")
	doltTopCmd.Flags().IntVar(&doltTopLimit, "limit", 15, "number of rows to print")
}
