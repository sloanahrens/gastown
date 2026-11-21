package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/workspace"
)

// doltUsersQuery is the one statement the command issues. It asks the server to
// compare each authentication string with the empty string, so the string
// itself never leaves the server and cannot reach this process's memory, its
// output or its logs.
const doltUsersQuery = "SELECT user, host, authentication_string = '' FROM mysql.user ORDER BY user, host"

// emptyPasswordFlag is the answer to the empty-password predicate. The server
// types the comparison TINY and go-sql-driver/mysql (textRows.readRow, packets.go
// lines 854 to 862) hands an integer column to database/sql as int64, or uint64
// for an unsigned BIGINT, so only those two shapes with the value 0 or 1 are
// read. Every other shape is refused on purpose, a []byte and a string included,
// even when bool parsing would accept it ("1", "true", "0"): a statement changed
// to select the authentication string then fails to scan instead of reporting
// the account as weak or strong. A NULL leaves Valid false, so the reader can
// tell "the server did not say" from "password set".
type emptyPasswordFlag struct {
	Bool  bool
	Valid bool
}

func (f *emptyPasswordFlag) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		// Valid stays false, so ReadAccounts refuses the row and names the
		// account instead of reading an unknown answer as "password set".
		return nil
	case int64:
		if v == 0 || v == 1 {
			f.Bool, f.Valid = v == 1, true
			return nil
		}
	case uint64:
		if v == 0 || v == 1 {
			f.Bool, f.Valid = v == 1, true
			return nil
		}
	}
	return errors.New("the empty-password predicate answered with something other than 0 or 1")
}

// doltUserAccount is one account the server holds. EmptyPassword is whether the
// server says the password is empty; it is never the password or its hash.
type doltUserAccount struct {
	User          string
	Host          string
	EmptyPassword bool
}

// doltUserAccountName is the account as the command writes it. The user and
// host come from the server, so control characters are replaced before they
// reach a terminal.
func doltUserAccountName(a doltUserAccount) string {
	return sanitizeDoltTopCell(a.User) + "@" + sanitizeDoltTopCell(a.Host)
}

// doltUsersReader reads the server's accounts. ReadAccounts returns one entry
// per account, or an error: an empty slice with a nil error means the server
// answered with no account, which the caller refuses.
type doltUsersReader interface {
	ReadAccounts(ctx context.Context) ([]doltUserAccount, error)
	Close() error
}

// mysqlUsersReader reads mysql.user over one MySQL connection.
type mysqlUsersReader struct {
	db *sql.DB
}

func (r *mysqlUsersReader) ReadAccounts(ctx context.Context) ([]doltUserAccount, error) {
	rows, err := r.db.QueryContext(ctx, doltUsersQuery)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var accounts []doltUserAccount
	for rows.Next() {
		var user, host sql.NullString
		var empty emptyPasswordFlag
		if err := rows.Scan(&user, &host, &empty); err != nil {
			// The scan target refuses any string, so a statement changed to
			// select the authentication string fails here. database/sql puts
			// the value it could not convert in its error, so the driver's
			// message is dropped and the shape of the row is named instead.
			return nil, errors.New("a row from mysql.user did not scan into a user, a host and an empty-password flag")
		}
		account := doltUserAccount{User: user.String, Host: host.String}
		// The statement cannot return NULL here; a NULL means the answer is
		// unknown, and reading it as "password set" would pass a weak account
		// as a strong one.
		if !empty.Valid {
			return nil, fmt.Errorf("the server did not say whether the password of %s is empty", doltUserAccountName(account))
		}
		account.EmptyPassword = empty.Bool
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (r *mysqlUsersReader) Close() error { return r.db.Close() }

// doltUsersOpener finds the town and opens the connection to its Dolt server.
// The town root lookup and the driver are fields, so a test drives the
// refusals and the successful open without a workspace or a server.
type doltUsersOpener struct {
	findTownRoot func() (string, error)
	openDB       func(dsn string) (*sql.DB, error)
}

// open connects to the town's Dolt server. The DSN comes from
// buildDoltDSNFromConfig, so the command reaches the server the same way the
// other gt commands do, and an error names the address but never the
// credential.
func (o doltUsersOpener) open(ctx context.Context) (doltUsersReader, error) {
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
	return &mysqlUsersReader{db: db}, nil
}

// doltUsersDeps is the command's contact with the outside world: the reader the
// accounts come from and the stream they are written to. A test supplies fakes,
// so no unit test opens a connection.
type doltUsersDeps struct {
	open func(ctx context.Context) (doltUsersReader, error)
	out  io.Writer
}

// runDoltUsers prints one line per account and a summary. With check set it
// also returns an error naming every account whose password is empty, which
// makes gt exit 1. A server it cannot reach, a read that fails, no account at
// all and a write that fails each return an error before or instead of an
// account list: a list that is missing or partial would read as a complete one.
func runDoltUsers(ctx context.Context, deps doltUsersDeps, check bool) error {
	reader, err := deps.open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()

	accounts, err := reader.ReadAccounts(ctx)
	if err != nil {
		return fmt.Errorf("reading the accounts from mysql.user: %w", err)
	}
	if len(accounts) == 0 {
		return errors.New("the server listed no accounts in mysql.user")
	}
	if err := printDoltUsers(deps.out, accounts); err != nil {
		return err
	}
	if !check {
		return nil
	}

	var weak []string
	for _, a := range accounts {
		if a.EmptyPassword {
			weak = append(weak, doltUserAccountName(a))
		}
	}
	if len(weak) == 0 {
		return nil
	}
	return fmt.Errorf("accounts with an empty password: %s", strings.Join(weak, ", "))
}

// printDoltUsers writes one line per account and the summary. Names are padded
// to the widest, so the password column lines up and a weak account is one the
// reader can pick out.
func printDoltUsers(w io.Writer, accounts []doltUserAccount) error {
	names := make([]string, len(accounts))
	width := 0
	empty := 0
	for i, a := range accounts {
		names[i] = doltUserAccountName(a)
		if n := len([]rune(names[i])); n > width {
			width = n
		}
		if a.EmptyPassword {
			empty++
		}
	}

	for i, a := range accounts {
		state := "set"
		if a.EmptyPassword {
			state = "empty"
		}
		if _, err := fmt.Fprintf(w, "%s  password: %s\n", padDoltTopCell(names[i], width, false), state); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, doltUsersSummary(len(accounts), empty))
	return err
}

// doltUsersSummary counts the accounts and the empty passwords among them.
func doltUsersSummary(total, empty int) string {
	noun := "accounts"
	if total == 1 {
		noun = "account"
	}
	if empty == 0 {
		return fmt.Sprintf("%d %s, none with an empty password", total, noun)
	}
	return fmt.Sprintf("%d %s, %d with an empty password", total, noun, empty)
}

// doltUsersRealDeps is doltUsersDeps on the real machine.
func doltUsersRealDeps(out io.Writer) doltUsersDeps {
	opener := doltUsersOpener{
		findTownRoot: workspace.FindFromCwdOrError,
		openDB:       func(dsn string) (*sql.DB, error) { return sql.Open("mysql", dsn) },
	}
	return doltUsersDeps{open: opener.open, out: out}
}

var doltUsersCheck bool

// `gt dolt users` reports the server's accounts and which of them have no
// password. A Dolt server starts with a root account whose password is empty,
// so that root is the state an operator has to see before setting one.
var doltUsersCmd = &cobra.Command{
	Use:   "users",
	Short: "List the Dolt server's accounts and whether each password is empty",
	Long: `List the accounts in the Dolt server's mysql.user and say whether each
password is empty.

Each line names one account as user@host and prints "password: empty" or
"password: set", then one summary line counts the accounts and the empty
passwords among them.

The command asks the server to compare each authentication string with the
empty string, so the string itself is never read, printed or logged.

With --check the command exits 1 when any account has an empty password, and
the error names those accounts. Use it before exposing the server, or after a
change to its accounts.

The command only reads. It never changes an account, a password or a grant.

Example:
  gt dolt users --check`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runDoltUsers(cmd.Context(), doltUsersRealDeps(cmd.OutOrStdout()), doltUsersCheck)
	},
}

func init() {
	doltUsersCmd.Flags().BoolVar(&doltUsersCheck, "check", false, "exit 1 when any account has an empty password")
}
