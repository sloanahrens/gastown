// Command affected prints the Go packages whose tests a diff can affect, one
// import path per line, so a gate can test those and leave the rest of the
// suite to a slower cadence (gt-c3lp9). It prints ALL when every package has to
// run and NONE when the diff is empty, exiting 0 either way; the caller reads
// ALL as "run everything" and NONE as "nothing changed". It runs git and go
// list in the directory it is started in, and prints ALL, never a short list,
// when that directory is not the module root or the working tree is not the
// head commit's.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/affected"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getwd, os.Stdout, os.Stderr, realRunner))
}

// run is main without the process: getwd names the directory git and go list
// run in, r answers them, out takes the selection, and errw takes the reason a
// selection had to widen. A working directory it cannot read leaves the module
// root unconfirmed, so the selection widens rather than risks narrowing.
func run(args []string, getwd func() (string, error), out, errw io.Writer, r affected.Runner) int {
	fs := flag.NewFlagSet("affected", flag.ContinueOnError)
	fs.SetOutput(errw)
	base := fs.String("base", "origin/main", "ref the diff starts at")
	head := fs.String("head", "HEAD", "ref the diff ends at")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(errw, "affected: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	wd, err := getwd()
	if err != nil {
		fmt.Fprintln(errw, "affected: cannot read the working directory; selecting every package")
		fmt.Fprintln(out, "ALL")
		return 0
	}
	sel, err := affected.Select(r, wd, *base, *head)
	if err != nil {
		fmt.Fprintf(errw, "affected: %v; selecting every package\n", err)
		fmt.Fprintln(out, "ALL")
		return 0
	}
	if sel.All {
		fmt.Fprintln(out, "ALL")
		return 0
	}
	if len(sel.Packages) == 0 {
		fmt.Fprintln(out, "NONE")
		return 0
	}
	for _, importPath := range sel.Packages {
		fmt.Fprintln(out, importPath)
	}
	return 0
}

// realRunner runs argv in the working directory and returns its standard
// output, with the tool's own diagnostics left on the terminal. The error is
// the command's failure, never its output, which this cannot judge for secrets.
func realRunner(argv []string) ([]byte, error) {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: argv is built by Select from fixed words ("git diff", "go list") and refs the caller passes on the command line; no shell sees it
	cmd.Stderr = os.Stderr
	return cmd.Output()
}
