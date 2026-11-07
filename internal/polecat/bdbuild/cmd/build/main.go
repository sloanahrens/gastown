// Command build compiles a bd from the commit the installed bd reports, or from
// -commit's override, and prints that commit's full sha on stdout (gt-7b7a5).
// The polecat image's context command builds bd by calling bdbuild.Compile
// itself, from the source its pin names (internal/polecat/imagectx); this
// command remains for building a bd from a checkout of your own.
//
//	build -src <beads checkout> -out <bd binary> [-commit <sha>] [-build-dir <dir>]
//
// The compile happens in a clean worktree of the checkout, which is removed
// before the command returns. bdbuild.Compile's doc states what happens to a
// commit the checkout does not hold (gt-pme82).
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
	// The installed bd, read where this command runs and with the BEADS target
	// variables stripped: the report names bd, not a database.
	installed := beads.NewPlain("", beads.StripBDTargetEnv(os.Environ()))
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, installed, compileBD))
}

// compileBD is the production compile: the real git and go.
func compileBD(opt bdbuild.Options) (string, error) {
	return bdbuild.Compile(opt, bdbuild.ExecGit, bdbuild.ExecGo)
}

// run builds the bd and prints its path. installed is the bd the host runs and
// compile does the build; main passes the real ones, a test passes fakes.
func run(args []string, stdout, stderr io.Writer, installed bdbuild.Versioner, compile func(bdbuild.Options) (string, error)) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	src := fs.String("src", "", "the beads checkout the commit is built from")
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

	// One read of the installed bd answers both Commit and the build id Compile
	// stamps, so the image's bd prints the host's id for the same commit
	// (gt-7qinix).
	info, versionErr := installed.Version()
	rev, err := bdbuild.Commit(*commit, oneVersion{info: info, err: versionErr})
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: %v\n", err)
		return 1
	}
	full, err := compile(bdbuild.Options{
		Src: *src, Out: *out, Commit: rev, BuildDir: *buildDir,
		InstalledBuildID: info.BuildID, InstalledCommit: info.Commit,
	})
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "polecat-image: bd built from %s\n", full)
	fmt.Fprintln(stdout, full)
	return 0
}

// oneVersion replays one installed-bd version report: Commit asks what the bd
// reports, and the build stamp needs the same answer, so bd is run once.
type oneVersion struct {
	info beads.VersionInfo
	err  error
}

func (v oneVersion) Version() (beads.VersionInfo, error) { return v.info, v.err }
