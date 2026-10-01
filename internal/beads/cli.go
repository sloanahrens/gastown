package beads

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
)

// Typed wrappers over the bd maintenance subcommands (config, sql, stats,
// init, mol wisp) that callers outside this package used to build as raw
// argv. On a wrapper from NewPlain they send exactly the argv those call
// sites sent.

// ConfigGet returns the value of a bd config key, or "" when it is not set
// (bd prints "<key> (not set)" and exits 0). Informational "Note:" lines bd
// may print are skipped (ParseConfigOutput).
func (b *Beads) ConfigGet(key string) (string, error) {
	out, err := b.run("config", "get", key)
	if err != nil {
		return "", err
	}
	return ParseConfigOutput(out), nil
}

// ConfigSet sets a bd config key.
func (b *Beads) ConfigSet(key, value string) error {
	_, err := b.run("config", "set", key, value)
	return err
}

// SQLCSV runs a declared read through bd sql --csv and returns its records,
// header row included. Only stdout is parsed: bd writes diagnostics to
// stderr, and merging them into the CSV breaks it (gt-m7t).
func (b *Beads) SQLCSV(query beadsql.Query) ([][]string, error) {
	args, err := query.BdArgs("--csv")
	if err != nil {
		return nil, err
	}
	out, err := b.run(args...)
	if err != nil {
		return nil, err
	}
	records, err := csv.NewReader(bytes.NewReader(out)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("csv parse: %w", err)
	}
	return records, nil
}

// CountIssues returns how many rows the issues table holds, closed issues
// included and wisps (their own table) not.
func (b *Beads) CountIssues() (int, error) {
	records, err := b.SQLCSV(beadsql.IssueCount())
	if err != nil {
		return 0, err
	}
	if len(records) < 2 || len(records[1]) == 0 {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(records[1][0]))
	if err != nil {
		return 0, fmt.Errorf("bd sql count: %q is not a number", records[1][0])
	}
	return n, nil
}

// TableExists reports whether the database has a table named name: a query
// on it succeeds.
func (b *Beads) TableExists(name string) bool {
	args, err := beadsql.TableProbe(name).BdArgs()
	if err != nil {
		return false
	}
	_, err = b.run(args...)
	return err == nil
}

// StatsJSON returns bd stats --json. A caller that only needs to know bd
// can open the database checks the error.
func (b *Beads) StatsJSON() ([]byte, error) {
	return b.run("stats", "--json")
}

// Export writes every issue in the database to path as JSONL (bd export
// -o), the file bd's fallback import reads.
func (b *Beads) Export(path string) error {
	_, err := b.run("export", "-o", path)
	return err
}

// InitOptions describe a bd init of a server-mode database.
type InitOptions struct {
	Prefix       string // --prefix, when set
	Database     string // --database, when set
	ServerPort   int    // --server-port, when non-zero
	Force        bool   // --force: re-initialize over an existing database
	DestroyToken string // --destroy-token, the confirmation --force requires
}

// InitDatabase runs bd init --server with opts.
func (b *Beads) InitDatabase(opts InitOptions) error {
	args := []string{"init"}
	if opts.Prefix != "" {
		args = append(args, "--prefix", opts.Prefix)
	}
	if opts.Database != "" {
		args = append(args, "--database", opts.Database)
	}
	args = append(args, "--server")
	if opts.ServerPort != 0 {
		args = append(args, "--server-port", strconv.Itoa(opts.ServerPort))
	}
	if opts.Force {
		args = append(args, "--force")
	}
	if opts.DestroyToken != "" {
		args = append(args, "--destroy-token="+opts.DestroyToken)
	}
	_, err := b.run(args...)
	return err
}

// MolWispList returns the wisps bd mol wisp list reports, with their ID,
// title, status, priority and timestamps. bd 1.x wraps them in an object
// ({"count":..,"wisps":[...]}); a bare array, as older bd printed, is read
// too.
func (b *Beads) MolWispList() ([]*Issue, error) {
	out, err := b.run("mol", "wisp", "list", "--json")
	if err != nil {
		return nil, err
	}
	return parseMolWispList(out)
}

func parseMolWispList(out []byte) ([]*Issue, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	var wisps []*Issue
	if out[0] == '[' {
		if err := json.Unmarshal(out, &wisps); err != nil {
			return nil, fmt.Errorf("parsing bd mol wisp list: %w", err)
		}
	} else {
		var wrapper struct {
			Wisps []*Issue `json:"wisps"`
		}
		if err := json.Unmarshal(out, &wrapper); err != nil {
			return nil, fmt.Errorf("parsing bd mol wisp list: %w", err)
		}
		wisps = wrapper.Wisps
	}
	for _, w := range wisps {
		w.Ephemeral = true
	}
	return wisps, nil
}

// WispGCCandidates returns the IDs bd mol wisp gc would delete for an age
// threshold, without deleting anything (--dry-run): open wisps idle longer
// than age that nothing blocks, hooks or pins, and their unprotected
// dependents. gastown deliberately has no method that runs the real gc: an
// open merge-request wisp queued past the threshold is a candidate too.
func (b *Beads) WispGCCandidates(age time.Duration) ([]string, error) {
	out, err := b.run("mol", "wisp", "gc", "--dry-run", "--json", "--age", age.String())
	if err != nil {
		return nil, err
	}
	var res struct {
		CleanedIDs []string `json:"cleaned_ids"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &res); err != nil {
		return nil, fmt.Errorf("parsing bd mol wisp gc --dry-run: %w", err)
	}
	return res.CleanedIDs, nil
}
