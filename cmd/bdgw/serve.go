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
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// serveConfig is what bdgw serve adds to the shared settings.
type serveConfig struct {
	config
	addr          string
	shutdownGrace time.Duration
	// newGateway builds the gateway from the pieces serve loads. A test
	// replaces it to drive the refusal of an unwired gateway, which serve's own
	// pieces cannot produce.
	newGateway func(bdgateway.GatewayOptions) (*bdgateway.Gateway, error)
	// listen opens the socket serve accepts on. A test replaces it with an
	// in-memory listener, so a unit test runs the real drain without a socket.
	listen func(addr string) (net.Listener, error)
}

func newServeConfig() *serveConfig {
	return &serveConfig{
		config: defaultConfig(),
		addr:   defaultAddr,
		// Longer than the largest class maximum (120 s), so a call already
		// running at shutdown finishes rather than being cut by the drain.
		shutdownGrace: 130 * time.Second,
		newGateway:    bdgateway.NewGateway,
		listen: func(addr string) (net.Listener, error) {
			return net.Listen("tcp", addr)
		},
	}
}

func runServe(args []string, stderr io.Writer) int {
	sc := newServeConfig()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bindShared(fs, &sc.config)
	fs.StringVar(&sc.addr, "addr", sc.addr, "the address to listen on")
	fs.DurationVar(&sc.shutdownGrace, "shutdown-grace", sc.shutdownGrace, "how long a graceful shutdown waits for calls in flight")
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
	})
	if err != nil {
		return err
	}

	ln, err := sc.listen(sc.addr)
	if err != nil {
		return err
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

	srv := &http.Server{
		Addr:    sc.addr,
		Handler: gateway,
		// A caller cannot hold a connection by trickling a head or a body: the
		// head is bounded by the codec, the body by the stdin cap, and both by
		// this. The response is unbounded by design; bd's own timeout ends it.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	listenErr := make(chan error, 1)
	go func() { listenErr <- srv.Serve(ln) }()

	var serveErr error
	select {
	case err := <-listenErr:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
		gateway.BeginShutdown()
		drainCtx, cancel := context.WithTimeout(context.Background(), sc.shutdownGrace)
		defer cancel()
		serveErr = srv.Shutdown(drainCtx)
	}
	// Stop the notification before closing, so a SIGHUP arriving now is never
	// sent to a closed channel; then closing the channel ends the watcher when
	// the server stopped on its own rather than on ctx.
	signal.Stop(hup)
	close(hup)
	<-watchDone
	return serveErr
}
