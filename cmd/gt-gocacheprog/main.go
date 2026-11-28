// Command gt-gocacheprog is the cache program a Go build's -cacheprog runs.
// It serves the journaled disk store under -dir, bounds the store at -max-bytes
// when the go command closes it, and appends the serving run's counts to
// <dir>/stats.jsonl (gt-dpjby4, gt-l4bzgs, gt-n9cohm).
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

// run opens the journaled store under -dir, probes it for writability and
// serves the requests on stdin. A close request evicts the store down to
// -max-bytes before its answer is written, and the run's counts are appended to
// <dir>/stats.jsonl. It returns 0, or names the failing step in one stderr line
// and returns 1 (gt-6i1x13, gt-dpjby4, gt-n9cohm). A stats line that cannot be
// written is reported in one stderr line and leaves the exit status to the
// session (gt-l4bzgs). An eviction that fails adds 1 to the line's errors and
// one stderr line, and a journal close this run makes that fails adds 1; both
// leave the responses and exit status alone (gt-n9cohm).
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, check func(dir string) error, now func() time.Time) int {
	fs := flag.NewFlagSet("gt-gocacheprog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "directory of the cache store (required, absolute)")
	maxBytes := fs.Int64("max-bytes", 0, "store body bytes the eviction leaves at most (required, above 0)")
	if err := fs.Parse(args); err != nil {
		return fail(stderr, "flags", err)
	}
	if *dir == "" {
		return fail(stderr, "flags", errors.New("-dir is required"))
	}
	if !filepath.IsAbs(*dir) {
		return fail(stderr, "flags", fmt.Errorf("-dir %q is not absolute", *dir))
	}
	if *maxBytes <= 0 {
		return fail(stderr, "flags", fmt.Errorf("-max-bytes %d is not above 0", *maxBytes))
	}
	disk, err := gocacheprog.NewDisk(*dir, now)
	if err != nil {
		return fail(stderr, "store", err)
	}
	if err := check(*dir); err != nil {
		return fail(stderr, "writable", err)
	}
	journal, err := gocacheprog.OpenJournal(*dir, now)
	if err != nil {
		return fail(stderr, "journal", err)
	}
	store := &session{Journaled: gocacheprog.NewJournaled(disk, journal), dir: *dir, maxBytes: *maxBytes, now: now, journal: journal}
	start := now()
	stats, serveErr := gocacheprog.Serve(context.Background(), stdin, stdout, store)
	// Serve closes the journal on a close request, which counts a failed close
	// once; this closes the journal of a session that ended without one.
	if err := store.journal.Close(); err != nil {
		fmt.Fprintf(stderr, "gt-gocacheprog: journal: %v\n", err)
		stats.Errors++
	}
	if store.evictErr != nil {
		fmt.Fprintf(stderr, "gt-gocacheprog: evict: %v\n", store.evictErr)
		stats.Errors++
	}
	if err := appendStats(*dir, stats, start, now, openStats); err != nil {
		fmt.Fprintf(stderr, "gt-gocacheprog: stats: %v\n", err)
	}
	if serveErr != nil {
		return fail(stderr, "serve", serveErr)
	}
	return 0
}

// session is the journaled store run serves. Serve calls its Close on a close
// request before the close answer is written (gt-n9cohm).
type session struct {
	*gocacheprog.Journaled
	dir      string
	maxBytes int64
	now      func() time.Time
	journal  *gocacheprog.Journal
	evictErr error
}

// Close closes the journal, then evicts the store down to maxBytes. A failed
// journal close is returned, so Serve counts it. An eviction error is kept for
// run to report, since the close answer does not carry it (gt-n9cohm).
func (s *session) Close() error {
	jerr := s.journal.Close()
	_, _, s.evictErr = gocacheprog.Evict(s.dir, s.maxBytes, s.journal.Path(), s.now())
	return jerr
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
