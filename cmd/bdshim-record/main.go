// Command bdshim-record runs the real bd for every row of the golden table against a fixture
// database, and writes one golden per row: <verb>-<flags>.json holds bd's stdout and
// <verb>-<flags>.exit holds its exit code. The bd shim is tested against these, so it answers the
// way bd really does (gt-p85pw.87).
//
//	bdshim-record -fixture <JSONL file of beads> [-bd-image gastown-polecat:dev] [-bd /usr/local/bin/bd] [-dolt-host host.docker.internal] [-out cmd/bdshim/testdata/golden] [-scratch scratch] [-check]
//	bdshim-record -flags [-bd-image gastown-polecat:dev] [-bd /usr/local/bin/bd] [-dolt-host host.docker.internal] [-out internal/bdshim/testdata/bd-flags.txt] [-check]
//
// With -flags it records the value flags bd's help documents for each command, which the shim's
// tables are checked against, instead of the golden rows.
//
// Every bd call runs in a throwaway container: docker run --rm, with the directory it works in
// mounted at /work and nothing else, so bd finds no beads workspace above it however the
// environment is set (gt-p85pw.117). -bd-image names the image and -bd the bd binary's path inside
// it; their defaults are gastown-polecat:dev, the image deploy/polecat/Dockerfile builds, and
// /usr/local/bin/bd, where that image puts bd. The container reaches the throwaway Dolt server on
// -dolt-host, host.docker.internal by default, since 127.0.0.1 inside it is the container itself.
// The container's environment is the BEADS_DOLT_* names of the recording environment, less the
// BEADS_DOLT_SERVER_DATABASE selector a copy's own metadata must decide, plus the recorder's pinned
// actor and git identity, so a golden's author reads the same on every machine; every other
// variable stays on this side of the boundary. Each value reaches docker through the client's
// environment rather than the command line, so an endpoint value or a secret never appears in the
// process list.
//
// Every row runs in a copy of the fixture: a directory under -scratch holding a database of its
// own, made with bd init and filled from -fixture, which is copied into each copy since nothing
// outside the mounted directory is readable. The read rows share one copy and each mutating row
// gets one of its own, so no row sees another's writes; each copy's database is dropped and its
// directory removed before the command exits. Before any row, bd is asked for each bead the fixture
// must hold and each verb the table names, so a fixture or a table bd cannot answer fails the run.
// With -check the table runs again and the goldens that differ, and those of rows the table no
// longer has, are listed on stdout; nothing is written, and the exit code is 1 when any are listed.
//
// Before anything is written, the guard proves the container is the one the goldens describe: its
// bd must be the build the host's bd on PATH is (release, build id, commit and schema level, though
// a checkout's build stamps its branch before the commit and a cross-compiled one need not), and bd
// must resolve no workspace outside /work. Only then does the scratch's bd init run, and the
// workspace it made must be on disk in the scratch. That workspace exists for the check alone: its
// database is dropped and its directory removed as soon as the guard passes. A scratch behind a
// symlink is refused, and so is a recorder whose working directory is a workspace root itself; one
// that merely sits under a workspace passes, since work lives under the town (gt-p85pw.116).
//
// The environment must name a throwaway Dolt server, and the recorder refuses one that names the
// town's: the town's port, a host that is not this machine, or no port at all, which leaves bd with
// its own default, the town's server. The scratch's database is made on the server that names, and
// one made on the town's server is cruft no cleanup owns (gt-p85pw.115).
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
	bd       string
	bdImage  string
	doltHost string
	fixture  string
	out      string
	scratch  string
	check    bool
	flags    bool
}

func run(args []string, stdout, stderr io.Writer) int {
	return runIn(os.Getwd, execChild, args, stdout, stderr)
}

// runIn is run with the working directory and the child factory injected, so a test puts the
// recorder in a tree of its own and answers bd itself, neither changing the process's directory nor
// starting a process.
func runIn(getwd func() (string, error), child func(options) childBD, args []string, stdout, stderr io.Writer) int {
	var opts options
	flags := flag.NewFlagSet("bdshim-record", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.bd, "bd", "/usr/local/bin/bd", "the bd binary's path inside the container image")
	flags.StringVar(&opts.bdImage, "bd-image", "gastown-polecat:dev", "the image every bd child runs in, which carries the linux bd at -bd")
	flags.StringVar(&opts.doltHost, "dolt-host", "host.docker.internal", "the host name the container reaches the throwaway Dolt server on")
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
	} else {
		// An empty -scratch names no directory, and joining it to the working directory would name
		// that directory itself, so it is refused here as the directory the run cannot make.
		if opts.scratch == "" {
			fmt.Fprintf(stderr, "bdshim-record: make scratch directory %q: name a directory, not an empty path\n", opts.scratch)
			return 1
		}
		// docker mounts the scratch by its host path, which must be absolute, so every copy and the
		// guard name it from the working directory, never a relative path docker would read as one
		// (gt-p85pw.117).
		abs, err := absolutePath(getwd, opts.scratch)
		if err != nil {
			fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
			return 1
		}
		opts.scratch = abs
		scratch = abs
	}
	bd := child(opts)
	if err := guardRun(getwd, scratch, bd); err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 1
	}
	if opts.flags {
		if opts.out == "" {
			opts.out = filepath.Join("internal", "bdshim", "testdata", "bd-flags.txt")
		}
		return recordFlags(opts, bd.Run, stderr)
	}
	if opts.out == "" {
		opts.out = filepath.Join("cmd", "bdshim", "testdata", "golden")
	}
	if opts.fixture == "" {
		fmt.Fprintln(stderr, "bdshim-record: -fixture is required")
		return 2
	}
	// A fixture the run refuses is an input the operator named, so it exits 2 as a missing -fixture does.
	provision, keep, err := provisionFixtureFrom(getwd, bd.Run, opts.fixture, opts.scratch)
	if err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 2
	}
	if err := os.MkdirAll(opts.scratch, 0o755); err != nil {
		fmt.Fprintf(stderr, "bdshim-record: make scratch directory %q: %v\n", opts.scratch, err)
		return 1
	}
	rc := Recorder{
		Run:       bd.Run,
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
