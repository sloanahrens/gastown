// Command build compiles the polecat image's bd from the commit the installed
// bd reports, or from -commit's override, and prints that commit's full sha on
// stdout for the image's label (gt-7b7a5). `make polecat-image` runs it.
//
//	build -src <beads checkout> -out <bd binary> [-commit <sha>] [-build-dir <dir>]
//
// The commit must be in the checkout's object store; nothing is fetched. The
// compile happens in a clean worktree of the checkout, which is removed before
// the command returns.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	src := fs.String("src", "", "the beads checkout whose object store holds the commit")
	out := fs.String("out", "", "the bd binary to write")
	commit := fs.String("commit", "", "the commit to build; default the commit the installed bd reports")
	buildDir := fs.String("build-dir", "", "the directory the clean worktree is made in; default a temporary one")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *src == "" || *out == "" {
		fmt.Fprintln(stderr, "polecat-image: -src and -out are required")
		return 2
	}

	// The installed bd, read where this command runs and with the BEADS target
	// variables stripped: the report names bd, not a database.
	rev, err := bdbuild.Commit(*commit, beads.NewPlain("", beads.StripBDTargetEnv(os.Environ())))
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: %v\n", err)
		return 1
	}
	full, err := bdbuild.Compile(bdbuild.Options{Src: *src, Out: *out, Commit: rev, BuildDir: *buildDir}, bdbuild.ExecGit, bdbuild.ExecGo)
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "polecat-image: bd built from %s\n", full)
	fmt.Fprintln(stdout, full)
	return 0
}
