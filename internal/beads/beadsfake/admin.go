package beadsfake

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
)

// Admin is the bd maintenance surface: config keys, table probes, stats and
// wisp listing, as *beads.Beads provides them. RunAdminContract pins the
// fake to bd on it.
type Admin interface {
	ConfigGet(key string) (string, error)
	ConfigSet(key, value string) error
	CountIssues() (int, error)
	TableExists(name string) bool
	StatsJSON() ([]byte, error)
	MolWispList() ([]*beads.Issue, error)
}

var _ Admin = (*Fake)(nil)

// bdTables are the tables a fresh bd database has that callers probe for.
var bdTables = []string{"issues", "wisps", "labels", "wisp_labels", "dependencies", "wisp_dependencies", "comments", "events", "config"}

// ErrNotScripted is returned by SQL and SQLCSV when the test did not script
// an answer (OnSQL). SQL is not modelled.
var ErrNotScripted = errors.New("beadsfake: SQL not scripted")

func (f *Fake) configMap() map[string]string {
	if f.config == nil {
		f.config = map[string]string{"issue_prefix": f.prefix}
	}
	return f.config
}

// ConfigGet returns a config key's value, "" when unset. issue_prefix is
// the fake's prefix until set.
func (f *Fake) ConfigGet(key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("config get " + key); err != nil {
		return "", err
	}
	return f.configMap()[key], nil
}

// ConfigSet sets a config key.
func (f *Fake) ConfigSet(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("config set " + key); err != nil {
		return err
	}
	f.configMap()[key] = value
	return nil
}

// CountIssues counts the issues, closed ones included and wisps not.
func (f *Fake) CountIssues() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.issues {
		if !r.issue.Ephemeral {
			n++
		}
	}
	return n, nil
}

// TableExists reports whether the table exists: bd's standard tables do,
// less any DropTable removed.
func (f *Fake) TableExists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dropped[name] {
		return false
	}
	for _, t := range bdTables {
		if t == name {
			return true
		}
	}
	return false
}

// DropTable makes TableExists report name missing, for tests of databases
// that predate a table.
func (f *Fake) DropTable(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dropped == nil {
		f.dropped = map[string]bool{}
	}
	f.dropped[name] = true
}

// StatsJSON returns {"summary":{"total_issues":N,...}} over the issues (not
// the wisps).
func (f *Fake) StatsJSON() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("stats"); err != nil {
		return nil, err
	}
	var total, open, closed, inProgress int
	for _, r := range f.issues {
		if r.issue.Ephemeral {
			continue
		}
		total++
		switch r.issue.Status {
		case string(beads.StatusOpen):
			open++
		case string(beads.StatusClosed):
			closed++
		case string(beads.StatusInProgress):
			inProgress++
		}
	}
	return json.Marshal(map[string]any{
		"schema_version": 1,
		"summary": map[string]int{
			"total_issues": total, "open_issues": open, "closed_issues": closed, "in_progress_issues": inProgress,
		},
	})
}

// MolWispList returns the wisps that are not closed, newest first, as bd mol
// wisp list reports them.
func (f *Fake) MolWispList() ([]*beads.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("mol wisp list"); err != nil {
		return nil, err
	}
	var out []*beads.Issue
	for _, r := range f.ordered() {
		if r.issue.Ephemeral && r.issue.Status != string(beads.StatusClosed) {
			out = append(out, f.snapshot(r))
		}
	}
	return out, nil
}

// GCWisps records a bd mol wisp gc. Garbage collection is not modelled:
// nothing is deleted. GCCount reports the calls.
func (f *Fake) GCWisps() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("mol wisp gc"); err != nil {
		return err
	}
	f.gcCalls++
	return nil
}

// GCCount is how many times GCWisps ran.
func (f *Fake) GCCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gcCalls
}

// InitDatabase records a bd init and sets issue_prefix when opts has one.
// Inits reports the calls.
func (f *Fake) InitDatabase(opts beads.InitOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("init"); err != nil {
		return err
	}
	f.inits = append(f.inits, opts)
	if opts.Prefix != "" {
		f.configMap()["issue_prefix"] = opts.Prefix
	}
	return nil
}

// Inits returns the InitDatabase calls, in order.
func (f *Fake) Inits() []beads.InitOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]beads.InitOptions(nil), f.inits...)
}

// OnSQL scripts SQL and SQLCSV: answer receives the statement and returns
// CSV records (header first) or an error. Without it both return
// ErrNotScripted.
func (f *Fake) OnSQL(answer func(query string) ([][]string, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sql = answer
}

// SQLStatements returns every statement SQL and SQLCSV received, in order.
func (f *Fake) SQLStatements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sqlLog...)
}

func (f *Fake) runSQL(query string) ([][]string, error) {
	f.mu.Lock()
	f.sqlLog = append(f.sqlLog, query)
	answer := f.sql
	f.mu.Unlock()
	if answer == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotScripted, query)
	}
	return answer(query)
}

// SQLCSV returns the scripted records for query.
func (f *Fake) SQLCSV(query string) ([][]string, error) { return f.runSQL(query) }

// SQL returns the scripted records for query, rendered one line per record
// with fields joined by commas.
func (f *Fake) SQL(query string) ([]byte, error) {
	records, err := f.runSQL(query)
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, rec := range records {
		for i, field := range rec {
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, field...)
		}
		out = append(out, '\n')
	}
	return out, nil
}

// FailWith makes the maintenance command named by op fail with err until
// cleared with a nil err. op is the bd subcommand as the fake names it:
// "config get <key>", "config set <key>", "stats", "mol wisp list",
// "mol wisp gc" or "init".
func (f *Fake) FailWith(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures == nil {
		f.failures = map[string]error{}
	}
	if err == nil {
		delete(f.failures, op)
		return
	}
	f.failures[op] = err
}

// failure returns the scripted failure for op. Callers hold f.mu.
func (f *Fake) failure(op string) error {
	return f.failures[op]
}
