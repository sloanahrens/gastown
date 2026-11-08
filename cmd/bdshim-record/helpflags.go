package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/bdshim/flagrecord"
)

// flagFileMode is the permissions the recorded flags file is written with, the
// same as a golden's: the file is committed, so git decides its tracked mode.
const flagFileMode = 0o600

// recordFlagsInScratch records the flag file with the scratch workspace held for the whole walk.
// bd mail --help needs a store to print, so a help call with no directory exits 1 and stops the
// walk (gt-ufil6n). The guard proves the scratch and makes its workspace first, and the workspace is
// released on every path out of the walk, a failed walk included. It returns the command's exit
// code.
func recordFlagsInScratch(getwd func() (string, error), opts options, child childBD, stderr io.Writer) (code int) {
	cwd, err := guardCheck(getwd, child)
	if err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 1
	}
	release, err := holdScratch(cwd, opts.scratch, child)
	if err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 1
	}
	defer func() {
		if err := release(); err != nil {
			fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
			code = 1
		}
	}()
	return recordFlags(opts, child.Run, stderr)
}

// recordFlags writes the recorded flag file at opts.out, or with -check
// reports whether the file still matches bd's help without writing. Every
// help call runs in opts.scratch, the workspace recordFlagsInScratch holds,
// and a walk with no scratch refuses to start rather than run bd with no
// directory. It returns the command's exit code.
func recordFlags(opts options, run Runner, stderr io.Writer) int {
	if opts.scratch == "" {
		fmt.Fprintln(stderr, "bdshim-record: the flags walk needs a scratch directory to run bd's help in")
		return 1
	}
	inScratch := func(_ string, args []string) ([]byte, int, error) {
		return run(opts.scratch, args)
	}
	want, err := flagrecord.RecordFlagFile(inScratch)
	if err != nil {
		fmt.Fprintf(stderr, "bdshim-record: %v\n", err)
		return 1
	}
	if opts.check {
		have, err := os.ReadFile(opts.out)
		if err != nil {
			fmt.Fprintf(stderr, "bdshim-record: reading %s: %v\n", opts.out, err)
			return 1
		}
		if !bytes.Equal(have, want) {
			fmt.Fprintf(stderr, "bdshim-record: %s differs from bd's help; run without -check to record it\n", opts.out)
			return 1
		}
		fmt.Fprintf(stderr, "bdshim-record: %s matches bd's help\n", opts.out)
		return 0
	}
	if err := os.WriteFile(opts.out, want, flagFileMode); err != nil {
		fmt.Fprintf(stderr, "bdshim-record: writing %s: %v\n", opts.out, err)
		return 1
	}
	fmt.Fprintf(stderr, "bdshim-record: wrote %s\n", opts.out)
	return 0
}
