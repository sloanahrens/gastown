package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/backend"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/workspace"
)

// depListRawIDs queries the raw dependencies table to get dependency target
// IDs. Unlike bd dep list, this does NOT join with the issues table, so it
// works for cross-database dependencies where the target issues live in a
// different Dolt database. See GH #2624.
//
// It moved here from internal/convoy when that package was deleted
// (gt-gzhin.7): the query is generic dependency SQL, not convoy state, and
// scheduler_epic is its only caller.
//
// dir is the town beads directory for HQ queries. direction is "down"
// (issue_id → depends_on_id) or "up" (depends_on_id → issue_id). depType
// filters by dependency type (e.g., "tracks", "blocks"); empty means all.
//
// It returns deduplicated, unwrapped issue IDs (external:prefix:id → id).
func depListRawIDs(dir, issueID, direction, depType string) ([]string, error) {
	return depListRawIDsWith(dir, issueID, direction, depType, backend.FromEnv(os.Getenv), workspace.Find, depListRawIDsViaDolt)
}

// rawDepDolt is the Dolt read the raw path tries first.
type rawDepDolt func(dir, issueID, direction, depType string) ([]string, error)

// townRootFinder is workspace.Find: the town root above dir, or "" outside a town.
type townRootFinder func(startDir string) (string, error)

// depListRawIDsWith is depListRawIDs with the tracker settings, the town
// lookup and the Dolt read passed in, so a test can supply them without reading
// the environment or opening Dolt.
func depListRawIDsWith(dir, issueID, direction, depType string, cfg backend.Config, find townRootFinder, dolt rawDepDolt) ([]string, error) {
	// Bead IDs are system-generated alphanumeric strings with hyphens, dots,
	// and underscores — validate to prevent injection before interpolating below.
	if !beads.IsValidBeadID(issueID) {
		return nil, fmt.Errorf("invalid bead ID: %q", issueID)
	}

	var parseKey string
	if direction == "up" {
		parseKey = "issue_id"
	} else {
		parseKey = "depends_on_id"
	}
	if depType != "" && !beads.IsValidBeadID(depType) {
		return nil, fmt.Errorf("invalid dep type: %q", depType)
	}

	ids, err := depListRawIDsOnTracker(dir, issueID, direction, depType, cfg, find)
	if err == nil {
		return ids, nil
	}
	if !errors.Is(err, errNotOnTracker) {
		return nil, err
	}

	if ids, err := dolt(dir, issueID, direction, depType); err == nil {
		return ids, nil
	}

	rows, err := beads.NewPinned(beads.ResolveBeadsDir(dir)).SQLCSV(beadsql.RawDeps(issueID, direction, depType))
	if err != nil {
		return nil, fmt.Errorf("bd sql for deps of %s: %w", issueID, err)
	}
	ids, err = parseRawDepRows(rows, parseKey)
	if err != nil {
		return nil, fmt.Errorf("parsing dep sql for %s: %w", issueID, err)
	}
	return ids, nil
}

// errNotOnTracker means dir's rig is not known to be on the tracker, so the
// Dolt and bd sql path answers the query instead, as it always has.
var errNotOnTracker = errors.New("rig is not on the tracker")

// depListRawIDsOnTracker answers the dependency query from the tracker seam
// when dir's route names the tracker, so no Dolt connection or bd sql call is
// made. A dir outside any town, and a rig with no route or a Dolt route, return
// errNotOnTracker and the caller runs the raw path unchanged. A town or route
// table that cannot be read is an error, never errNotOnTracker: without it the
// seam cannot tell a tracker rig from a Dolt rig, and a Dolt read for a tracker
// rig is the second store the backend exists to prevent. A tracker that cannot
// be opened is an error too.
func depListRawIDsOnTracker(dir, issueID, direction, depType string, cfg backend.Config, find townRootFinder) ([]string, error) {
	townRoot, err := find(dir)
	if err != nil {
		return nil, fmt.Errorf("finding the town for %s: %w", issueID, err)
	}
	if townRoot == "" {
		return nil, errNotOnTracker
	}
	beadsDir := beads.ResolveBeadsDir(dir)
	onTracker, err := routedToTracker(townRoot, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("reading the route table for %s: %w", issueID, err)
	}
	if !onTracker {
		return nil, errNotOnTracker
	}

	store, err := backend.Open(townRoot, beadsDir, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening beads for %s: %w", issueID, err)
	}
	var deps []beads.IssueDep
	if direction == "up" {
		deps, err = store.DepListUp(issueID, depType)
	} else {
		deps, err = store.DepList(issueID, depType)
	}
	if err != nil {
		return nil, fmt.Errorf("tracker deps of %s: %w", issueID, err)
	}
	raw := make([]string, 0, len(deps))
	for _, dep := range deps {
		raw = append(raw, dep.ID)
	}
	return unwrapIssueIDs(raw), nil
}

// routedToTracker reports whether the route table under townRoot names the
// tracker for beadsDir. It matches a route the way backend.Open does, so the
// two agree on which rig is which. A rig with no route, or a route with no
// backend, is on Dolt.
func routedToTracker(townRoot, beadsDir string) (bool, error) {
	routes, err := beads.LoadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return false, err
	}
	want := filepath.Clean(beadsDir)
	for _, r := range routes {
		if filepath.Clean(filepath.Join(townRoot, r.Path, ".beads")) == want {
			return r.BackendName() == beads.BackendTracker, nil
		}
	}
	return false, nil
}

func depListRawIDsViaDolt(dir, issueID, direction, depType string) ([]string, error) {
	beadsDir := beads.ResolveBeadsDir(dir)
	cfg, ok := doltserver.ReadBeadsRuntimeConfig(beadsDir)
	if !ok || cfg.Database == "" || cfg.Port == 0 {
		return nil, fmt.Errorf("missing server metadata for %s", beadsDir)
	}
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	dsn := depListRawDSN(depListRawUserInfo(dir), host, cfg.Port, cfg.Database)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := beadsql.Open(ctx, dsn, cfg.Database)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	return queryRawDepIDs(ctx, db, beadsql.RawDeps(issueID, direction, depType))
}

// depListRawUserInfo is the user[:password] for the town that owns dir, from
// its Dolt config. A dir outside any town connects as the default user.
func depListRawUserInfo(dir string) string {
	townRoot, err := workspace.Find(dir)
	if err != nil || townRoot == "" {
		return doltserver.DefaultUser
	}
	return doltserver.DefaultConfig(townRoot).UserDSN()
}

// depListRawDSN is the DSN depListRawIDsViaDolt connects with.
func depListRawDSN(userInfo, host string, port int, database string) string {
	return fmt.Sprintf("%s@tcp(%s)/%s?parseTime=true", userInfo, net.JoinHostPort(host, strconv.Itoa(port)), url.PathEscape(database))
}

func queryRawDepIDs(ctx context.Context, db *beadsql.DB, query beadsql.Query) ([]string, error) {
	rows, err := db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := make(map[string]bool)
	var ids []string
	for rows.Next() {
		var rawID sql.NullString
		if err := rows.Scan(&rawID); err != nil {
			return nil, err
		}
		if !rawID.Valid {
			continue
		}
		id := beads.ExtractIssueID(rawID.String)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// unwrapIssueIDs deduplicates raw dependency IDs and unwraps each one to its
// issue ID (external:prefix:id → id), keeping the first-seen order. Every raw
// dependency read in this file answers through it, so the rule lives in one
// place.
func unwrapIssueIDs(raw []string) []string {
	seen := make(map[string]bool, len(raw))
	var ids []string
	for _, r := range raw {
		id := beads.ExtractIssueID(r)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// parseRawDepRows reads the parseKey column of a bd sql --csv answer: a
// header row, then one row per dependency.
func parseRawDepRows(rows [][]string, parseKey string) ([]string, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	col := -1
	for i, name := range rows[0] {
		if name == parseKey {
			col = i
		}
	}
	if col < 0 {
		return nil, fmt.Errorf("no %s column in %q", parseKey, rows[0])
	}
	var raw []string
	for _, row := range rows[1:] {
		if col >= len(row) {
			continue
		}
		raw = append(raw, row[col])
	}
	return unwrapIssueIDs(raw), nil
}
