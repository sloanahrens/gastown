package beadsfake

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

var _ beads.Admin = (*Fake)(nil)

// bdTables are the tables a fresh bd database has that callers probe for.
var bdTables = []string{"issues", "wisps", "labels", "wisp_labels", "dependencies", "wisp_dependencies", "comments", "events", "config"}

// ErrNotScripted is returned by SQLCSV when the test did not script an
// answer (OnSQL). SQL is not modeled.
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

// protectedStatuses are the statuses bd's age gc never reclaims: work in
// progress (in_progress, blocked, hooked) and frozen (deferred, pinned).
var protectedStatuses = map[string]bool{
	"in_progress": true, "blocked": true, "hooked": true, "deferred": true, "pinned": true,
}

// WispGCCandidates returns the wisps bd mol wisp gc --dry-run reports for
// age: open wisps idle longer than age that no open issue blocks and no
// open issue hooks (hook_bead), plus, as bd cascades, their unprotected
// wisp dependents of any age. Nothing is deleted; the Fake has no gc.
func (f *Fake) WispGCCandidates(age time.Duration) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("mol wisp gc"); err != nil {
		return nil, err
	}
	hooked := map[string]bool{}
	for _, r := range f.issues {
		if r.issue.HookBead != "" && r.issue.Status != string(beads.StatusClosed) {
			hooked[r.issue.HookBead] = true
		}
	}
	protected := func(r *record) bool {
		return !r.issue.Ephemeral || r.issue.Status == string(beads.StatusClosed) ||
			protectedStatuses[r.issue.Status] || f.blocked(r) || hooked[r.issue.ID]
	}
	now := f.clock.Now()
	picked := map[string]bool{}
	var out []string
	for _, r := range f.ordered() {
		if protected(r) {
			continue
		}
		updated, err := time.Parse(time.RFC3339, r.issue.UpdatedAt)
		if err == nil && now.Sub(updated) > age {
			picked[r.issue.ID] = true
			out = append(out, r.issue.ID)
		}
	}
	// Cascade: wisps depending on a candidate, transitively.
	for grew := true; grew; {
		grew = false
		for _, r := range f.ordered() {
			if picked[r.issue.ID] || protected(r) {
				continue
			}
			for _, e := range r.deps {
				if picked[e.to] {
					picked[r.issue.ID] = true
					out = append(out, r.issue.ID)
					grew = true
					break
				}
			}
		}
	}
	return out, nil
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

// OnSQL scripts SQLCSV: answer receives the statement and returns CSV
// records (header first) or an error. Without it SQLCSV returns
// ErrNotScripted.
func (f *Fake) OnSQL(answer func(query string) ([][]string, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sql = answer
}

// SQLStatements returns every statement SQLCSV received, in order.
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
func (f *Fake) SQLCSV(query beadsql.Query) ([][]string, error) { return f.runSQL(query.String()) }

// FailWith makes the maintenance command named by op fail with err until
// cleared with a nil err. op is the bd subcommand as the fake names it:
// "config get <key>", "config set <key>", "stats", "mol wisp list",
// "mol wisp gc" (the dry run), "export" or "init".
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

// journalWrite appends a journal record for id's state now. Callers hold
// f.mu.
func (f *Fake) journalWrite(op, id string) {
	rec := beads.EventRecord{Seq: int64(len(f.journal) + 1), TS: f.now(), Op: op, IssueID: id, Actor: f.actor}
	if r, ok := f.issues[id]; ok {
		rec.Status = r.issue.Status
	}
	f.journal = append(f.journal, rec)
}

// EventsTail returns the journal records after since, at most limit of them
// (0 = all). Dependency edits are not journaled here, though bd journals
// them. The fake keeps every record, so it never reports truncation.
func (f *Fake) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("events tail"); err != nil {
		return nil, err
	}
	page := &beads.EventsPage{NextSince: since}
	for _, r := range f.journal {
		if r.Seq <= since {
			continue
		}
		if limit > 0 && len(page.Records) == limit {
			page.More = true
			break
		}
		page.Records = append(page.Records, r)
		page.NextSince = r.Seq
	}
	return page, nil
}

// Export writes every issue, wisps left out, to path as JSONL in creation
// order: one issue object a line, as Show returns it.
func (f *Fake) Export(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("export"); err != nil {
		return err
	}
	var records []*record
	for _, r := range f.issues {
		if !r.issue.Ephemeral {
			records = append(records, r)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].seq < records[j].seq })
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range records {
		if err := enc.Encode(f.snapshot(r)); err != nil {
			return err
		}
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
