// Command stage writes the polecat image's seed sources: the rig the image is
// built at, at the build commit, and every rig deploy/polecat/seed-pins pins,
// at its pin (gt-1rz2r). `make polecat-seed-stage` runs it, and the Dockerfile
// bind-mounts the directory it writes for the seed RUN that builds the image's
// Go caches.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/polecat/seed"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	return runWith(args, os.Stdout, os.Stderr, seed.ExecGit)
}

// runWith runs the stage command with its output and git injected, so a test
// drives the same path a caller does.
func runWith(args []string, stdout, stderr io.Writer, git seed.Git) int {
	fs := flag.NewFlagSet("stage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pins := fs.String("pins", "deploy/polecat/seed-pins", "the file naming each pinned rig's commit")
	town := fs.String("town", "", "the town root holding <rig>/mayor/rig checkouts")
	dest := fs.String("dest", "", "the directory the sources are written into")
	buildRig := fs.String("build-rig", "gastown", "the rig the image is built at, seeded from the build commit")
	buildCommit := fs.String("build-commit", "", "the commit the image is built at")
	buildSrc := fs.String("build-src", "", "the checkout the build rig is read from (default <town>/<build-rig>/mayor/rig)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *town == "" || *dest == "" {
		fmt.Fprintln(stderr, "polecat-seed: -town and -dest are required")
		return 2
	}
	// Only a given flag is checked, and only a given value is passed on: an
	// absent flag stays "" so seed resolves the town's canonical checkout and
	// fails exactly as it did before this flag existed.
	if flagSet(fs, "build-src") {
		if *buildSrc == "" {
			fmt.Fprintln(stderr, "polecat-seed: -build-src is empty; name the checkout the build rig is read from")
			return 2
		}
		if fi, err := os.Stat(*buildSrc); err != nil {
			fmt.Fprintf(stderr, "polecat-seed: -build-src %q: %v\n", *buildSrc, err)
			return 2
		} else if !fi.IsDir() {
			fmt.Fprintf(stderr, "polecat-seed: -build-src %q is not a directory\n", *buildSrc)
			return 2
		}
	}

	data, err := os.ReadFile(*pins)
	if err != nil {
		fmt.Fprintf(stderr, "polecat-seed: cannot read %s: %v\n", *pins, err)
		return 1
	}
	pinned, err := seed.Pins(data)
	if err != nil {
		fmt.Fprintf(stderr, "polecat-seed: %s: %v\n", *pins, err)
		return 1
	}
	seedAt, buildNotes, err := seed.BuildRigSeed(pinned, *buildRig, *buildCommit, *town, *buildSrc, git)
	if err != nil {
		fmt.Fprintf(stderr, "polecat-seed: %v\n", err)
		return 1
	}
	sources, err := seed.Plan(pinned, *buildRig, seedAt)
	if err != nil {
		fmt.Fprintf(stderr, "polecat-seed: %v\n", err)
		return 1
	}
	notes, err := seed.Guard(pinned, *buildRig, *town, git)
	if err != nil {
		fmt.Fprintf(stderr, "polecat-seed: %v\n", err)
		return 1
	}
	printNotes(stdout, append(buildNotes, notes...))
	for _, s := range sources {
		how := "pinned"
		if s.Rig == *buildRig {
			how = "the build commit"
			if s.Commit != *buildCommit {
				how = "its pin"
			}
		}
		fmt.Fprintf(stdout, "polecat-seed: %s at %.12s (%s)\n", s.Rig, s.Commit, how)
	}
	if err := seed.Stage(*dest, *town, *buildRig, *buildSrc, sources, git); err != nil {
		fmt.Fprintf(stderr, "polecat-seed: %v\n", err)
		return 1
	}
	return 0
}

// flagSet reports whether the flag named was given on the command line. An
// explicitly empty value is then refused instead of replaced by the default.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// printNotes writes each guard note once, on its own line, with the stage's
// prefix.
func printNotes(w io.Writer, notes []string) {
	for _, n := range notes {
		fmt.Fprintf(w, "polecat-seed: %s\n", n)
	}
}
