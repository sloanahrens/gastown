// Command ctxlabels reads the polecat image's build listing from standard
// input (gt-gg9hv.62), holds it to the green commit, and appends BD_COMMIT=
// and GO_FLOOR= to $GITHUB_ENV for the image step. The workflow at
// deploy/ci/images.workflow.yml pipes the listing through it; the read and its
// refusal tests live in internal/ci/ctxlabels.
//
//	go run ./internal/polecat/imagectx/cmd/build ... | ctxlabels -sha <green sha> -env-out <$GITHUB_ENV>
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/ci/ctxlabels"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stderr))
}

func run(args []string, stdin io.Reader, stderr io.Writer) int {
	fs := flag.NewFlagSet("ctxlabels", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sha := fs.String("sha", "", "the green commit the context must be staged at")
	envOut := fs.String("env-out", "", "the file BD_COMMIT= and GO_FLOOR= are appended to, as $GITHUB_ENV")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sha == "" || *envOut == "" {
		fmt.Fprintln(stderr, "ctxlabels: -sha and -env-out are required")
		return 2
	}
	listing, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "ctxlabels: reading the build listing: %v\n", err)
		return 1
	}
	labels, err := ctxlabels.Parse(string(listing), *sha)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	f, err := os.OpenFile(*envOut, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "ctxlabels: opening %s: %v\n", *envOut, err)
		return 1
	}
	writeErr := labels.Env(f)
	closeErr := f.Close()
	if writeErr != nil {
		fmt.Fprintf(stderr, "%v\n", writeErr)
		return 1
	}
	if closeErr != nil {
		fmt.Fprintf(stderr, "ctxlabels: closing %s: %v\n", *envOut, closeErr)
		return 1
	}
	return 0
}
