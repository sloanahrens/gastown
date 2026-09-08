package doctor

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os/exec"
	"strings"
)

// runBdSQLCSV runs `bd sql --csv <query>` in dir and returns the parsed CSV
// records from stdout (header row included).
//
// Stderr must be kept separate from stdout here: bd emits non-fatal
// diagnostics on stderr (shared-server notices, routing messages, config
// warnings, version-skew hints), and merging them into the CSV stream via
// CombinedOutput corrupts parsing — a one-line notice becomes CSV line 1 and
// the real header fails with "record on line 2: wrong number of fields" (gt-m7t).
func runBdSQLCSV(dir, query string) ([][]string, error) {
	cmd := exec.Command("bd", "sql", "--csv", query) //nolint:gosec // G204: callers pass constant queries
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("bd sql: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("bd sql: %w", err)
	}

	records, err := csv.NewReader(&stdout).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("csv parse: %w", err)
	}
	return records, nil
}
