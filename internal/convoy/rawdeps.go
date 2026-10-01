package convoy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// DepListRawIDs queries the raw dependencies table to get dependency target
// IDs. Unlike bd dep list, this does NOT join with the issues table, so it
// works for cross-database dependencies where the target issues live in a
// different Dolt database. See GH #2624.
//
// dir is the town beads directory for HQ queries. direction is "down"
// (issue_id → depends_on_id) or "up" (depends_on_id → issue_id). depType
// filters by dependency type (e.g., "tracks", "blocks"); empty means all.
//
// It returns deduplicated, unwrapped issue IDs (external:prefix:id → id).
func (t Town) DepListRawIDs(dir, issueID, direction, depType string) ([]string, error) {
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

	if ids, err := depListRawIDsViaDolt(dir, issueID, direction, depType); err == nil {
		return ids, nil
	}

	args, err := beadsql.RawDeps(issueID, direction, depType).BdArgs("--json")
	if err != nil {
		return nil, err
	}
	out, err := t.bdJSONAutoCommit(dir, args...)
	if err != nil {
		return nil, fmt.Errorf("bd sql for deps of %s: %w", issueID, err)
	}
	ids, err := parseRawDepRows(out, parseKey)
	if err != nil {
		return nil, fmt.Errorf("parsing dep sql for %s: %w", issueID, err)
	}
	return ids, nil
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
	dsn := fmt.Sprintf("root@tcp(%s)/%s?parseTime=true", net.JoinHostPort(host, strconv.Itoa(cfg.Port)), url.PathEscape(cfg.Database))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := beadsql.Open(ctx, dsn, cfg.Database)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	return queryRawDepIDs(ctx, db, beadsql.RawDeps(issueID, direction, depType))
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

func parseRawDepRows(out []byte, parseKey string) ([]string, error) {
	var rows []map[string]string
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(rows))
	var ids []string
	for _, row := range rows {
		id := beads.ExtractIssueID(row[parseKey])
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// DepListRawIDs is Town.DepListRawIDs for a bare directory and the process
// environment.
func DepListRawIDs(dir, issueID, direction, depType string) ([]string, error) {
	return Town{Root: dir}.DepListRawIDs(dir, issueID, direction, depType)
}
