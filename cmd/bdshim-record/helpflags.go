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

// recordFlags writes the recorded flag file at opts.out, or with -check
// reports whether the file still matches bd's help without writing. It
// returns the command's exit code.
func recordFlags(opts options, run Runner, stderr io.Writer) int {
	want, err := flagrecord.RecordFlagFile(run)
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
