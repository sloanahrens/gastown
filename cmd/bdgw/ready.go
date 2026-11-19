package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// doltProbeTimeout bounds one readiness Dolt check. The gateway's /readyz uses
// the same bound, so the exec probe and the HTTP check answer not-ready after
// the same wait.
const doltProbeTimeout = 2 * time.Second

// readyConfig is what bdgw ready adds to the shared settings: the Dolt probe's
// dial and its bound. Both are seams, so a test drives each answer without a
// socket and without waiting the production bound.
type readyConfig struct {
	config
	dial         func(ctx context.Context, network, addr string) (net.Conn, error)
	probeTimeout time.Duration
}

func newReadyConfig() *readyConfig {
	return &readyConfig{
		config: defaultConfig(),
		// The dialer's own limit keeps a half-open connection from holding the
		// check even if the probe's context were unbounded.
		dial:         (&net.Dialer{Timeout: doltProbeTimeout}).DialContext,
		probeTimeout: doltProbeTimeout,
	}
}

// runReady is the container's health check: the same questions GET /readyz asks,
// answered by a short-lived process with no server and no token.
func runReady(args []string, stderr io.Writer) int {
	return runReadyWith(args, stderr, newReadyConfig())
}

// runReadyWith parses and answers one ready command against rc. It takes the
// config so a test can inject the Dolt dial.
func runReadyWith(args []string, stderr io.Writer, rc *readyConfig) int {
	fs := flag.NewFlagSet("ready", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bindShared(fs, &rc.config)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "bdgw ready: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if err := rc.require("tokens-dir", "verbs", "workspace"); err != nil {
		fmt.Fprintln(stderr, "bdgw ready:", err)
		return exitUsage
	}
	probe := newDoltProbe(rc.doltHost, rc.doltPort, rc.probeTimeout, rc.dial)
	if err := checkReady(rc.tokensDir, rc.verbsPath, rc.workspaceRoot, probe); err != nil {
		fmt.Fprintln(stderr, "bdgw ready: not ready:", err)
		return exitFailed
	}
	return exitOK
}

// newDoltProbe builds the Dolt check both modes run, bounded by timeout even
// when the dial ignores the context it is given: the answer on the goroutine and
// the deadline are raced, so a dial that never returns cannot hold readiness.
func newDoltProbe(host, port string, timeout time.Duration, dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(context.Context) error {
	inner := bdgateway.NewDoltProbe(host, port, dial)
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return probeWithin(ctx, inner)
	}
}

// probeWithin returns within ctx's deadline even when the probe's dial ignores
// the context. A dial that never returns parks its goroutine; readiness does not
// wait on it.
func probeWithin(ctx context.Context, probe func(context.Context) error) error {
	done := make(chan error, 1)
	go func() { done <- probe(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// checkReady loads what the gateway loads before it serves: the token
// directory, the verbs file and a workspace tree, then whether Dolt answers at
// the endpoint the calls run against. probe must bound its own run; newDoltProbe
// is the bounded one both modes pass. The workspace is read, never recovered or
// swapped, so the check is safe while the gateway is pushing. bd itself is not
// started: a health check that spawned a process could itself be the load that
// fails it, and a missing binary is a first-call failure.
func checkReady(tokensDir, verbsPath, workspaceRoot string, probe func(context.Context) error) error {
	if _, err := bdgateway.NewAuthenticator(tokensDir); err != nil {
		return fmt.Errorf("tokens: %w", err)
	}
	if _, err := bdgateway.LoadPolicyFile(verbsPath); err != nil {
		return fmt.Errorf("verbs: %w", err)
	}
	present, err := bdgateway.WorkspacePresent(workspaceRoot)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if !present {
		return errors.New("workspace: no files are loaded")
	}
	// The Dolt check runs last: every file can be loaded while Dolt is still
	// starting after a reboot, and then each call fails.
	if probe == nil {
		return errors.New("dolt: no probe is wired")
	}
	if err := probe(context.Background()); err != nil {
		return fmt.Errorf("dolt: %w", err)
	}
	return nil
}
