// Command gt-gocacheprog is the cache program a Go build's -cacheprog runs.
// It opens the disk store under -dir and answers the go command's requests on
// stdin and stdout (gt-dpjby4).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/gocacheprog"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, writableCheck))
}

// run opens the store under -dir, probes it for writability and serves the
// requests on stdin. It returns 0, or names the failing step in one stderr
// line and returns 1 (gt-6i1x13, gt-dpjby4).
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, check func(dir string) error) int {
	fs := flag.NewFlagSet("gt-gocacheprog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "directory of the cache store (required, absolute)")
	if err := fs.Parse(args); err != nil {
		return fail(stderr, "flags", err)
	}
	if *dir == "" {
		return fail(stderr, "flags", errors.New("-dir is required"))
	}
	if !filepath.IsAbs(*dir) {
		return fail(stderr, "flags", fmt.Errorf("-dir %q is not absolute", *dir))
	}
	store, err := gocacheprog.NewDisk(*dir, time.Now)
	if err != nil {
		return fail(stderr, "store", err)
	}
	if err := check(*dir); err != nil {
		return fail(stderr, "writable", err)
	}
	if _, err := gocacheprog.Serve(context.Background(), stdin, stdout, store); err != nil {
		return fail(stderr, "serve", err)
	}
	return 0
}

// writableCheck is the seam main and the tests pass run (gt-6i1x13).
func writableCheck(dir string) error { return probeWritable(dir, os.CreateTemp, os.Remove) }

// probeWritable proves dir accepts writes. NewDisk only calls MkdirAll, which
// succeeds on an existing directory whose permissions forbid writing, so a
// create, close or remove failure must refuse the store (gt-6i1x13).
func probeWritable(dir string, create func(string, string) (*os.File, error), remove func(string) error) error { //nolint:unparam // create and remove are the seams tests drive
	f, err := create(dir, ".gt-gocacheprog-probe-*")
	if err != nil {
		return fmt.Errorf("gt-gocacheprog: temp file in %s: %w", dir, err)
	}
	name := f.Name()
	if err := errors.Join(f.Close(), remove(name)); err != nil {
		return fmt.Errorf("gt-gocacheprog: temp file in %s: %w", dir, err)
	}
	return nil
}

// fail writes the step and its cause as one stderr line and returns 1.
func fail(stderr io.Writer, step string, err error) int {
	fmt.Fprintf(stderr, "gt-gocacheprog: %s: %v\n", step, err)
	return 1
}
