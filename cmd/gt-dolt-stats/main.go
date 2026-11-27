// Command gt-dolt-stats appends one resource row for the gt-dolt container to
// the metrics files and feeds it to the pod's copy, for a launchd job that runs
// it once a minute (gt-362cts, gt-r31xgm).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/dockerstats"
	"github.com/steveyegge/gastown/internal/manifestdata"
	"github.com/steveyegge/gastown/internal/resourcesampler"
)

const gtDolt = "gt-dolt"
const podContext = "colima-gt-control"
const podNamespace = "gt"

// feedTimeout bounds the pod feed, so an unreachable pod cannot hold the one-shot command.
const feedTimeout = 20 * time.Second

// readTimeout bounds one docker stats read, so a hung daemon cannot hold the one-shot command; a variable so a test shortens it.
var readTimeout = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, time.Now, os.UserHomeDir, dockerStats, func(argv []string, stdin []byte) error {
		return feedWithin(context.Background(), func(ctx context.Context) error {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv comes from podAppendArgv
			cmd.Stdin = bytes.NewReader(stdin)
			return cmd.Run()
		})
	}))
}

// feedWithin bounds run to feedTimeout; a run after the context ended reports that error (gt-r31xgm).
func feedWithin(parent context.Context, run func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, feedTimeout)
	defer cancel()
	err := run(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// podFeed feeds row to the pod's day file at file as one JSON line (gt-r31xgm).
func podFeed(feed func(argv []string, stdin []byte) error, row resourcesampler.Row, file string) error {
	line, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("encode row: %w", err)
	}
	argv, err := podAppendArgv(podContext, podNamespace, file)
	if err != nil {
		return err
	}
	return feed(argv, append(line, '\n'))
}

// dockerStats reads one docker stats line for container, ending the docker process when ctx ends.
func dockerStats(ctx context.Context, container string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{json .}}", container).Output() //nolint:gosec // G204: container is one argv element the launchd job's flag sets, parsed by no shell
}

// run appends one row for -container under -dir, which defaults to the home
// directory's gt/.runtime/metrics, feeds the row to the pod's day file and
// returns 0, or returns 1 when a flag, the home lookup, the read, the parse, the
// append or the feed fails. A failing step prints one stderr line,
// "gt-dolt-stats: <step>: <error>"; a failing flag prints nothing; a failed feed
// keeps the local row, the fallback for when the pod is down. The read is
// bounded by readTimeout and the feed by feedTimeout.
func run(args []string, stderr io.Writer, now func() time.Time, home func() (string, error), read func(ctx context.Context, container string) ([]byte, error), feed func(argv []string, stdin []byte) error) int {
	fs := flag.NewFlagSet("gt-dolt-stats", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "directory of the metrics files, default ~/gt/.runtime/metrics")
	container := fs.String("container", gtDolt, "name of the container to read")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *dir == "" {
		homeDir, err := home()
		if err != nil {
			fmt.Fprintf(stderr, "gt-dolt-stats: home: %v\n", err)
			return 1
		}
		*dir = filepath.Join(homeDir, "gt", ".runtime", "metrics")
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
	t := now()
	row, err := dockerstats.Record(*dir, *container, t, readBounded)
	if err != nil {
		fmt.Fprintf(stderr, "gt-dolt-stats: %v\n", err)
		return 1
	}
	podFile := path.Join(manifestdata.PodTownRoot, ".runtime", "metrics", "resources", t.UTC().Format("2006-01-02")+".jsonl")
	if err := podFeed(feed, row, podFile); err != nil {
		fmt.Fprintf(stderr, "gt-dolt-stats: pod feed: %v\n", err)
		return 1
	}
	return 0
}
