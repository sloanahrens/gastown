package main

import (
	"context"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/rehearsal"
)

// interruptContext gives the run a context that the first interrupt or SIGTERM
// cancels. The handler is released as soon as it is done, restoring the default
// handling so a second signal kills the process.
func interruptContext(parent context.Context,
	notify func(context.Context, ...os.Signal) (context.Context, context.CancelFunc)) (context.Context, context.CancelFunc) {
	ctx, stop := notify(parent, os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

// runCommand turns the run command line into the run's Options and returns
// execute's code. A refused command line, an unreadable working directory or a
// refused input names the reason on stderr and returns 2 without executing.
func runCommand(ctx context.Context, args []string, getwd func() (string, error),
	execute func(context.Context, rehearsal.Options, io.Writer, io.Writer) int, stdout, stderr io.Writer) int {
	a, err := rehearsal.ParseRunArgs(args)
	if err != nil {
		return usage(stderr, "run", strings.TrimPrefix(err.Error(), "gt-rehearsal run: "))
	}
	root, err := getwd()
	if err != nil {
		return usage(stderr, "run", "working directory: "+err.Error())
	}
	o, err := rehearsal.NewRunOptions(a, root, time.Now)
	if err != nil {
		return usage(stderr, "run", err.Error())
	}
	return execute(ctx, o, stdout, stderr)
}
