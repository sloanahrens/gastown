package reaper

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql" // registers the "mysql" driver sql.Open uses below

	"github.com/steveyegge/gastown/internal/testdb"
)

// DiscoverDatabases queries SHOW DATABASES on the Dolt server and returns
// all production databases, filtering out system databases and test pollution.
// Falls back to DefaultDatabases on any error.
func DiscoverDatabases(host string, port int) []string {
	dsn := fmt.Sprintf("root@tcp(%s:%d)/?parseTime=true&timeout=5s", host, port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return DefaultDatabases
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return DefaultDatabases
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if name == "information_schema" || name == "mysql" {
			continue
		}
		if testdb.IsTestDatabaseName(name) {
			continue
		}
		databases = append(databases, name)
	}

	if len(databases) == 0 {
		return DefaultDatabases
	}
	return databases
}
