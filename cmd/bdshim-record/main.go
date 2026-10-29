// Command bdshim-record runs the real bd for every row of the golden table against a fixture
// database, and writes one golden per row: <verb>-<flags>.json holds bd's stdout and
// <verb>-<flags>.exit holds its exit code. The bd shim is tested against these, so it answers the
// way bd really does (gt-p85pw.87).
//
//	bdshim-record -fixture <dir holding .beads> [-bd bd] [-out cmd/bdshim/testdata/golden] [-scratch scratch] [-check]
//
// Every row runs in a copy of the fixture made under -scratch, which must be outside the fixture:
// the read rows share one copy and each mutating row gets one of its own, and all are removed before
// the command exits. Before any row, bd is asked for each bead the fixture must hold and each verb
// the table names, so a fixture or a table bd cannot answer fails the run. With -check the table
// runs again and the goldens that differ, and those of rows the table no longer has, are listed on
// stdout; nothing is written, and the exit code is 1 when any are listed.
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
}

func run(args []string, stdout, stderr io.Writer) int {
	var opts options
	flags := flag.NewFlagSet("bdshim-record", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.bd, "bd", "bd", "the bd binary to run")
	flags.StringVar(&opts.fixture, "fixture", "", "the fixture directory bd runs in; it holds a .beads directory")
	flags.StringVar(&opts.out, "out", filepath.Join("cmd", "bdshim", "testdata", "golden"), "the directory the goldens are written to")
	flags.StringVar(&opts.scratch, "scratch", "scratch", "the directory fresh fixture copies are made in; it must be outside -fixture")
	flags.BoolVar(&opts.check, "check", false, "re-run the table and list the goldens that differ, writing nothing")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if opts.fixture == "" {
		fmt.Fprintln(stderr, "bdshim-record: -fixture is required")
		return 2
	}
	if err := refuseScratchInsideFixture(opts.fixture, opts.scratch); err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 2
	}
	if err := os.MkdirAll(opts.scratch, 0o755); err != nil {
		fmt.Fprintf(stderr, "bdshim-record: make scratch directory %q: %v\n", opts.scratch, err)
		return 1
	}
	rc := Recorder{
		Run:  execBD(opts.bd),
		Copy: copyFixture(opts.fixture, opts.scratch),
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
