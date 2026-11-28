// Command gt-gocacheprog is the cache program a Go build's -cacheprog runs.
// It opens the disk store under -dir, answers the go command's requests on
// stdin and stdout, and appends the serving run's counts to <dir>/stats.jsonl
// (gt-dpjby4, gt-l4bzgs).
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
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, writableCheck, time.Now))
}

// run opens the store under -dir, probes it for writability and serves the
// requests on stdin, then appends the run's counts to <dir>/stats.jsonl. It
// returns 0, or names the failing step in one stderr line and returns 1
// (gt-6i1x13, gt-dpjby4). A stats line that cannot be written is reported in
// one stderr line and leaves the exit status to the session (gt-l4bzgs).
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, check func(dir string) error, now func() time.Time) int {
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
	start := now()
	stats, serveErr := gocacheprog.Serve(context.Background(), stdin, stdout, store)
	if err := appendStats(*dir, stats, start, now, openStats); err != nil {
		fmt.Fprintf(stderr, "gt-gocacheprog: stats: %v\n", err)
	}
	if serveErr != nil {
		return fail(stderr, "serve", serveErr)
	}
	return 0
}

// statsFileName is the file each run appends its counts to (gt-l4bzgs).
const statsFileName = "stats.jsonl"

// appendStats appends one line of counts to dir/stats.jsonl, timed from start
// to now. open is a seam, as probeWritable's are, so a test drives an open,
// write or close failure in process. The line goes to a file opened for append
// in one write, so processes sharing the directory do not interleave. Its
// fields are integers and a UTC RFC 3339 time, which %q quotes as JSON does
// (gt-l4bzgs).
func appendStats(dir string, stats gocacheprog.Stats, start time.Time, now func() time.Time, open func(string) (io.WriteCloser, error)) error {
	line := fmt.Sprintf(`{"gets":%d,"hits":%d,"misses":%d,"puts":%d,"put_bytes":%d,"hit_bytes":%d,"errors":%d,"start":%q,"elapsed_ms":%d}`+"\n",
		stats.Gets, stats.Hits, stats.Misses, stats.Puts, stats.PutBytes, stats.HitBytes,
		stats.Errors, start.UTC().Format(time.RFC3339), now().Sub(start).Milliseconds())
	f, err := open(filepath.Join(dir, statsFileName))
	if err != nil {
		return err
	}
	_, werr := f.Write([]byte(line))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// openStats opens path for append, creating it: appendStats's production seam.
func openStats(path string) (io.WriteCloser, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644) //nolint:gosec // G302: counts, not secrets, and the reader may be another user
	if err != nil {
		return nil, err
	}
	return f, nil
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
