// Command build stages the polecat image's build context from the pins
// (gt-gg9hv.33), with every source fetched by URL and no container engine run.
// `make polecat-image` runs it from the gt checkout, then does the engine build
// and the push from the context it prints.
//
//	build -pins <image-pins> -seed-pins <seed-pins> -seed-urls <seed-urls> -rev <sha> -out <dir>
//
// It runs in the gt checkout, whose tree must be clean and at -rev. It prints
// the pin-drift guard's notes, then "context: <dir>" and the labels the image
// build needs, one per line on stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

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

	imagePins, err := readFile(*pins, daemonimage.ParsePins)
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: %v\n", err)
		return 1
	}
	bd := bdPin(imagePins)
	seeds, err := readFile(*seedPins, seed.Pins)
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: %v\n", err)
		return 1
	}
	urls, err := readFile(*seedURLs, imagectx.ParseSeedURLs)
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: %v\n", err)
		return 1
	}

	res, err := imagectx.Build(context.Background(), imagectx.Inputs{
		Src: src, Rev: *rev, Out: *out, BD: bd, Seeds: seeds, SeedURLs: urls,
	}, imagectx.Seams{Run: imagectx.ExecRun, Git: bdbuild.ExecGit, Go: bdbuild.ExecGo})
	if err != nil {
		fmt.Fprintf(stderr, "polecat-image: %v\n", err)
		return 1
	}
	for _, line := range res.Lines() {
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// bdPin returns the bd line of the image-pins file, the source the image's bd is
// built from. ParsePins names each source once, so the line is there; a pin with
// no name is never built, because Build refuses a bd pin it does not name bd.
func bdPin(pins []daemonimage.Pin) daemonimage.Pin {
	for _, p := range pins {
		if p.Name == "bd" {
			return p
		}
	}
	return daemonimage.Pin{}
}

// readFile reads path and parses it with parse, naming the file in any error.
func readFile[T any](path string, parse func([]byte) (T, error)) (T, error) {
	var zero T
	data, err := os.ReadFile(path)
	if err != nil {
		return zero, fmt.Errorf("cannot read %s: %w", path, err)
	}
	v, err := parse(data)
	if err != nil {
		return zero, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}
