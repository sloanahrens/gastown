package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/server"
	"github.com/steveyegge/gastown/tracker/store"
)

const (
	defaultListen   = "127.0.0.1:7433"
	shutdownTimeout = 10 * time.Second
	headerTimeout   = 10 * time.Second
)

// run is trackerd. ready, when non-nil, is called with the listening address
// once the server accepts connections (tests use it to find a port 0 listener).
// The DSN holds a password: it is read from the environment only and never
// written to output or to an error.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, ready func(net.Addr)) error {
	fs := flag.NewFlagSet("trackerd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", defaultListen, "loopback address to serve on")
	migrate := fs.Bool("migrate", false, "bring the schema up to date, then exit")
	issueFor := fs.String("issue-token", "", "create a token for this actor, then exit (needs -token-file)")
	scopeFlag := fs.String("scope", string(api.ScopeSeat), "scope of an issued token: seat, or admin for the daemon and operators")
	tokenFile := fs.String("token-file", "", "file to write an issued token to, mode 0600, created exclusively")
	reapEvery := fs.Duration("reap-every", time.Hour, "how often the wisp reaper runs a pass; 0 turns it off")
	maxAge := fs.Duration("reap-max-age", store.DefaultReapMaxAge, "close an open wisp created longer ago than this")
	purgeAge := fs.Duration("reap-purge-age", store.DefaultReapPurgeAge, "delete a closed wisp closed longer ago than this")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*issueFor == "") != (*tokenFile == "") {
		return errors.New("-issue-token and -token-file go together")
	}
	scope := api.Scope(*scopeFlag)
	if !scope.Valid() {
		return fmt.Errorf("-scope must be seat or admin, got %q", *scopeFlag)
	}
	if *reapEvery < 0 || *maxAge <= 0 || *purgeAge <= 0 {
		return errors.New("-reap-every must not be negative, and -reap-max-age and -reap-purge-age must be positive")
	}
	if err := server.RequireLoopback(*listen); err != nil {
		return err
	}
	dsn := getenv("TRACKER_DSN")
	if dsn == "" {
		return errors.New("TRACKER_DSN is not set (a Postgres connection string)")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return errors.New("cannot use TRACKER_DSN as a connection string") // the parse error can echo the DSN
	}
	defer pool.Close()
	pg := store.NewPostgres(pool)

	switch {
	case *migrate:
		if err := store.Migrate(ctx, pool); err != nil {
			return err
		}
		level, err := store.CurrentLevel(ctx, pool)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "schema at level %d\n", level)
		return nil
	case *issueFor != "":
		return issueToken(ctx, pg, *issueFor, scope, *tokenFile, stdout)
	}

	level, err := store.CurrentLevel(ctx, pool)
	if err != nil {
		return err
	}
	if want := store.SchemaLevel(); level != want {
		return fmt.Errorf("database is at schema level %d, this binary needs %d; run trackerd -migrate", level, want)
	}
	srv := server.New(server.Config{
		Store: pg, Auth: pg, Health: pg, SchemaLevel: store.SchemaLevel(),
		Log:  log.New(stderr, "trackerd: ", log.LstdFlags),
		Reap: store.ReapPolicy{MaxAge: *maxAge, PurgeAge: *purgeAge},
	})
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	// The reaper is stopped and joined before the pool closes, so no pass
	// runs against a closed database.
	reapCtx, stopReaper := context.WithCancel(ctx)
	reaped := srv.StartReaper(reapCtx, *reapEvery)
	defer func() {
		stopReaper()
		<-reaped
	}()
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: headerTimeout}
	done := make(chan error, 1)
	go func() { done <- httpSrv.Serve(ln) }()
	if ready != nil {
		ready(ln.Addr())
	}
	select {
	case err := <-done:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// tokenIssuer is what issueToken needs of the store.
type tokenIssuer interface {
	IssueTokenScoped(ctx context.Context, actor string, scope api.Scope, expires *time.Time) (string, error)
}

// issueToken creates a token and writes it to path (0600, exclusive, so an
// existing file is never overwritten). The token is never printed.
func issueToken(ctx context.Context, pg tokenIssuer, actor string, scope api.Scope, path string, stdout io.Writer) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: path is the operator's own flag
	if err != nil {
		return fmt.Errorf("token file: %w", err)
	}
	token, err := pg.IssueTokenScoped(ctx, actor, scope, nil)
	if err == nil {
		_, err = f.WriteString(token + "\n")
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("issue token: %w", err)
	}
	fmt.Fprintf(stdout, "%s token for %s written to %s\n", scope, actor, path)
	return nil
}
