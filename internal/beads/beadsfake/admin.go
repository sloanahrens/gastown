package beadsfake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

var _ beads.Admin = (*Fake)(nil)

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

func (f *Fake) kvMap() map[string]string {
	if f.kv == nil {
		f.kv = map[string]string{}
	}
	return f.kv
}

// KVList returns the fake's key-value store.
func (f *Fake) KVList() (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("kv list"); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(f.kvMap()))
	for k, v := range f.kvMap() {
		out[k] = v
	}
	return out, nil
}

// KVSet stores value at key, replacing what the fake held there.
func (f *Fake) KVSet(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("kv set " + key); err != nil {
		return err
	}
	f.kvMap()[key] = value
	return nil
}

// KVGet returns key's value, or an error wrapping beads.ErrNotFound.
func (f *Fake) KVGet(key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("kv get " + key); err != nil {
		return "", err
	}
	v, ok := f.kvMap()[key]
	if !ok {
		return "", fmt.Errorf("kv get %s: %w", key, beads.ErrNotFound)
	}
	return v, nil
}

// Version returns the bd report the fake was built with: a fork build at the
// schema level beadsql declares.
func (f *Fake) Version() (beads.VersionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("version"); err != nil {
		return beads.VersionInfo{}, err
	}
	return *f.version, nil
}

// UpdateOffersFlag answers true for every flag: the fake stands in for a bd
// built with the tree, which offers whatever the tree's callers ask for.
func (f *Fake) UpdateOffersFlag(string) (bool, error) { return true, nil }

// SchemaLevel returns the migration level the fake's database is at, which is
// the level its bd report names unless a test scripts otherwise.
func (f *Fake) SchemaLevel() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("schema level"); err != nil {
		return 0, err
	}
	return f.schemaLevel, nil
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
// "config get <key>", "config set <key>", "kv list", "kv get <key>",
// "kv set <key>", "stats", "mol wisp list", "mol wisp gc" (the dry run),
// "sample add", "sample list", "export", "init", "version", "schema level" or
// "show" (every Show call, for a store whose answer is unknown rather than
// not-found).
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

// sampleKinds are the kinds the fake stores. The cost-sample methods refuse
// every other kind, as *Beads does, rather than keeping a row a reader of the
// typed kinds would never see.
var sampleKinds = map[string]bool{
	beads.SampleKindDeepSeekBalance: true,
	beads.SampleKindClaudePlan:      true,
}

// checkSampleKind refuses a kind neither SampleKind constant names.
func checkSampleKind(kind string) error {
	if !sampleKinds[kind] {
		return fmt.Errorf("unknown sample kind %q (want %s or %s)", kind, beads.SampleKindDeepSeekBalance, beads.SampleKindClaudePlan)
	}
	return nil
}

// checkSampleReadings refuses a reading JSON, bd's payload format, cannot
// carry: a NaN or an infinity, named by its payload field.
func checkSampleReadings(s beads.Sample) error {
	for _, f := range []struct {
		field string
		v     *float64
	}{
		{"total", s.Total},
		{"granted", s.Granted},
		{"topped_up", s.ToppedUp},
		{"five_hour_pct", s.FiveHourPct},
		{"seven_day_pct", s.SevenDayPct},
	} {
		if f.v != nil && (math.IsNaN(*f.v) || math.IsInf(*f.v, 0)) {
			return fmt.Errorf("%s %v is not a finite number", f.field, *f.v)
		}
	}
	return nil
}

// WriteCostSample appends s to the fake's samples table. As bd's sample add
// does, it refuses a kind this package does not type, a zero At, and a reading
// the payload cannot carry, and it stores At at second precision in UTC. The
// row is a copy, so changing s afterwards does not change what a read returns.
func (f *Fake) WriteCostSample(_ context.Context, s beads.Sample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("sample add"); err != nil {
		return err
	}
	if err := checkSampleKind(s.Kind); err != nil {
		return fmt.Errorf("sample add: %w", err)
	}
	if s.At.IsZero() {
		return fmt.Errorf("sample add: no sample time given")
	}
	if err := checkSampleReadings(s); err != nil {
		return fmt.Errorf("sample add: %w", err)
	}
	s.At = s.At.UTC().Truncate(time.Second)
	f.samples = append(f.samples, cloneSample(s))
	return nil
}

// ReadCostSamples returns the fake's rows of kind, newest first: a row with a
// later At is listed before an earlier one, and rows at the same At in reverse
// write order. A non-zero since keeps the rows at or after it (at second
// precision, as bd's --since filter receives it) and a positive limit caps the
// count; zero returns every row, where bd leaves its own default. An unknown
// kind or a negative limit is an error, and the rows are copies.
func (f *Fake) ReadCostSamples(_ context.Context, kind string, since time.Time, limit int) ([]beads.Sample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("sample list"); err != nil {
		return nil, err
	}
	if err := checkSampleKind(kind); err != nil {
		return nil, fmt.Errorf("sample list: %w", err)
	}
	if limit < 0 {
		return nil, fmt.Errorf("sample list: limit %d is negative", limit)
	}
	type row struct {
		sample beads.Sample
		seq    int
	}
	var rows []row
	for i, s := range f.samples {
		if s.Kind == kind {
			rows = append(rows, row{sample: s, seq: i})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].sample.At.Equal(rows[j].sample.At) {
			return rows[i].sample.At.After(rows[j].sample.At)
		}
		return rows[i].seq > rows[j].seq
	})
	var out []beads.Sample
	for _, r := range rows {
		if !since.IsZero() && r.sample.At.Before(since.UTC().Truncate(time.Second)) {
			continue
		}
		if limit > 0 && len(out) == limit {
			break
		}
		out = append(out, cloneSample(r.sample))
	}
	return out, nil
}

// cloneSample copies s so a stored row and the sample a caller holds never
// share memory.
func cloneSample(s beads.Sample) beads.Sample {
	out := s
	out.Total = clonePtr(s.Total)
	out.Granted = clonePtr(s.Granted)
	out.ToppedUp = clonePtr(s.ToppedUp)
	out.FiveHourPct = clonePtr(s.FiveHourPct)
	out.SevenDayPct = clonePtr(s.SevenDayPct)
	out.FiveHourResetsAt = clonePtr(s.FiveHourResetsAt)
	out.SevenDayResetsAt = clonePtr(s.SevenDayResetsAt)
	return out
}

// clonePtr copies p, nil staying nil.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
