package deps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/util"
)

// KnownBDContractVersions are the bd JSON contract versions (bd version
// --json "contract_version", beads cmd/bd/envelope.go JSONContractVersion)
// this gt was written against. A bd reporting any other contract may have
// changed a payload gt parses, so the town does not run on it.
var KnownBDContractVersions = []int{1}

// bdSchemaSkewExit is the exit status bd's machine mode reserves for a
// database whose schema is ahead of the binary (kind schema_skew).
const bdSchemaSkewExit = 26

// ErrBDHandshake marks a refusal to run the town against the bd on PATH.
var ErrBDHandshake = errors.New("bd handshake failed")

// BDInstallHint is the only supported way to install bd. gastown never
// installs bd itself: the fork's safe-install checks it is building main and
// never moves the schema backwards.
const BDInstallHint = "install the beads fork's bd with `make safe-install` from the beads repository"

// BDVersionInfo is what `bd version --json` reports. BuildID,
// ContractVersion and DBSchemaVersion are empty on upstream bd and on fork
// builds from before the machine surface (beads be-3xa.3).
type BDVersionInfo struct {
	Version         string `json:"version"`
	Build           string `json:"build"`
	BuildID         string `json:"build_id"`
	Commit          string `json:"commit"`
	ContractVersion int    `json:"contract_version"`
	DBSchemaVersion int    `json:"db_schema_version"`
}

// BDRunner runs one bd invocation with extraEnv added to a clean environment
// and returns its stdout and stderr. A failure that should read as a bd exit
// status implements interface{ ExitCode() int }.
type BDRunner func(ctx context.Context, extraEnv []string, args ...string) (stdout, stderr []byte, err error)

// NewBDProcessRunner returns a BDRunner that runs the bd on PATH in dir, with
// BEADS target variables stripped from the inherited environment so bd
// resolves the database from dir alone.
func NewBDProcessRunner(dir string) BDRunner {
	return func(ctx context.Context, extraEnv []string, args ...string) ([]byte, []byte, error) {
		env := append(beads.StripBDTargetEnv(os.Environ()), extraEnv...)
		cmd := beads.CommandContextWithEnv(ctx, dir, env, args...)
		util.SetDetachedProcessGroup(cmd)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.Bytes(), stderr.Bytes(), err
	}
}

// BDHandshake is a bd that passed: its version report and the migration
// level of the database it serves, which are equal.
type BDHandshake struct {
	Path     string
	Found    BDVersionInfo
	DBSchema int
}

// String summarizes the handshake for status output.
func (h *BDHandshake) String() string {
	return fmt.Sprintf("bd %s (build %s, schema<=%d, contract %d); database at %d",
		h.Found.Version, h.Found.BuildID, h.Found.DBSchemaVersion, h.Found.ContractVersion, h.DBSchema)
}

// ParseBDVersionJSON reads `bd version --json`, bare or inside a machine-mode
// envelope. Output that is not a JSON object with a version is an error.
func ParseBDVersionJSON(out []byte) (BDVersionInfo, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(out), &raw); err != nil {
		return BDVersionInfo{}, fmt.Errorf("bd version --json output is not JSON: %q", util.FirstLine(string(out)))
	}
	payload := bytes.TrimSpace(out)
	if data, ok := raw["data"]; ok && len(data) > 0 && data[0] == '{' {
		payload = data
	}
	var info BDVersionInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return BDVersionInfo{}, fmt.Errorf("bd version --json output is not a version object: %w", err)
	}
	if info.Version == "" {
		return BDVersionInfo{}, fmt.Errorf("bd version --json output has no version: %q", util.FirstLine(string(out)))
	}
	return info, nil
}

// CheckBDHandshake decides whether the town may run against the bd run
// reaches. It requires a fork build (build_id present) whose JSON contract
// version this gt knows and whose schema level equals the migration level
// of the database it serves. The database is read through bd only after the
// version passes. Every refusal wraps ErrBDHandshake and names what was
// found, what is required and how to install bd; bdPath is for the message.
func CheckBDHandshake(ctx context.Context, bdPath string, run BDRunner) (*BDHandshake, error) {
	hs := &BDHandshake{Path: bdPath}
	stdout, stderr, err := run(ctx, nil, "version", "--json")
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
			return nil, refuseBD(hs, false, "bd not found on PATH")
		}
		return nil, refuseBD(hs, false, fmt.Sprintf("bd version --json failed: %v%s", err, stderrSuffix(stderr)))
	}
	info, err := ParseBDVersionJSON(stdout)
	if err != nil {
		return nil, refuseBD(hs, false, err.Error())
	}
	hs.Found = info

	var problems []string
	if info.BuildID == "" {
		problems = append(problems, "no build_id (not a beads fork build with the machine surface)")
	}
	if info.ContractVersion == 0 {
		problems = append(problems, "no contract_version")
	} else if !knownContract(info.ContractVersion) {
		problems = append(problems, fmt.Sprintf("contract %d is not one this gt knows (known: %s)", info.ContractVersion, knownContractList()))
	}
	if info.DBSchemaVersion == 0 {
		problems = append(problems, "no db_schema_version")
	}
	if len(problems) > 0 {
		return nil, refuseBD(hs, false, strings.Join(problems, "; "))
	}

	level, err := readDBSchemaLevel(ctx, run)
	if err != nil {
		return nil, refuseBD(hs, false, err.Error())
	}
	hs.DBSchema = level
	if level != info.DBSchemaVersion {
		return nil, refuseBD(hs, true, fmt.Sprintf("bd migrates to schema<=%d but the database is at %d", info.DBSchemaVersion, level))
	}
	return hs, nil
}

// ReadDBSchemaLevel is readDBSchemaLevel for callers that check a database
// other than the one the handshake reads, such as the daemon's per-store
// compatibility check.
func ReadDBSchemaLevel(ctx context.Context, run BDRunner) (int, error) {
	return readDBSchemaLevel(ctx, run)
}

// readDBSchemaLevel asks bd for the highest applied migration. It runs in
// machine mode so a database ahead of bd fails with bd's typed schema_skew
// exit rather than prose.
func readDBSchemaLevel(ctx context.Context, run BDRunner) (int, error) {
	stdout, stderr, err := run(ctx, []string{"BD_MACHINE=1"}, "sql", "--json", "SELECT MAX(version) AS version FROM schema_migrations")
	if err != nil {
		var coded interface{ ExitCode() int }
		if errors.As(err, &coded) && coded.ExitCode() == bdSchemaSkewExit {
			return 0, fmt.Errorf("the database schema is ahead of this bd (bd exit %d%s)", bdSchemaSkewExit, stderrSuffix(stderr))
		}
		return 0, fmt.Errorf("could not read the database migration level: %v%s", err, stderrSuffix(stderr))
	}
	level, err := parseDBSchemaLevel(stdout)
	if err != nil {
		return 0, fmt.Errorf("could not read the database migration level: %w", err)
	}
	return level, nil
}

func parseDBSchemaLevel(out []byte) (int, error) {
	trimmed := bytes.TrimSpace(out)
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if err := json.Unmarshal(trimmed, &envelope); err == nil && len(envelope.Data) > 0 {
			trimmed = envelope.Data
		}
	}
	var rows []map[string]any
	if err := json.Unmarshal(trimmed, &rows); err != nil {
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

func refuseBD(hs *BDHandshake, dbRead bool, reason string) error {
	found := "bd at " + hs.Path
	if hs.Path == "" {
		found = "bd (not on PATH)"
	}
	if hs.Found.Version != "" {
		f := hs.Found
		found += fmt.Sprintf(": version %s, build %s, schema<=%s, contract %s",
			f.Version, orUnknown(firstNonEmpty(f.BuildID, f.Build)), intOrUnknown(f.DBSchemaVersion), intOrUnknown(f.ContractVersion))
	}
	if dbRead {
		found += fmt.Sprintf("; database at %d", hs.DBSchema)
	}
	return fmt.Errorf("%w: %s\n  found:    %s\n  required: a beads fork build with contract %s whose schema level equals the database migration level\n  fix:      %s",
		ErrBDHandshake, reason, found, knownContractList(), BDInstallHint)
}

func knownContract(v int) bool {
	for _, k := range KnownBDContractVersions {
		if k == v {
			return true
		}
	}
	return false
}

func knownContractList() string {
	vs := append([]int(nil), KnownBDContractVersions...)
	sort.Ints(vs)
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, " or ")
}

func stderrSuffix(stderr []byte) string {
	if s := strings.TrimSpace(string(stderr)); s != "" {
		return ": " + util.FirstLine(s)
	}
	return ""
}

func intOrUnknown(n int) string {
	if n == 0 {
		return "?"
	}
	return strconv.Itoa(n)
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
