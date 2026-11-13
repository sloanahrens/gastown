// Command build stages the polecat image's build context from the pins
// (gt-gg9hv.33), with every source fetched by URL and no container engine run.
// `make polecat-image` runs it from the gt checkout, then does the engine build
// and the push from the context it prints.
//
//	build -pins <image-pins> -seed-pins <seed-pins> -seed-urls <seed-urls> -rev <sha> -out <dir> [-forge-owner <owner>]
//
// It runs in the gt checkout, whose tree must be clean and at -rev. It prints
// the pin-drift guard's notes, then "context: <dir>" and the labels the image
// build needs, one per line on stdout. It reads no town: -forge-owner names the
// owner, else the setting's default.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
	"github.com/steveyegge/gastown/internal/polecat/imagectx"
	"github.com/steveyegge/gastown/internal/polecat/seed"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pins := fs.String("pins", daemonimage.PinsPath, "the image-pins file: bd's URL and commit")
	seedPins := fs.String("seed-pins", "deploy/polecat/seed-pins", "the file naming each seeded rig's commit")
	seedURLs := fs.String("seed-urls", "deploy/polecat/seed-urls", "the file naming each seeded rig other than gastown by URL")
	rev := fs.String("rev", "", "the full sha of the gt commit the image is built at; the checkout must be clean and at it")
	out := fs.String("out", "", "the absolute, empty directory the context is staged in")
	forgeOwner := fs.String("forge-owner", "", "the Forgejo owner the {owner} placeholder in the pins and seed files expands to; unset takes the setting's default (config.DefaultForgeOwner), and the image workflows pass this flag on the day of the owner rename (gt-ipp4uy)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *rev == "" || *out == "" {
		fmt.Fprintln(stderr, "polecat-image: -rev and -out are required")
		return 2
	}
	src, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: cannot read the working directory: %v\n", err)
		return 1
	}
	// The owner is settled before any file is read, so a bad -forge-owner stops
	// the build with nothing read or built.
	var given bool
	fs.Visit(func(f *flag.Flag) {
		given = given || f.Name == "forge-owner"
	})
	owner, err := buildForgeOwner(*forgeOwner, given)
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: %v\n", err)
		return 1
	}

	inputs, err := readBuildInputs(*pins, *seedPins, *seedURLs, owner)
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: %v\n", err)
		return 1
	}

	res, err := imagectx.Build(context.Background(), imagectx.Inputs{
		Src: src, Rev: *rev, Out: *out, BD: inputs.bd, Seeds: inputs.seeds, SeedURLs: inputs.urls,
	}, imagectx.Seams{Run: imagectx.ExecRun, Git: bdbuild.ExecGit, GitEnv: bdbuild.ExecGitEnv, Go: bdbuild.ExecGo})
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: %v\n", err)
		return 1
	}
	for _, line := range res.Lines() {
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// buildForgeOwner is the Forgejo owner the build expands its {owner} placeholders
// to: the -forge-owner value when the flag was given, which must be a usable
// owner name, else the setting's default. The build reads no town, since CI has
// none, so there is no town row.
func buildForgeOwner(explicit string, given bool) (string, error) {
	if !given {
		return config.DefaultForgeOwner, nil
	}
	if err := config.ValidForgeOwner(explicit); err != nil {
		return "", fmt.Errorf("forge owner from -forge-owner: %w", err)
	}
	return explicit, nil
}

// buildInputs are the three files the build reads, each with its {owner}
// placeholder expanded and parsed.
type buildInputs struct {
	bd    daemonimage.Pin
	seeds []seed.Source
	urls  map[string]string
}

// readBuildInputs reads the image-pins, seed-pins and seed-urls files, expanding
// each {owner} placeholder with owner before it is parsed. The bd pin is the
// line of the image-pins file the image's bd is built from; ParsePins names each
// source once, so the line is there.
func readBuildInputs(pinsPath, seedPinsPath, seedURLsPath, owner string) (buildInputs, error) {
	var in buildInputs
	imagePins, err := readFile(pinsPath, owner, daemonimage.ParsePins)
	if err != nil {
		return in, err
	}
	for _, p := range imagePins {
		if p.Name == "bd" {
			in.bd = p
		}
	}
	if in.seeds, err = readFile(seedPinsPath, owner, seed.Pins); err != nil {
		return in, err
	}
	if in.urls, err = readFile(seedURLsPath, owner, imagectx.ParseSeedURLs); err != nil {
		return in, err
	}
	return in, nil
}

// readFile reads path, expands its {owner} placeholder with owner, and parses
// it with parse, naming the file in any error.
func readFile[T any](path, owner string, parse func([]byte) (T, error)) (T, error) {
	var zero T
	data, err := os.ReadFile(path)
	if err != nil {
		return zero, fmt.Errorf("cannot read %s: %w", path, err)
	}
	text, err := config.ExpandForgeOwner(string(data), owner)
	if err != nil {
		return zero, fmt.Errorf("%s: %w", path, err)
	}
	v, err := parse([]byte(text))
	if err != nil {
		return zero, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}
