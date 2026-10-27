package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/util"
)

// VersionInfo is what `bd version --json` reports. BuildID, ContractVersion
// and DBSchemaVersion are empty on upstream bd and on fork builds from before
// the machine surface (beads be-3xa.3).
type VersionInfo struct {
	Version         string `json:"version"`
	Build           string `json:"build"`
	BuildID         string `json:"build_id"`
	Commit          string `json:"commit"`
	ContractVersion int    `json:"contract_version"`
	DBSchemaVersion int    `json:"db_schema_version"`
}

// versionTextRE matches a bd from before machine mode, whose version report is
// prose: "bd version 0.52.0" or "bd version 0.52.0 (dev: ...)".
var versionTextRE = regexp.MustCompile(`bd version (\d+\.\d+\.\d+)`)

// Version reports what the bd serving this store says about itself, and reads
// no database: the startup presence probe asks it before any store is open.
func (b *Beads) Version() (VersionInfo, error) {
	out, err := b.run("version", "--json")
	if err != nil {
		return VersionInfo{}, err
	}
	return parseVersionReport(out)
}

// SchemaLevel reports the highest applied migration of the database this store
// serves; a store that did not answer is an error wrapping ErrStoreUnavailable.
func (b *Beads) SchemaLevel() (int, error) { return b.schemaLevel(beadsql.SchemaLevel()) }

// schemaLevel runs one beadsql read of the applied level. A statement beadsql's
// guard refuses is an error here, never a bd call with no statement.
func (b *Beads) schemaLevel(query beadsql.Query) (int, error) {
	args, err := query.BdArgs("--json")
	if err != nil {
		return 0, err
	}
	out, err := b.runMachine(args...)
	if err != nil {
		return 0, schemaLevelError(err)
	}
	level, err := parseSchemaLevel(LegacyPayload(args, out))
	if err != nil {
		return 0, fmt.Errorf("could not read the database migration level: %w", err)
	}
	return level, nil
}

// schemaLevelError classifies a failed schema-level read: bd's schema_skew
// exit names a database ahead of bd, and store_unavailable — or a read its
// deadline ended — means the store failed to answer, not bd.
func schemaLevelError(err error) error {
	switch exitCodeOf(err) {
	case bdSchemaSkewExit:
		return fmt.Errorf("the database schema is ahead of this bd (bd exit %d): %w", bdSchemaSkewExit, err)
	case bdStoreUnavailableExit:
		return fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: the read did not finish: %w", ErrStoreUnavailable, err)
	}
	return fmt.Errorf("could not read the database migration level: %w", err)
}

// parseVersionReport reads what `bd version --json` printed: the JSON object,
// bare or inside a machine-mode envelope, or the text a bd from before machine
// mode printed. Output that names no version is an error.
func parseVersionReport(out []byte) (VersionInfo, error) {
	info, jsonErr := parseVersionJSON(out)
	if jsonErr == nil {
		return info, nil
	}
	if m := versionTextRE.FindSubmatch(out); len(m) == 2 {
		return VersionInfo{Version: string(m[1])}, nil
	}
	return VersionInfo{}, jsonErr
}

// parseVersionJSON reads a bare `bd version --json` report or one inside a
// machine-mode envelope. Output that is not a JSON object with a version is an
// error.
func parseVersionJSON(out []byte) (VersionInfo, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(out), &raw); err != nil {
		return VersionInfo{}, fmt.Errorf("bd version --json output is not JSON: %q", util.FirstLine(string(out)))
	}
	payload := bytes.TrimSpace(out)
	if data, ok := raw["data"]; ok && len(data) > 0 && data[0] == '{' {
		payload = data
	}
	var info VersionInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return VersionInfo{}, fmt.Errorf("bd version --json output is not a version object: %w", err)
	}
	if info.Version == "" {
		return VersionInfo{}, fmt.Errorf("bd version --json output has no version: %q", util.FirstLine(string(out)))
	}
	return info, nil
}

// parseSchemaLevel reads bd sql's answer to beadsql.SchemaLevel: one JSON row
// carrying the applied migration level.
func parseSchemaLevel(out []byte) (int, error) {
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		return 0, fmt.Errorf("bd sql output is not a JSON row list: %q", util.FirstLine(string(out)))
	}
	if len(rows) != 1 {
		return 0, fmt.Errorf("bd sql returned %d rows for MAX(version), want 1", len(rows))
	}
	switch v := rows[0]["version"].(type) {
	case float64:
		if v > 0 {
			return int(v), nil
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("schema_migrations has no applied version (got %v)", rows[0]["version"])
}
