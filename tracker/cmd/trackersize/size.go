package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// tsLayout stamps a record with UTC at second precision, as the other
	// metering records are stamped.
	tsLayout = "2006-01-02T15:04:05Z"
	// dateLayout names a day file after its UTC date, so a run on either side of
	// midnight lands in the file for the day it reports.
	dateLayout = "2006-01-02"
	// partialSuffix names the file a run is still writing; the day file takes
	// its final name only once every line is in it.
	partialSuffix = ".partial"
	jsonlSuffix   = ".jsonl"
	// dsnEnv names the variable a run reads its tracker DSN from. The DSN carries
	// a password, and an argument would show in every process listing.
	dsnEnv = "TRACKERSIZE_DSN"
)

// PrefixSize is one issue-id prefix's totals: its issue rows and the bytes of
// the text columns they carry.
type PrefixSize struct {
	Prefix string
	Rows   int64
	Bytes  int64
}

// Querier returns the tracker's size per issue-id prefix.
type Querier interface {
	Sizes(ctx context.Context) ([]PrefixSize, error)
}

// Clock supplies the time a report is stamped and named with.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// record is one line of a day file, in the field order the readers parse.
type record struct {
	TS        string `json:"ts"`
	Workspace string `json:"workspace"`
	Prefix    string `json:"prefix"`
	Rows      int64  `json:"rows"`
	Bytes     int64  `json:"bytes"`
}

// options is what one run was told: the database to read, the workspace it
// belongs to, and where the day files go.
type options struct {
	dsn       string
	workspace string
	out       string
}

// run is trackersize.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, err := parse(args, os.Getenv, stderr)
	if err != nil {
		return err
	}
	return report(ctx, opts, pgQuerier{dsn: opts.dsn}, systemClock{}, stdout)
}

// report writes today's day file for one workspace. The query runs first, so a
// failed read writes nothing at all.
func report(ctx context.Context, opts options, q Querier, clock Clock, stdout io.Writer) error {
	sizes, err := q.Sizes(ctx)
	if err != nil {
		return fmt.Errorf("query the tracker: %w", err)
	}
	// The query's order is its own business; sorting here makes the file the
	// same whatever order a Querier returns its prefixes in.
	slices.SortFunc(sizes, func(a, b PrefixSize) int { return strings.Compare(a.Prefix, b.Prefix) })

	now := clock.Now().UTC()
	lines := make([][]byte, 0, len(sizes))
	for _, s := range sizes {
		line, err := json.Marshal(record{
			TS:        now.Format(tsLayout),
			Workspace: opts.workspace,
			Prefix:    s.Prefix,
			Rows:      s.Rows,
			Bytes:     s.Bytes,
		})
		if err != nil {
			// A record of a string and two ints always encodes; failing here is
			// a defect rather than a state a nightly run can reach.
			return fmt.Errorf("encode the record for prefix %s: %w", s.Prefix, err)
		}
		lines = append(lines, line)
	}
	path := filepath.Join(opts.out, now.Format(dateLayout)+jsonlSuffix)
	if err := writeDay(path, lines); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s (%d prefixes)\n", path, len(sizes))
	return nil
}

// writeDay publishes lines as the day file at path: they are written to a
// partial name and renamed, so a reader never opens half a report and a run that
// dies mid-write leaves no day file to read.
func writeDay(path string, lines [][]byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the output directory %s: %w", dir, err)
	}
	partial := path + partialSuffix
	// A leftover path of any kind is removed first and the file is created
	// exclusively: a symlink planted at the partial name cannot be followed into
	// another file.
	if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear %s: %w", partial, err)
	}
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600) //nolint:gosec // G304: partial is built from the -out flag
	if err != nil {
		return fmt.Errorf("create %s: %w", partial, err)
	}
	var buf []byte
	for _, line := range lines {
		buf = append(append(buf, line...), '\n')
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		_ = os.Remove(partial)
		return fmt.Errorf("write %s: %w", partial, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("write %s: %w", partial, err)
	}
	if err := os.Rename(partial, path); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("publish %s: %w", path, err)
	}
	return nil
}

// parse reads the flags -out and -workspace, and the DSN from the dsnEnv
// variable that getenv returns. A DSN given as an argument is refused, and no
// error repeats the DSN. The checks come before the read, so a report is only
// attempted from a run that named where its database, its workspace and its
// files are.
func parse(args []string, getenv func(string) string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("trackersize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s=<dsn> trackersize -out DIR -workspace ID\n", dsnEnv)
		fs.PrintDefaults()
	}
	out := fs.String("out", "", "directory that holds the day files; created 0700 when missing")
	workspace := fs.String("workspace", "", "workspace the report is filed under")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return options{}, fmt.Errorf("the DSN is not an argument: set %s instead, since an argument shows in process listings", dsnEnv)
	}
	dsn := getenv(dsnEnv)
	if dsn == "" {
		return options{}, fmt.Errorf("%s is required: it names the tracker database a run reads", dsnEnv)
	}
	if *workspace == "" {
		return options{}, errors.New("-workspace is required: a report is filed under one workspace id")
	}
	if *out == "" {
		return options{}, errors.New("-out is required: it names the directory the day files go in")
	}
	return options{dsn: dsn, workspace: *workspace, out: *out}, nil
}
