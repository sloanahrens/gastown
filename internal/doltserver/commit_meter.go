package doltserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DBCommits is one database's reading on the commits-per-day meter
// (gt-8z769.4): the Dolt commits made in the last 24 hours. Err is set, and
// Commits is meaningless, when that database's count failed.
type DBCommits struct {
	Database string `json:"database"`
	Commits  int    `json:"commits"`
	Err      string `json:"error,omitempty"`
}

// commitsLastDayQuery counts one database's commits in the last 24 hours.
// dolt_log.date is UTC; UTC_TIMESTAMP() keeps the window 24h whatever the
// session time zone. Measured 2026-09-30 on the town server: under 0.7s per
// database with ~24k commits of history, cheap enough to run on demand.
const commitsLastDayQuery = "SELECT COUNT(*) FROM `%s`.dolt_log WHERE date > UTC_TIMESTAMP() - INTERVAL 1 DAY"

// CommitsLastDay counts the commits each town database made in the last 24
// hours, in database name order. A database whose count fails gets Err set;
// the error return is for a server or database list that cannot be read.
func (h *host) CommitsLastDay(ctx context.Context, townRoot string) ([]DBCommits, error) {
	config := h.DefaultConfig(townRoot)

	databases, err := h.ListDatabases(townRoot)
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	if len(databases) == 0 {
		return nil, nil
	}

	dsn := fmt.Sprintf("%s@tcp(%s)/", config.userDSN(), config.HostPort())
	db, err := h.openMySQL(dsn)
	if err != nil {
		return nil, fmt.Errorf("opening mysql connection: %w", err)
	}
	defer db.Close()
	db.SetConnMaxLifetime(5 * time.Second)
	db.SetMaxOpenConns(1)

	sort.Strings(databases)
	counts := make([]DBCommits, 0, len(databases))
	for _, name := range databases {
		reading := DBCommits{Database: name}
		if strings.Contains(name, "`") {
			reading.Err = "database name is not a plain identifier"
		} else if err := db.QueryRowContext(ctx, fmt.Sprintf(commitsLastDayQuery, name)).Scan(&reading.Commits); err != nil {
			reading.Err = err.Error()
		}
		counts = append(counts, reading)
	}
	return counts, nil
}

// CommitsLastDay is (*host).CommitsLastDay on the real machine.
func CommitsLastDay(ctx context.Context, townRoot string) ([]DBCommits, error) {
	return std.CommitsLastDay(ctx, townRoot)
}

// OverCommitBudget returns the readings whose 24h commit count is above
// limit, busiest first. Failed readings are never over budget.
func OverCommitBudget(counts []DBCommits, limit int) []DBCommits {
	var over []DBCommits
	for _, c := range counts {
		if c.Err == "" && c.Commits > limit {
			over = append(over, c)
		}
	}
	sort.SliceStable(over, func(i, j int) bool { return over[i].Commits > over[j].Commits })
	return over
}

// FormatCommitCounts renders readings as "gt=3048 hq=2041", failed ones as
// "gt=?".
func FormatCommitCounts(counts []DBCommits) string {
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		if c.Err != "" {
			parts = append(parts, c.Database+"=?")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", c.Database, c.Commits))
	}
	return strings.Join(parts, " ")
}
