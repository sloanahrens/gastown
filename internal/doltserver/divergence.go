package doltserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DivergenceFetchTimeout bounds the DOLT_FETCH inside FetchAndVerify. Fetching
// a database over the git protocol is the slow part of the pre-flight; it
// shares the 2m budget the compactor_dog daemon used for the same fetch before
// gt-nfu7 removed the daemon's compaction path.
const DivergenceFetchTimeout = 2 * time.Minute

// RemoteDivergence is what FetchAndVerify learned about a database's remote.
//
// It is the shared pre-flight for every path that flattens commit history: a
// flatten rewrites the graph, and a later force-push then destroys whatever
// only the remote had. Both `gt maintain` and (historically) the compactor_dog
// daemon need the same answer, so the query lives here rather than beside
// either caller.
type RemoteDivergence struct {
	// Remote is the name of the first configured Dolt remote, or "" when the
	// database has no remote at all. An empty Remote means there was nothing
	// to verify — it is not a verdict that the database is safe.
	Remote string

	// RemoteHead is the commit the remote's main branch pointed at when it was
	// fetched, or "" when the remote has no main branch yet (a first push).
	RemoteHead string

	// Diverged is true when RemoteHead is absent from the local dolt_log: the
	// remote holds commits this database does not have, so flattening and
	// force-pushing would drop them.
	Diverged bool
}

// FetchAndVerify fetches the database's first remote and reports whether the
// remote's main branch has commits the local history lacks.
//
// A database with no remote returns the zero RemoteDivergence and no error.
// Any failure to complete the check — an unreachable remote, a query error —
// is returned as an error, never as a Diverged=false verdict: callers using
// this as a guard must fail closed, because "could not check" and "checked,
// safe" are different facts and only one of them licenses a destructive write.
//
// db must already be connected to dbName (the queries read that database's
// dolt_remotes / dolt_remote_branches / dolt_log).
//
// Caveat for callers: a *local* flatten with no push leaves the remote
// pointing at pre-flatten history, so this reports Diverged on the next run
// until the remote is force-pushed. That is the intended reading — the remote
// really does hold commits the local graph no longer contains, and a
// force-push would drop them.
func FetchAndVerify(ctx context.Context, db *sql.DB, dbName string) (RemoteDivergence, error) {
	if !validSQLName(dbName) {
		return RemoteDivergence{}, fmt.Errorf("invalid database name %q: must match [a-zA-Z0-9_.-]+", dbName)
	}

	// Discover the remote first. dolt_remotes is a local table, so a database
	// with no remote costs one cheap query and no network round trip.
	var remoteName string
	err := db.QueryRowContext(ctx, "SELECT name FROM dolt_remotes ORDER BY name LIMIT 1").Scan(&remoteName)
	if errors.Is(err, sql.ErrNoRows) {
		return RemoteDivergence{}, nil // No remote — nothing to verify
	}
	if err != nil {
		return RemoteDivergence{}, fmt.Errorf("list remotes: %w", err)
	}

	// Fetch. Without this the remote-tracking ref below is whatever the last
	// fetch left behind, and divergence that happened since would be invisible.
	fetchCtx, cancel := context.WithTimeout(ctx, DivergenceFetchTimeout)
	defer cancel()
	if _, err := db.ExecContext(fetchCtx, "CALL DOLT_FETCH(?)", remoteName); err != nil {
		return RemoteDivergence{Remote: remoteName}, fmt.Errorf("DOLT_FETCH %s: %w", remoteName, err)
	}

	// Read the remote's main branch. Dolt stores remote branches locally as
	// refs/remotes/<remote>/<branch> and surfaces them under that full name in
	// dolt_remote_branches — a query for "<remote>/main" finds nothing, and
	// the column is `hash`, not `commit_hash`.
	ref := "remotes/" + remoteName + "/main"
	var remoteHead string
	err = db.QueryRowContext(ctx, "SELECT hash FROM dolt_remote_branches WHERE name = ?", ref).Scan(&remoteHead)
	if errors.Is(err, sql.ErrNoRows) {
		// The remote has no main branch yet — a first push, nothing to lose.
		return RemoteDivergence{Remote: remoteName}, nil
	}
	if err != nil {
		return RemoteDivergence{Remote: remoteName}, fmt.Errorf("read %s: %w", ref, err)
	}

	// The remote head is reachable from local iff it appears in local history.
	var present int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM dolt_log WHERE commit_hash = ?", remoteHead,
	).Scan(&present); err != nil {
		return RemoteDivergence{Remote: remoteName, RemoteHead: remoteHead}, fmt.Errorf("ancestor check: %w", err)
	}

	return RemoteDivergence{
		Remote:     remoteName,
		RemoteHead: remoteHead,
		Diverged:   present == 0,
	}, nil
}
