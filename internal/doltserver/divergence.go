package doltserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// DivergenceFetchTimeout bounds each DOLT_FETCH inside FetchAndVerify. Fetching
// a database over the git protocol is the slow part of the pre-flight; it
// shares the 2m budget the compactor_dog daemon used for the same fetch before
// gt-nfu7 removed the daemon's compaction path. A database with several
// remotes spends this budget once per remote: FetchAndVerify gives each remote
// its own timeout derived from the caller's context, so a slow first remote
// does not starve the second (gt-aku6).
const DivergenceFetchTimeout = 2 * time.Minute

// divergenceFetchTimeout is the per-fetch budget actually used: the constant
// above, overridable in tests via GASTOWN_DIVERGENCE_FETCH_TIMEOUT so the
// per-remote budget can be exercised without waiting two minutes per remote.
func divergenceFetchTimeout() time.Duration {
	if v := os.Getenv("GASTOWN_DIVERGENCE_FETCH_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DivergenceFetchTimeout
}

// RemoteDivergence is what FetchAndVerify learned about a database's remotes.
//
// It is the shared pre-flight for every path that flattens commit history: a
// flatten rewrites the graph, and a later force-push then destroys whatever
// only the remote had. Both `gt maintain` and (historically) the compactor_dog
// daemon need the same answer, so the query lives here rather than beside
// either caller.
type RemoteDivergence struct {
	// Remotes lists every configured Dolt remote the check ran on, in the
	// order checked. It is empty only when the database has no remote at
	// all — that means there was nothing to verify, not a verdict that the
	// database is safe.
	Remotes []string

	// RemoteHead is the commit the responsible remote's branch pointed at
	// when it was fetched, or "" when that remote has no matching branch yet
	// (a first push).
	RemoteHead string

	// Diverged is true when RemoteHead is absent from the local dolt_log: the
	// remote holds commits this database does not have, so flattening and
	// force-pushing would drop them.
	Diverged bool
}

// FetchAndVerify fetches every remote configured on the database and reports
// whether any of them holds commits the local history lacks on the branch
// that database is actually on.
//
// A database with no remote returns the zero RemoteDivergence and no error.
// Any failure to complete the check — an unreachable remote, a query error —
// is returned as an error, never as a Diverged=false verdict: callers using
// this as a guard must fail closed, because "could not check" and "checked,
// safe" are different facts and only one of them licenses a destructive write.
// The same fail-closed rule applies across remotes: checking only the first
// configured remote would clear a database whose second remote has actually
// diverged, so every remote is checked and the first divergence (or the first
// remote that cannot be verified) stops the check and is reported.
//
// db must already be connected to dbName (the queries read that database's
// dolt_remotes / dolt_remote_branches / dolt_log / active_branch()).
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

	// Discover every remote first. dolt_remotes is a local table, so a
	// database with no remote costs one cheap query and no network round trip.
	remotes, err := listRemotes(ctx, db)
	if err != nil {
		return RemoteDivergence{}, fmt.Errorf("list remotes: %w", err)
	}
	if len(remotes) == 0 {
		return RemoteDivergence{}, nil // No remote — nothing to verify
	}

	// The branch a force-push would actually target is whatever branch this
	// database is on, not a hardcoded "main": a database checked out on a
	// different branch would otherwise never have that branch's divergence
	// examined.
	branch, err := activeBranch(ctx, db)
	if err != nil {
		return RemoteDivergence{}, fmt.Errorf("active branch: %w", err)
	}

	var checked []string
	for _, remoteName := range remotes {
		div, err := fetchAndVerifyRemote(ctx, db, remoteName, branch)
		div.Remotes = checked
		if err != nil {
			return div, err
		}
		checked = append(checked, remoteName)
		if div.Diverged {
			return div, nil
		}
	}

	return RemoteDivergence{Remotes: checked}, nil
}

// listRemotes returns every configured remote's name, in a stable (name) order.
func listRemotes(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM dolt_remotes ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var remotes []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		remotes = append(remotes, name)
	}
	return remotes, rows.Err()
}

// activeBranch returns the database's current branch (e.g. "main").
func activeBranch(ctx context.Context, db *sql.DB) (string, error) {
	var branch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&branch); err != nil {
		return "", err
	}
	return branch, nil
}

// fetchAndVerifyRemote fetches a single remote and reports whether its branch
// (the database's active branch) holds commits the local history lacks.
func fetchAndVerifyRemote(ctx context.Context, db *sql.DB, remoteName, branch string) (RemoteDivergence, error) {
	// Fetch. Without this the remote-tracking ref below is whatever the last
	// fetch left behind, and divergence that happened since would be invisible.
	fetchCtx, cancel := context.WithTimeout(ctx, divergenceFetchTimeout())
	defer cancel()
	if _, err := db.ExecContext(fetchCtx, "CALL DOLT_FETCH(?)", remoteName); err != nil {
		return RemoteDivergence{}, fmt.Errorf("DOLT_FETCH %s: %w", remoteName, err)
	}

	// Read the remote's branch. Dolt stores remote branches locally as
	// refs/remotes/<remote>/<branch> and surfaces them under that full name in
	// dolt_remote_branches — a query for "<remote>/<branch>" finds nothing, and
	// the column is `hash`, not `commit_hash`.
	ref := "remotes/" + remoteName + "/" + branch
	var remoteHead string
	err := db.QueryRowContext(ctx, "SELECT hash FROM dolt_remote_branches WHERE name = ?", ref).Scan(&remoteHead)
	if errors.Is(err, sql.ErrNoRows) {
		// The remote has no matching branch yet — a first push, nothing to lose.
		return RemoteDivergence{}, nil
	}
	if err != nil {
		return RemoteDivergence{}, fmt.Errorf("read %s: %w", ref, err)
	}

	// The remote head is reachable from local iff it appears in local history.
	var present int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM dolt_log WHERE commit_hash = ?", remoteHead,
	).Scan(&present); err != nil {
		return RemoteDivergence{RemoteHead: remoteHead}, fmt.Errorf("ancestor check: %w", err)
	}

	return RemoteDivergence{
		RemoteHead: remoteHead,
		Diverged:   present == 0,
	}, nil
}
