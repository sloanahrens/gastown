package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// serveConfig is what bdgw serve adds to the shared settings.
type serveConfig struct {
	config
	addr string
	// pushAddr is the push-only listener's address. Empty turns that listener
	// off, so a deployment without the push Service serves one port only.
	pushAddr      string
	shutdownGrace time.Duration
	// newGateway builds the gateway from the pieces serve loads. A test
	// replaces it to drive the refusal of an unwired gateway, which serve's own
	// pieces cannot produce.
	newGateway func(bdgateway.GatewayOptions) (*bdgateway.Gateway, error)
	// listen opens the sockets serve accepts on. A test replaces it with
	// in-memory listeners, so a unit test runs the real drain without a socket.
	listen func(addr string) (net.Listener, error)
	// dial opens the Dolt probe's connection. Its own timeout bounds the check,
	// so readiness does not inherit its limit from the caller's context; a test
	// replaces it to drive each answer without a socket.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func newServeConfig() *serveConfig {
	return &serveConfig{
		config:   defaultConfig(),
		addr:     defaultAddr,
		pushAddr: defaultPushAddr,
		// Longer than the largest class maximum (120 s), so a call already
		// running at shutdown finishes rather than being cut by the drain.
		shutdownGrace: 130 * time.Second,
		newGateway:    bdgateway.NewGateway,
		listen: func(addr string) (net.Listener, error) {
			return net.Listen("tcp", addr)
		},
		dial: (&net.Dialer{Timeout: doltProbeTimeout}).DialContext,
	}
}

// bindServe adds serve's own flags to fs. runServe and the tests bind the same
// set, so a test that parses a flag drives the binding the command uses.
func bindServe(fs *flag.FlagSet, sc *serveConfig) {
	bindShared(fs, &sc.config)
	fs.StringVar(&sc.addr, "addr", sc.addr, "the address to listen on")
	fs.StringVar(&sc.pushAddr, "push-addr", sc.pushAddr, "the push-only listener's address; empty turns it off")
	fs.DurationVar(&sc.shutdownGrace, "shutdown-grace", sc.shutdownGrace, "how long a graceful shutdown waits for calls in flight")
	fs.BoolVar(&sc.logPerPod, "log-per-pod", sc.logPerPod, "write the call log under <log-dir>/<hostname>, so each pod has its own")
}

func runServe(args []string, stderr io.Writer) int {
	sc := newServeConfig()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bindServe(fs, sc)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "bdgw serve: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if err := sc.require("tokens-dir", "verbs", "log-dir", "workspace", "bd", "home"); err != nil {
		fmt.Fprintln(stderr, "bdgw serve:", err)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, sc); err != nil {
		fmt.Fprintln(stderr, "bdgw serve:", err)
		return exitFailed
	}
	return exitOK
}

// serve builds every piece, serves until ctx ends, and then drains: new calls
// are refused at once and the ones already running finish within the grace.
func serve(ctx context.Context, sc *serveConfig) error {
	auth, err := bdgateway.NewAuthenticator(sc.tokensDir)
	if err != nil {
		return err
	}
	policy, err := bdgateway.LoadPolicyFile(sc.verbsPath)
	if err != nil {
		return err
	}
	workspaces, err := bdgateway.OpenWorkspaceStore(sc.workspaceRoot)
	if err != nil {
		return err
	}
	log, err := sc.callLog(policy)
	if err != nil {
		return err
	}
	defer func() {
		if err := log.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "bdgw serve: call log:", err)
		}
	}()
	runner, err := sc.executor()
	if err != nil {
		return err
	}
	gateway, err := sc.newGateway(bdgateway.GatewayOptions{
		Auth:       auth,
		Policy:     policy,
		Workspaces: workspaces,
		Log:        log,
		Runner:     runner,
		Caps:       runner,
		DoltProbe:  newDoltProbe(sc.doltHost, sc.doltPort, doltProbeTimeout, sc.dial),
	})
	if err != nil {
		return err
	}

	ln, err := sc.listen(sc.addr)
	if err != nil {
		return err
	}
	// A second, push-only listener gives the repair routes a port a Service can
	// publish while the pod is unready; an empty address keeps it off.
	var pushLn net.Listener
	if sc.pushAddr != "" {
		pushLn, err = sc.listen(sc.pushAddr)
		if err != nil {
			// No server has started on the main listener yet, so closing it keeps
			// a refused startup from leaking the socket.
			if closeErr := ln.Close(); closeErr != nil {
				return fmt.Errorf("%w; closing %s: %v", err, sc.addr, closeErr)
			}
			return err
		}
	}
	// Token rotation is a file replace followed by SIGHUP; a reload that fails
	// keeps the old table and is reported without stopping the gateway.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		if err := auth.WatchSignals(ctx, hup, func(err error) {
			fmt.Fprintln(os.Stderr, "bdgw serve: token reload:", err)
		}); err != nil {
			fmt.Fprintln(os.Stderr, "bdgw serve: token reload:", err)
		}
	}()

	mainSrv := newHTTPServer(sc.addr, gateway)
	servers := []*http.Server{mainSrv}
	listenErrs := make(chan error, 2)
	go func() { listenErrs <- mainSrv.Serve(ln) }()
	if pushLn != nil {
		pushSrv := newHTTPServer(sc.pushAddr, bdgateway.NewPushHandler(gateway))
		servers = append(servers, pushSrv)
		go func() { listenErrs <- pushSrv.Serve(pushLn) }()
	}

	var serveErr error
	var stopped int
	select {
	case err := <-listenErrs:
		stopped = 1
		// One listener stopped on its own: stop the other too, so serve never
		// leaves a server running, and report the error that ended this one.
		// ErrServerClosed is an orderly stop with nothing to report.
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
		if drainErr := drainServers(servers, sc.shutdownGrace); serveErr == nil {
			serveErr = drainErr
		}
	case <-ctx.Done():
		gateway.BeginShutdown()
		serveErr = drainServers(servers, sc.shutdownGrace)
	}
	// Every server is shut down; wait for the Serve calls still running to
	// return, so serve never leaves a listener accepting after it returns.
	for ; stopped < len(servers); stopped++ {
		<-listenErrs
	}
	// Stop the notification before closing, so a SIGHUP arriving now is never
	// sent to a closed channel; then closing the channel ends the watcher when
	// the server stopped on its own rather than on ctx.
	signal.Stop(hup)
	close(hup)
	<-watchDone
	return serveErr
}

// newHTTPServer builds one listener's server with the gateway's timeouts, shared
// so both listeners bound a slow caller the same way. A caller cannot hold a
// connection by trickling a head or a body: the head is bounded by the codec,
// the body by the stdin cap, and both by these. The response is unbounded by
// design; bd's own timeout ends it.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
}

// drainServers stops every server at once and waits for the calls already
// running to finish. The deadline is shared, so a second listener never adds a
// second grace to shutdown.
func drainServers(servers []*http.Server, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	errs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = srv.Shutdown(ctx)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
