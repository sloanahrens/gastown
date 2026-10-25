package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	_ "github.com/go-sql-driver/mysql" // the Dolt (MySQL protocol) driver
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/migrate"
)

// rigNames is a repeatable -rig-name prefix=name flag.
type rigNames map[string]string

func (r rigNames) String() string { return fmt.Sprint(map[string]string(r)) }

func (r rigNames) Set(v string) error {
	prefix, name, ok := strings.Cut(v, "=")
	if !ok || prefix == "" || name == "" {
		return fmt.Errorf("-rig-name wants prefix=name, got %q", v)
	}
	r[prefix] = name
	return nil
}

// run is trackerimport. Both DSNs hold passwords, so they come only from the
// environment and appear in no output or error.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	return runWith(ctx, args, getenv, stdout, stderr, readDolt)
}

// readDolt opens the Dolt database named by dsn and reads it.
func readDolt(ctx context.Context, dsn string) (*migrate.Dataset, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, errors.New("cannot use DOLT_DSN as a connection string")
	}
	defer db.Close()
	return migrate.Read(ctx, db)
}

// runWith is run with the Dolt reader injected, so tests need no Dolt server.
func runWith(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, read func(context.Context, string) (*migrate.Dataset, error)) error {
	fs := flag.NewFlagSet("trackerimport", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dry := fs.Bool("dry-run", false, "read Dolt and report what would be imported; write nothing")
	names := rigNames{}
	fs.Var(names, "rig-name", "display name of a rig, as prefix=name (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	doltDSN, trackerDSN := getenv("DOLT_DSN"), getenv("TRACKER_DSN")
	if doltDSN == "" {
		return errors.New("DOLT_DSN is not set (user:password@tcp(host:port)/database)")
	}
	if trackerDSN == "" && !*dry {
		return errors.New("TRACKER_DSN is not set (a Postgres connection string)")
	}

	ds, err := read(ctx, doltDSN)
	if err != nil {
		return err
	}
	wisps := 0
	for _, is := range ds.Issues {
		if is.Wisp {
			wisps++
		}
	}
	fmt.Fprintf(stdout, "read: %d issues (%d wisps), %d links, %d labels, %d comments, %d events, %d config keys\n",
		len(ds.Issues), wisps, len(ds.Deps), len(ds.Labels), len(ds.Comments), len(ds.Events), len(ds.Config))
	if *dry {
		fmt.Fprintln(stdout, "dry run: nothing written")
		return nil
	}

	pool, err := pgxpool.New(ctx, trackerDSN)
	if err != nil {
		return errors.New("cannot use TRACKER_DSN as a connection string")
	}
	defer pool.Close()
	rep, err := migrate.Import(ctx, pool, ds, migrate.Options{RigNames: names})
	var ve *migrate.VerifyError
	if errors.As(err, &ve) {
		for _, p := range ve.Diff.Problems {
			fmt.Fprintln(stdout, "DIFF:", p)
		}
		return err // nothing was committed
	}
	if err != nil {
		return err
	}
	printCounts(stdout, "written", rep.Written)
	printCounts(stdout, "skipped", rep.Skipped)
	// The import verified itself before committing; check the committed
	// state once more, from the pool, as the final word.
	diff, err := migrate.Verify(ctx, pool, ds)
	if err != nil {
		return err
	}
	if !diff.Equal() {
		for _, p := range diff.Problems {
			fmt.Fprintln(stdout, "DIFF:", p)
		}
		return fmt.Errorf("the committed data differs from Dolt in %d place(s) although the import verified before commit; investigate before using it", diff.Total)
	}
	fmt.Fprintln(stdout, "verified: the tracker holds what Dolt held")
	return nil
}

func printCounts(w io.Writer, what string, m map[string]int) {
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s: %-45s %d\n", what, k, m[k])
	}
}
