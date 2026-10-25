// Command stage writes the polecat image's seed sources: the rig the image is
// built at, at the build commit, and every rig deploy/polecat/seed-pins pins,
// at its pin (gt-1rz2r). `make polecat-seed-stage` runs it, and the Dockerfile
// bind-mounts the directory it writes for the seed RUN that builds the image's
// Go caches.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/steveyegge/gastown/internal/polecat/seed"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("stage", flag.ContinueOnError)
	pins := fs.String("pins", "deploy/polecat/seed-pins", "the file naming each pinned rig's commit")
	town := fs.String("town", "", "the town root holding <rig>/mayor/rig checkouts")
	dest := fs.String("dest", "", "the directory the sources are written into")
	buildRig := fs.String("build-rig", "gastown", "the rig the image is built at, seeded from the build commit")
	buildCommit := fs.String("build-commit", "", "the commit the image is built at")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *town == "" || *dest == "" {
		fmt.Fprintln(os.Stderr, "polecat-seed: -town and -dest are required")
		return 2
	}

	data, err := os.ReadFile(*pins)
	if err != nil {
		fmt.Fprintf(os.Stderr, "polecat-seed: cannot read %s: %v\n", *pins, err)
		return 1
	}
	pinned, err := seed.Pins(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "polecat-seed: %s: %v\n", *pins, err)
		return 1
	}
	sources, err := seed.Plan(pinned, *buildRig, *buildCommit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "polecat-seed: %v\n", err)
		return 1
	}
	if err := seed.Guard(pinned, *buildRig, *town, seed.ExecGit); err != nil {
		fmt.Fprintf(os.Stderr, "polecat-seed: %v\n", err)
		return 1
	}
	for _, s := range sources {
		how := "pinned"
		if s.Rig == *buildRig {
			how = "the build commit"
		}
		fmt.Printf("polecat-seed: %s at %.12s (%s)\n", s.Rig, s.Commit, how)
	}
	if err := seed.Stage(*dest, *town, sources, seed.ExecGit); err != nil {
		fmt.Fprintf(os.Stderr, "polecat-seed: %v\n", err)
		return 1
	}
	return 0
}
