package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/usage"
)

// usageSampleStore is an in-memory claude_plan store for the verb tests.
type usageSampleStore struct {
	rows    []usage.ClaudePlan
	readErr error
}

func (s *usageSampleStore) NewestClaudePlan(context.Context) (*usage.ClaudePlan, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	if len(s.rows) == 0 {
		return nil, nil
	}
	p := s.rows[len(s.rows)-1]
	return &p, nil
}

func (s *usageSampleStore) WriteClaudePlanAfter(ctx context.Context, p usage.ClaudePlan, prev *usage.ClaudePlan) error {
	newest, err := s.NewestClaudePlan(ctx)
	if err != nil {
		return err
	}
	switch {
	case newest == nil && prev == nil:
	case newest == nil || prev == nil:
		return usage.ErrNewestChanged
	case !newest.At.Equal(prev.At) || !newest.SameValues(*prev):
		return usage.ErrNewestChanged
	}
	s.rows = append(s.rows, p)
	return nil
}

// usageSampleRig is one test's clock and store. The clock is a field the test
// moves between runs, so the sample time is the test's, not the wall clock's.
type usageSampleRig struct {
	now   time.Time
	store *usageSampleStore
}

func newUsageSampleRig() *usageSampleRig {
	return &usageSampleRig{
		now:   time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC),
		store: &usageSampleStore{},
	}
}

// deps wires the rig into the command. The clock closure reads r.now, so a
// test that moves r.now between runs moves the sample time with it.
func (r *usageSampleRig) deps() usageSampleDeps {
	return usageSampleDeps{
		now:       func() time.Time { return r.now },
		openStore: func() (usage.Store, error) { return r.store, nil },
	}
}

// runUsageSampleCmd executes one `gt usage sample` with payload on stdin and
// returns what it wrote to stderr and the command's error.
func runUsageSampleCmd(t *testing.T, deps usageSampleDeps, payload string, args ...string) (string, error) {
	t.Helper()
	var stderr bytes.Buffer
	c := newUsageSampleCmd(deps)
	c.SetArgs(args)
	c.SetIn(strings.NewReader(payload))
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&stderr)
	err := c.Execute()
	return stderr.String(), err
}

// statusLinePayload is one status-line document with both limit windows. The
// percentages are JSON number text, so a test can also send a bad one.
func statusLinePayload(fivePct, sevenPct string) string {
	return `{"model":{"id":"claude-opus"},"rate_limits":{` +
		`"five_hour":{"used_percentage":` + fivePct + `,"resets_at":1794400000},` +
		`"seven_day":{"used_percentage":` + sevenPct + `,"resets_at":1794700000}}}`
}

func TestUsageSampleWritesOneRowFromInjectedClock(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()

	stderr, err := runUsageSampleCmd(t, r.deps(), statusLinePayload("41", "12"))
	if err != nil {
		t.Fatalf("exit err = %v, want nil", err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want none", stderr)
	}
	if len(r.store.rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(r.store.rows))
	}
	row := r.store.rows[0]
	if row.FiveHourPct != 41 || row.SevenDayPct != 12 {
		t.Errorf("percentages = %v, %v; want 41, 12", row.FiveHourPct, row.SevenDayPct)
	}
	if row.FiveHourResetsAt.Unix() != 1794400000 || row.SevenDayResetsAt.Unix() != 1794700000 {
		t.Errorf("resets = %v, %v; want the payload's epoch seconds", row.FiveHourResetsAt, row.SevenDayResetsAt)
	}
	if !row.At.Equal(r.now) || row.At.Location() != time.UTC {
		t.Errorf("at = %v, want the injected clock %v in UTC", row.At, r.now)
	}
}

func TestUsageSampleDedupesUnchangedReadingWithinTenMinutes(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	payload := statusLinePayload("41", "12")
	start := r.now

	feed := func(at time.Duration) {
		t.Helper()
		r.now = start.Add(at)
		if stderr, err := runUsageSampleCmd(t, r.deps(), payload); err != nil || stderr != "" {
			t.Fatalf("feed at +%v: err=%v stderr=%q", at, err, stderr)
		}
	}
	feed(0)
	feed(time.Minute)
	if len(r.store.rows) != 1 {
		t.Fatalf("rows after two feeds inside ten minutes = %d, want 1", len(r.store.rows))
	}
	feed(11 * time.Minute)
	if len(r.store.rows) != 2 {
		t.Errorf("rows after a feed past ten minutes = %d, want 2", len(r.store.rows))
	}
}

func TestUsageSampleWritesNothingWithoutRateLimits(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()

	stderr, err := runUsageSampleCmd(t, r.deps(), `{"model":{"id":"claude-opus"}}`, "--strict")
	if err != nil {
		t.Fatalf("exit err = %v, want nil: no rate_limits is not a failure even under --strict", err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want none", stderr)
	}
	if len(r.store.rows) != 0 {
		t.Errorf("rows = %d, want 0", len(r.store.rows))
	}
}

func TestUsageSampleMalformedJSONExitsZeroWithoutStrict(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()

	stderr, err := runUsageSampleCmd(t, r.deps(), `{"rate_limits":`)
	if err != nil {
		t.Fatalf("exit err = %v, want nil: the status line must not fail", err)
	}
	assertOneStderrLine(t, stderr)
	if len(r.store.rows) != 0 {
		t.Errorf("rows = %d, want 0", len(r.store.rows))
	}
}

func TestUsageSampleMalformedJSONExitsNonZeroWithStrict(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()

	stderr, err := runUsageSampleCmd(t, r.deps(), `{"rate_limits":`, "--strict")
	if err == nil {
		t.Fatal("exit err = nil, want non-zero under --strict")
	}
	if code := exitCodeForError(err); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	assertOneStderrLine(t, stderr)
	if len(r.store.rows) != 0 {
		t.Errorf("rows = %d, want 0", len(r.store.rows))
	}
}

func TestUsageSampleReportsRefusedStoreOnStderr(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	deps := r.deps()
	deps.openStore = func() (usage.Store, error) {
		return nil, errors.New("store refused by test")
	}

	stderr, err := runUsageSampleCmd(t, deps, statusLinePayload("41", "12"))
	if err != nil {
		t.Fatalf("exit err = %v, want nil without --strict", err)
	}
	assertOneStderrLine(t, stderr)
	if !strings.Contains(stderr, "store refused by test") {
		t.Errorf("stderr = %q, want it to carry the opener's error", stderr)
	}
	if len(r.store.rows) != 0 {
		t.Errorf("rows = %d, want 0", len(r.store.rows))
	}
}

func TestUsageSampleRefusedStoreExitsNonZeroWithStrict(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	deps := r.deps()
	deps.openStore = func() (usage.Store, error) {
		return nil, errors.New("store refused by test")
	}

	_, err := runUsageSampleCmd(t, deps, statusLinePayload("41", "12"), "--strict")
	if code := exitCodeForError(err); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if len(r.store.rows) != 0 {
		t.Errorf("rows = %d, want 0", len(r.store.rows))
	}
}

// fakeClaudePlanDeps wires the command's store to the beads fake the way
// production wires it to the town's beads client, so the verb runs the same
// adapter in the test.
func fakeClaudePlanDeps(r *usageSampleRig, client beads.Client) usageSampleDeps {
	deps := r.deps()
	deps.openStore = func() (usage.Store, error) { return clientClaudePlanStore{client: client}, nil }
	return deps
}

// fakeClaudePlanRows reads every claude_plan row the fake holds, newest first.
func fakeClaudePlanRows(t *testing.T, f *beadsfake.Fake) []beads.Sample {
	t.Helper()
	rows, err := f.ReadCostSamples(context.Background(), beads.SampleKindClaudePlan, time.Time{}, 0)
	if err != nil {
		t.Fatalf("ReadCostSamples: %v", err)
	}
	return rows
}

func TestUsageSampleWritesOneClaudePlanRowToTheBeadsStore(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	// bd stores At at second precision, so the row's time is the injected clock
	// cut to the second rather than the clock's sub-second value.
	r.now = r.now.Add(350 * time.Millisecond)
	f := beadsfake.New()

	stderr, err := runUsageSampleCmd(t, fakeClaudePlanDeps(r, f), statusLinePayload("41", "12"), "--strict")
	if err != nil {
		t.Fatalf("exit err = %v, want nil; stderr = %q", err, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want none", stderr)
	}
	rows := fakeClaudePlanRows(t, f)
	if len(rows) != 1 {
		t.Fatalf("claude_plan rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Kind != beads.SampleKindClaudePlan {
		t.Errorf("kind = %q, want %q", row.Kind, beads.SampleKindClaudePlan)
	}
	if want := r.now.Truncate(time.Second); !row.At.Equal(want) {
		t.Errorf("at = %v, want the injected clock at second precision %v", row.At, want)
	}
	if row.FiveHourPct == nil || *row.FiveHourPct != 41 || row.SevenDayPct == nil || *row.SevenDayPct != 12 {
		t.Errorf("percentages = %v, %v; want 41, 12", row.FiveHourPct, row.SevenDayPct)
	}
	if row.FiveHourResetsAt == nil || row.FiveHourResetsAt.Unix() != 1794400000 ||
		row.SevenDayResetsAt == nil || row.SevenDayResetsAt.Unix() != 1794700000 {
		t.Errorf("resets = %v, %v; want the payload's epoch seconds", row.FiveHourResetsAt, row.SevenDayResetsAt)
	}
	balance, err := f.ReadCostSamples(context.Background(), beads.SampleKindDeepSeekBalance, time.Time{}, 0)
	if err != nil {
		t.Fatalf("ReadCostSamples(deepseek_balance): %v", err)
	}
	if len(balance) != 0 {
		t.Errorf("deepseek_balance rows = %d, want 0: the reading is a claude_plan row", len(balance))
	}
}

func TestUsageSampleDedupesAgainstTheBeadsStore(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	f := beadsfake.New()
	deps := fakeClaudePlanDeps(r, f)
	payload := statusLinePayload("41", "12")
	start := r.now

	feed := func(at time.Duration) {
		t.Helper()
		r.now = start.Add(at)
		if stderr, err := runUsageSampleCmd(t, deps, payload); err != nil || stderr != "" {
			t.Fatalf("feed at +%v: err=%v stderr=%q", at, err, stderr)
		}
	}
	feed(0)
	feed(time.Minute)
	if rows := fakeClaudePlanRows(t, f); len(rows) != 1 {
		t.Fatalf("rows after two feeds inside ten minutes = %d, want 1", len(rows))
	}
	feed(11 * time.Minute)
	if rows := fakeClaudePlanRows(t, f); len(rows) != 2 {
		t.Errorf("rows after a feed past ten minutes = %d, want 2", len(rows))
	}
}

func TestUsageSampleRefusesANewestRowWithNoReading(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		drop    func(*beads.Sample)
		missing string
	}{
		{"five-hour percentage", func(s *beads.Sample) { s.FiveHourPct = nil }, "five_hour_pct"},
		{"five-hour reset", func(s *beads.Sample) { s.FiveHourResetsAt = nil }, "five_hour_resets_at"},
		{"seven-day percentage", func(s *beads.Sample) { s.SevenDayPct = nil }, "seven_day_pct"},
		{"seven-day reset", func(s *beads.Sample) { s.SevenDayResetsAt = nil }, "seven_day_resets_at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newUsageSampleRig()
			f := beadsfake.New()
			// A claude_plan row missing one reading is not a reading. The fake
			// stores it as bd would: the kind and time alone pass its checks.
			at := r.now.Add(-time.Hour)
			pct41, pct12 := 41.0, 12.0
			fiveResets := time.Unix(1794400000, 0).UTC()
			sevenResets := time.Unix(1794700000, 0).UTC()
			row := beads.Sample{
				Kind:             beads.SampleKindClaudePlan,
				At:               at,
				FiveHourPct:      &pct41,
				FiveHourResetsAt: &fiveResets,
				SevenDayPct:      &pct12,
				SevenDayResetsAt: &sevenResets,
			}
			tc.drop(&row)
			if err := f.WriteCostSample(context.Background(), row); err != nil {
				t.Fatalf("seed the incomplete row: %v", err)
			}

			stderr, err := runUsageSampleCmd(t, fakeClaudePlanDeps(r, f), statusLinePayload("41", "12"), "--strict")
			if err == nil {
				t.Fatal("exit err = nil, want non-zero: a row with no reading must not read as a plan")
			}
			assertOneStderrLine(t, stderr)
			if !strings.Contains(stderr, tc.missing) {
				t.Errorf("stderr = %q, want the missing reading %q named", stderr, tc.missing)
			}
			if rows := fakeClaudePlanRows(t, f); len(rows) != 1 {
				t.Errorf("rows = %d, want the one seeded row: the reading was not written", len(rows))
			}
		})
	}
}

// racingClient appends another writer's row on the store's re-read, after the
// command read the newest row itself. The store's compare must see it.
type racingClient struct {
	*beadsfake.Fake
	reads      int
	competitor beads.Sample
}

func (c *racingClient) ReadCostSamples(ctx context.Context, kind string, since time.Time, limit int) ([]beads.Sample, error) {
	c.reads++
	if c.reads == 2 {
		if err := c.Fake.WriteCostSample(ctx, c.competitor); err != nil {
			return nil, err
		}
	}
	return c.Fake.ReadCostSamples(ctx, kind, since, limit)
}

func TestUsageSampleDoesNotDuplicateWhenAnotherWriterAppendsFirst(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	at := r.now.UTC().Truncate(time.Second)
	pct41, pct12 := 41.0, 12.0
	fiveResets := time.Unix(1794400000, 0).UTC()
	sevenResets := time.Unix(1794700000, 0).UTC()
	client := &racingClient{
		Fake: beadsfake.New(),
		competitor: beads.Sample{
			Kind:             beads.SampleKindClaudePlan,
			At:               at,
			FiveHourPct:      &pct41,
			FiveHourResetsAt: &fiveResets,
			SevenDayPct:      &pct12,
			SevenDayResetsAt: &sevenResets,
		},
	}

	stderr, err := runUsageSampleCmd(t, fakeClaudePlanDeps(r, client), statusLinePayload("41", "12"), "--strict")
	if err != nil {
		t.Fatalf("exit err = %v, want nil: the competitor's row covers the reading; stderr = %q", err, stderr)
	}
	rows := fakeClaudePlanRows(t, client.Fake)
	if len(rows) != 1 {
		t.Fatalf("claude_plan rows = %d, want only the competitor's: a row the store's compare did not see must not be duplicated", len(rows))
	}
	if !rows[0].At.Equal(at) {
		t.Errorf("row at = %v, want the competitor's %v", rows[0].At, at)
	}
}

func TestUsageSampleRefusesWhenTheBeadsStoreReadFails(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	f := beadsfake.New()
	f.FailWith("sample list", errors.New("sample list failed"))

	stderr, err := runUsageSampleCmd(t, fakeClaudePlanDeps(r, f), statusLinePayload("41", "12"), "--strict")
	if err == nil {
		t.Fatal("exit err = nil, want non-zero: an unreadable newest row must not write a duplicate")
	}
	assertOneStderrLine(t, stderr)
	if !strings.Contains(stderr, "sample list failed") {
		t.Errorf("stderr = %q, want the store's error", stderr)
	}
	f.FailWith("sample list", nil)
	if rows := fakeClaudePlanRows(t, f); len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}

// flakyRereadClient serves the command's first read and fails every read after
// it, so the store's own re-read inside the append is the one that fails.
type flakyRereadClient struct {
	*beadsfake.Fake
	reads int
}

func (c *flakyRereadClient) ReadCostSamples(ctx context.Context, kind string, since time.Time, limit int) ([]beads.Sample, error) {
	c.reads++
	if c.reads > 1 {
		return nil, errors.New("sample list failed on the re-read")
	}
	return c.Fake.ReadCostSamples(ctx, kind, since, limit)
}

func TestUsageSampleRefusesWhenTheStoreRereadFails(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	client := &flakyRereadClient{Fake: beadsfake.New()}

	stderr, err := runUsageSampleCmd(t, fakeClaudePlanDeps(r, client), statusLinePayload("41", "12"), "--strict")
	if err == nil {
		t.Fatal("exit err = nil, want non-zero: a re-read that fails must not append over a row it could not check")
	}
	assertOneStderrLine(t, stderr)
	if !strings.Contains(stderr, "sample list failed on the re-read") {
		t.Errorf("stderr = %q, want the store's error", stderr)
	}
	if rows := fakeClaudePlanRows(t, client.Fake); len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}

func TestUsageSampleReportsWhenTheBeadsStoreWriteFails(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	f := beadsfake.New()
	f.FailWith("sample add", errors.New("sample add failed"))

	stderr, err := runUsageSampleCmd(t, fakeClaudePlanDeps(r, f), statusLinePayload("41", "12"), "--strict")
	if err == nil {
		t.Fatal("exit err = nil, want non-zero under --strict: the row did not land")
	}
	assertOneStderrLine(t, stderr)
	if !strings.Contains(stderr, "sample add failed") {
		t.Errorf("stderr = %q, want the store's error", stderr)
	}
	if rows := fakeClaudePlanRows(t, f); len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}

func TestOpenClaudePlanStoreUsesTheTownItResolves(t *testing.T) {
	t.Parallel()
	store, err := openClaudePlanStoreFrom(func() (string, error) { return t.TempDir(), nil })
	if err != nil {
		t.Fatalf("openClaudePlanStoreFrom = %v, want a store over the resolved town", err)
	}
	if store == nil {
		t.Fatal("store = nil, want a store over the resolved town")
	}
}

func TestOpenClaudePlanStoreRefusesWhenTheTownIsNotFound(t *testing.T) {
	t.Parallel()
	_, err := openClaudePlanStoreFrom(func() (string, error) { return "", errors.New("no Gas Town here") })
	if err == nil {
		t.Fatal("openClaudePlanStoreFrom err = nil, want an error: a store over no town is never opened")
	}
	if !strings.Contains(err.Error(), "no Gas Town here") {
		t.Errorf("err = %v, want it to carry the finder's error", err)
	}
}

func TestUsageSampleRefusesWhenNewestRowUnreadable(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	r.store.readErr = errors.New("sample list failed")

	stderr, err := runUsageSampleCmd(t, r.deps(), statusLinePayload("41", "12"), "--strict")
	if err == nil {
		t.Fatal("exit err = nil, want non-zero: an unreadable newest row must not write a duplicate")
	}
	assertOneStderrLine(t, stderr)
	if len(r.store.rows) != 0 {
		t.Errorf("rows = %d, want 0", len(r.store.rows))
	}
}

func TestUsageSampleHelpDocumentsStdinContractAndDedupe(t *testing.T) {
	t.Parallel()
	c := newUsageSampleCmd(newUsageSampleRig().deps())
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--help"})
	if err := c.Execute(); err != nil {
		t.Fatalf("--help: %v", err)
	}
	help := out.String()
	for _, want := range []string{"stdin", "rate_limits.five_hour", "rate_limits.seven_day", "ten minutes", "--strict"} {
		if !strings.Contains(help, want) {
			t.Errorf("help does not mention %q", want)
		}
	}
}

func TestUsageSampleStdinReadFailureIsRefused(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	var stderr bytes.Buffer
	c := newUsageSampleCmd(r.deps())
	c.SetArgs([]string{"--strict"})
	c.SetIn(unreadableStdin{})
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&stderr)

	err := c.Execute()
	if err == nil {
		t.Fatal("exit err = nil, want non-zero: an unreadable payload must not read as no reading")
	}
	assertOneStderrLine(t, stderr.String())
	if !strings.Contains(stderr.String(), "read stdin") {
		t.Errorf("stderr = %q, want the read failure named, not the parse of an empty payload", stderr.String())
	}
	if len(r.store.rows) != 0 {
		t.Errorf("rows = %d, want 0", len(r.store.rows))
	}
}

func TestUsageSampleRefusesOversizedPayload(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	// The padding field sits ahead of a valid reading, so the refusal comes from
	// the size check and not from a parse error.
	payload := `{"pad":"` + strings.Repeat("x", maxStatusLineBytes) + `",` + statusLinePayload("41", "12")[1:]

	stderr, err := runUsageSampleCmd(t, r.deps(), payload, "--strict")
	if err == nil {
		t.Fatal("exit err = nil, want non-zero: an oversized payload must not be read in full")
	}
	assertOneStderrLine(t, stderr)
	if !strings.Contains(stderr, strconv.Itoa(maxStatusLineBytes)) {
		t.Errorf("stderr = %q, want it to name the %d-byte cap", stderr, maxStatusLineBytes)
	}
	if len(r.store.rows) != 0 {
		t.Errorf("rows = %d, want 0", len(r.store.rows))
	}
}

func TestUsageSampleStopsReadingPastCap(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	in := &countingStdin{left: 4 * maxStatusLineBytes}
	var stderr bytes.Buffer
	c := newUsageSampleCmd(r.deps())
	c.SetArgs([]string{"--strict"})
	c.SetIn(in)
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&stderr)

	if err := c.Execute(); err == nil {
		t.Fatal("exit err = nil, want non-zero: an oversized payload must be refused")
	}
	if in.read > maxStatusLineBytes+1 {
		t.Errorf("read %d bytes of a %d-byte payload, want at most %d: the read must stop at the cap",
			in.read, in.left+in.read, maxStatusLineBytes+1)
	}
}

func TestUsageSampleAcceptsPayloadAtCap(t *testing.T) {
	t.Parallel()
	r := newUsageSampleRig()
	const prefix = `{"pad":"`
	rest := statusLinePayload("41", "12")[1:]
	pad := strings.Repeat("x", maxStatusLineBytes-len(prefix)-len(`",`)-len(rest))
	payload := prefix + pad + `",` + rest
	if len(payload) != maxStatusLineBytes {
		t.Fatalf("payload = %d bytes, want exactly the cap %d", len(payload), maxStatusLineBytes)
	}

	stderr, err := runUsageSampleCmd(t, r.deps(), payload, "--strict")
	if err != nil {
		t.Fatalf("exit err = %v, want nil at exactly the cap; stderr = %q", err, stderr)
	}
	if len(r.store.rows) != 1 {
		t.Errorf("rows = %d, want 1", len(r.store.rows))
	}
}

// countingStdin yields left bytes of padding and counts what was read, so a
// test can tell a bounded read from a read of the whole payload.
type countingStdin struct {
	left int
	read int
}

func (c *countingStdin) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.left)
	for i := range n {
		p[i] = 'x'
	}
	c.left -= n
	c.read += n
	return n, nil
}

// unreadableStdin is a stdin that cannot be read.
type unreadableStdin struct{}

func (unreadableStdin) Read([]byte) (int, error) { return 0, errors.New("stdin closed") }

func assertOneStderrLine(t *testing.T, stderr string) {
	t.Helper()
	if n := strings.Count(stderr, "\n"); n != 1 {
		t.Errorf("stderr has %d lines, want exactly one: %q", n, stderr)
	}
}
