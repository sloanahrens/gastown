// Command bdshim-record runs the real bd for every row of the golden table against a fixture
// database, and writes one golden per row: <verb>-<flags>.json holds bd's stdout and
// <verb>-<flags>.exit holds its exit code. The bd shim is tested against these, so it answers the
// way bd really does (gt-p85pw.87).
//
//	bdshim-record -fixture <JSONL file of beads> [-bd bd] [-out cmd/bdshim/testdata/golden] [-scratch scratch] [-check]
//	bdshim-record -flags [-bd bd] [-out internal/bdshim/testdata/bd-flags.txt] [-check]
//
// With -flags it records the value flags bd's help documents for each command, which the shim's
// tables are checked against, instead of the golden rows.
//
// Every row runs in a copy of the fixture: a directory under -scratch holding a database of its
// own, made with bd init and filled from -fixture. The read rows share one copy and each mutating
// row gets one of its own, so no row sees another's writes; each copy's database is dropped and its
// directory removed before the command exits. bd reaches the Dolt server through the environment
// (BEADS_DOLT_SERVER_PORT and friends), which passes through to every run, and a container server
// needs BEADS_TEST_SERVER=1 there as bd itself does. Before any row, bd is asked for each bead the
// fixture must hold and each verb the table names, so a fixture or a table bd cannot answer fails
// the run. With -check the table runs again and the goldens that differ, and those of rows the table
// no longer has, are listed on stdout; nothing is written, and the exit code is 1 when any are
// listed.
//
// -scratch must sit outside any .beads tree. bd init walks up from the copy it runs in, refuses a
// tree that already holds a workspace, and on the way rewrites the first store it finds, so the
// recorder refuses to run when its working directory or the scratch directory sits in one
// (gt-p85pw.103). A scratch directory that is its own git repository works, since no directory above
// it holds a workspace.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// options is the parsed command line.
type options struct {
	bd      string
	fixture string
	out     string
	scratch string
	check   bool
	flags   bool
}

func run(args []string, stdout, stderr io.Writer) int {
	return runIn(os.Getwd, args, stdout, stderr)
}

// runIn is run with the working directory injected, so a test puts the recorder in a tree of its own
// without changing the process's directory.
func runIn(getwd func() (string, error), args []string, stdout, stderr io.Writer) int {
	var opts options
	flags := flag.NewFlagSet("bdshim-record", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.bd, "bd", "bd", "the bd binary to run")
	flags.StringVar(&opts.fixture, "fixture", "", "the fixture file of beads, one JSONL line per bead, that every copy's database is imported from")
	flags.StringVar(&opts.out, "out", "", "what the recording is written to: the goldens' directory, or the flags file with -flags (defaults are per mode)")
	flags.StringVar(&opts.scratch, "scratch", "scratch", "the directory fresh fixture copies are made in and removed from")
	flags.BoolVar(&opts.check, "check", false, "re-run the recording and report the goldens or flag rows that differ, writing nothing")
	flags.BoolVar(&opts.flags, "flags", false, "record the value flags bd documents per command, not the goldens")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	scratch := opts.scratch
	if opts.flags {
		scratch = "" // a flags recording makes no copies, so it has no scratch directory to guard
	}
	if err := guardRun(getwd, scratch); err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 1
	}
	runner := execBD(opts.bd)
	if opts.flags {
		if opts.out == "" {
			opts.out = filepath.Join("internal", "bdshim", "testdata", "bd-flags.txt")
		}
		return recordFlags(opts, runner, stderr)
	}
	if opts.out == "" {
		opts.out = filepath.Join("cmd", "bdshim", "testdata", "golden")
	}
	if opts.fixture == "" {
		fmt.Fprintln(stderr, "bdshim-record: -fixture is required")
		return 2
	}
	provision, keep, err := provisionFixtureFrom(getwd, runner, opts.fixture, opts.scratch)
	if err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(opts.scratch, 0o755); err != nil {
		fmt.Fprintf(stderr, "bdshim-record: make scratch directory %q: %v\n", opts.scratch, err)
		return 1
	}
	rc := Recorder{
		Run:       runner,
		Copy:      provision,
		Normalize: func(stdout []byte) []byte { return normalizeGolden(stdout, keep) },
	}
	return execute(opts, rc, Table(), stdout, stderr)
}

// execute records the rows into opts.out, or with opts.check lists the goldens that differ and the
// ones of rows the table no longer has. It returns the exit code.
func execute(opts options, rc Recorder, rows []Row, stdout, stderr io.Writer) int {
	goldens, err := rc.Capture(rows)
	if err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 1
	}
	if opts.check {
		differ, err := Differ(opts.out, goldens)
		if err != nil {
			fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
			return 1
		}
		orphans, err := Orphans(opts.out, goldens)
		if err != nil {
			fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
			return 1
		}
		for _, name := range differ {
			fmt.Fprintln(stdout, name)
		}
		for _, name := range orphans {
			fmt.Fprintln(stdout, name)
		}
		fmt.Fprintf(stderr, "bdshim-record: %d of %d goldens differ from %s", len(differ), len(goldens), opts.out)
		if len(orphans) > 0 {
			fmt.Fprintf(stderr, ", and %d are of rows the table no longer has", len(orphans))
		}
		fmt.Fprintln(stderr)
		if len(differ) > 0 || len(orphans) > 0 {
			return 1
		}
		return 0
	}
	if err := Write(opts.out, goldens); err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 1
	}
	dropped, err := RemoveDropped(opts.out, goldens)
	if err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "bdshim-record: wrote %d goldens to %s\n", len(goldens), opts.out)
	if len(dropped) > 0 {
		fmt.Fprintf(stderr, "bdshim-record: removed the goldens of rows the table no longer has: %v\n", dropped)
	}
	return 0
}
