// Command gt-dolt-stats appends one resource row for the gt-dolt container to
// the metrics files, for a launchd job that runs it once a minute (gt-362cts).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/dockerstats"
)

const gtDolt = "gt-dolt"

// readTimeout bounds one docker stats read, so a hung daemon cannot hold the one-shot command; a variable so a test shortens it.
var readTimeout = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, time.Now, dockerStats))
}

// dockerStats reads one docker stats line for container, ending the docker process when ctx ends.
func dockerStats(ctx context.Context, container string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{json .}}", container).Output() //nolint:gosec // G204: container is the sampler's fixed name
}

// run appends one row for gt-dolt under -dir and returns 0, or returns 1 when
// a flag, the read, the parse or the append fails. A failing step prints one
// stderr line, "gt-dolt-stats: <step>: <error>", and appends no row; a failing
// flag prints nothing. The read is bounded by readTimeout.
func run(args []string, stderr io.Writer, now func() time.Time, read func(ctx context.Context, container string) ([]byte, error)) int {
	fs := flag.NewFlagSet("gt-dolt-stats", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "directory of the metrics files")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	timeout := readTimeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	readBounded := func(container string) ([]byte, error) {
		out, err := read(ctx, container)
		if ctx.Err() != nil {
			return nil, fmt.Errorf("docker stats: timed out after %s", timeout)
		}
		return out, err
	}
	if _, err := dockerstats.Record(*dir, gtDolt, now(), readBounded); err != nil {
		fmt.Fprintf(stderr, "gt-dolt-stats: %v\n", err)
		return 1
	}
	return 0
}
