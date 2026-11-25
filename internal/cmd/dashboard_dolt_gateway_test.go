package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// fakeDoltGatewayStats is the section's injected gateway read: the answers that
// each poll returns in turn, the last one repeating.
type fakeDoltGatewayStats struct {
	answers []bdgateway.GatewayStats
	calls   int
}

func (f *fakeDoltGatewayStats) read(context.Context) (bdgateway.GatewayStats, error) {
	i := min(f.calls, len(f.answers)-1)
	f.calls++
	return f.answers[i], nil
}

// gatewayCaps is a stats answer carrying the cap counters the section reads.
func gatewayCaps(inFlight, peak int, refusals uint64) bdgateway.GatewayStats {
	return bdgateway.GatewayStats{InFlight: inFlight, PeakInFlight: peak, CapsPresent: true, TooManyCalls: refusals}
}

// TestDoltGatewaySectionCountsTheRefusalDelta: two reads whose too_many_calls
// went 5 to 9 put the 4 refusals between them in the bucket the second read
// falls in, and the first read counts nothing because the counts start when the
// dashboard starts.
func TestDoltGatewaySectionCountsTheRefusalDelta(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	stats := &fakeDoltGatewayStats{answers: []bdgateway.GatewayStats{gatewayCaps(2, 4, 5), gatewayCaps(1, 6, 9)}}
	section := newDoltGatewaySection(stats.read, "bd-gateway.gt.svc:7440")

	got, err := section.read(context.Background(), start)
	if err != nil {
		t.Fatalf("the first read: %v", err)
	}
	if want := []string{"ten minutes", "429 refusals"}; !slices.Equal(got.Head, want) {
		t.Errorf("Head = %v, want %v", got.Head, want)
	}
	for _, row := range got.Rows {
		if row[1] != "0" {
			t.Errorf("the first read counted %v; the counts start when the dashboard starts", row)
		}
	}
	got, err = section.read(context.Background(), start.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("the second read: %v", err)
	}
	if last, want := got.Rows[len(got.Rows)-1], []string{"15:30-15:40", "4"}; !slices.Equal(last, want) {
		t.Errorf("the bucket the delta fell in = %v, want %v", last, want)
	}
}

// TestDoltGatewaySectionRestartAddsNothing: a counter below its previous value
// means the gateway restarted, so that read adds nothing to any bucket and the
// Note says so; the read after it counts from the restarted gateway's value.
func TestDoltGatewaySectionRestartAddsNothing(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	stats := &fakeDoltGatewayStats{answers: []bdgateway.GatewayStats{gatewayCaps(1, 5, 50), gatewayCaps(0, 0, 2), gatewayCaps(1, 1, 5)}}
	section := newDoltGatewaySection(stats.read, "bd-gateway.gt.svc:7440")

	if _, err := section.read(context.Background(), start); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	got, err := section.read(context.Background(), start.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("the read that saw the restart: %v", err)
	}
	if !strings.Contains(got.Note, "restarted") {
		t.Errorf("Note = %q, want it to say the gateway restarted", got.Note)
	}
	for _, row := range got.Rows {
		if row[1] != "0" {
			t.Errorf("a restarted gateway added %v; it adds nothing for that read", row)
		}
	}
	got, err = section.read(context.Background(), start.Add(20*time.Minute))
	if err != nil {
		t.Fatalf("the read after the restart: %v", err)
	}
	if strings.Contains(got.Note, "restarted") {
		t.Errorf("Note = %q, want the restart note only on the read that saw it", got.Note)
	}
	if last, want := got.Rows[len(got.Rows)-1], []string{"15:40-15:50", "3"}; !slices.Equal(last, want) {
		t.Errorf("the bucket after the restart = %v, want %v", last, want)
	}
}

// TestDoltGatewaySectionOmitsTheRefusalColumn: an answer with no caps claim
// leaves the 429 column out rather than showing zeros, and the note says why.
func TestDoltGatewaySectionOmitsTheRefusalColumn(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	stats := &fakeDoltGatewayStats{answers: []bdgateway.GatewayStats{{}}}
	got, err := newDoltGatewaySection(stats.read, "bd-gateway.gt.svc:7440").read(context.Background(), now)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := []string{"ten minutes"}; !slices.Equal(got.Head, want) {
		t.Errorf("Head = %v, want %v", got.Head, want)
	}
	if !strings.Contains(got.Note, "carry no in-flight or 429 counters") {
		t.Errorf("Note = %q, want it to say the stats carry no in-flight or 429 counters", got.Note)
	}
	for _, row := range got.Rows {
		if len(row) != 1 {
			t.Errorf("row %v has %d cells, want 1 for the head", row, len(row))
		}
	}
}

// TestDoltGatewaySectionCapsGapAddsNothing: an answer that drops the counters
// clears their baseline, so the read after the gap is a new baseline and adds
// nothing, however far apart the two answers that carried them were; the read
// after that counts its delta again.
func TestDoltGatewaySectionCapsGapAddsNothing(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	stats := &fakeDoltGatewayStats{answers: []bdgateway.GatewayStats{gatewayCaps(1, 1, 5), {}, gatewayCaps(1, 1, 9), gatewayCaps(1, 1, 12)}}
	section := newDoltGatewaySection(stats.read, "bd-gateway.gt.svc:7440")

	var got dashboard.DoltSection
	for i := range 3 {
		var err error
		if got, err = section.read(context.Background(), start.Add(time.Duration(i)*10*time.Minute)); err != nil {
			t.Fatalf("read %d: %v", i+1, err)
		}
	}
	for _, row := range got.Rows {
		if row[1] != "0" {
			t.Errorf("the read after a gap without counters added %v; it is a new baseline and adds nothing", row)
		}
	}
	got, err := section.read(context.Background(), start.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("read 4: %v", err)
	}
	if last, want := got.Rows[len(got.Rows)-1], []string{"15:50-16:00", "3"}; !slices.Equal(last, want) {
		t.Errorf("the bucket after the new baseline = %v, want %v", last, want)
	}
}

// TestDoltGatewaySectionLateFetchCountsNothing: two reads whose fetches are
// applied in the reverse order they were taken. The first read's fetch is
// still in flight (its value is the older one, 5) while the second read takes
// its fetch (9) and counts; the late older value must not read as a restart or
// rewind the baseline, so the read after both counts 12 - 9 once, and no read
// says the gateway restarted.
func TestDoltGatewaySectionLateFetchCountsNothing(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	var section *doltGatewaySection
	var inner dashboard.DoltSection
	calls := 0
	stats := func(ctx context.Context) (bdgateway.GatewayStats, error) {
		calls++
		switch calls {
		case 1: // the baseline read
			return gatewayCaps(1, 1, 1), nil
		case 2: // the slow read: while its fetch is out, a newer read fetches and counts
			var err error
			if inner, err = section.read(ctx, start.Add(10*time.Minute)); err != nil {
				t.Errorf("the newer read: %v", err)
			}
			return gatewayCaps(1, 1, 5), nil
		case 3: // the newer read's fetch
			return gatewayCaps(1, 1, 9), nil
		}
		return gatewayCaps(1, 1, 12), nil
	}
	section = newDoltGatewaySection(stats, "bd-gateway.gt.svc:7440")

	if _, err := section.read(context.Background(), start); err != nil {
		t.Fatalf("the baseline read: %v", err)
	}
	late, err := section.read(context.Background(), start.Add(9*time.Minute))
	if err != nil {
		t.Fatalf("the slow read: %v", err)
	}
	if strings.Contains(late.Note, "restarted") || strings.Contains(inner.Note, "restarted") {
		t.Errorf("a late older fetch read as a restart: slow %q, newer %q", late.Note, inner.Note)
	}
	got, err := section.read(context.Background(), start.Add(20*time.Minute))
	if err != nil {
		t.Fatalf("the read after both: %v", err)
	}
	if strings.Contains(got.Note, "restarted") {
		t.Errorf("Note = %q, want no restart", got.Note)
	}
	if last, want := got.Rows[len(got.Rows)-1], []string{"15:40-15:50", "3"}; !slices.Equal(last, want) {
		t.Errorf("the bucket after both reads = %v, want %v (12 - 9 counted once)", last, want)
	}
}

// TestDoltGatewaySectionShowsInFlight: a read whose answer carries in_flight 3
// and peak_in_flight 7 draws the six ten-minute rows, the row holding now last,
// and a note naming both counts and when the per-ten-minute counts begin.
func TestDoltGatewaySectionShowsInFlight(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 22, 15, 24, 0, 0, time.UTC)
	section := newDoltGatewaySection(func(context.Context) (bdgateway.GatewayStats, error) {
		return bdgateway.GatewayStats{InFlight: 3, PeakInFlight: 7, CapsPresent: true}, nil
	}, "bd-gateway.gt.svc:7440")
	got, err := section.read(context.Background(), now)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Title != "bd gateway load" {
		t.Errorf("title = %q, want %q", got.Title, "bd gateway load")
	}
	if len(got.Rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(got.Rows))
	}
	if first, last := got.Rows[0][0], got.Rows[5][0]; first != "14:30-14:40" || last != "15:20-15:30" {
		t.Errorf("rows span %q to %q, want 14:30-14:40 to 15:20-15:30", first, last)
	}
	for _, want := range []string{"in flight now: 3", "peak in flight since start: 7", "start when the dashboard starts"} {
		if !strings.Contains(got.Note, want) {
			t.Errorf("note = %q, want it to hold %q", got.Note, want)
		}
	}

	noCaps := newDoltGatewaySection(func(context.Context) (bdgateway.GatewayStats, error) {
		return bdgateway.GatewayStats{}, nil
	}, "bd-gateway.gt.svc:7440")
	got, err = noCaps.read(context.Background(), now)
	if err != nil {
		t.Fatalf("read without caps: %v", err)
	}
	if !strings.Contains(got.Note, "carry no in-flight or 429 counters") {
		t.Errorf("note = %q, want it to say the counters are missing", got.Note)
	}
}

// TestDoltGatewaySectionErrsNamingTheHost: a stats read that fails is the
// section's error naming the gateway's host and the error, never the bearer
// token the pusher read from its token file.
func TestDoltGatewaySectionErrsNamingTheHost(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	token := "s3cret-token-value"
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	unreachable := bdgateway.GatewayPusher{
		URL:       "http://127.0.0.1:7440",
		TokenFile: tokenFile,
		HTTP:      doerFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") }),
	}
	for _, tc := range []struct {
		name string
		read doltSectionRead
		want string
	}{
		{"a fake stats reader's error", newDoltGatewaySection(
			func(context.Context) (bdgateway.GatewayStats, error) {
				return bdgateway.GatewayStats{}, errors.New("dial tcp: connection refused")
			},
			"bd-gateway.gt.svc:7440").read, "bd-gateway.gt.svc:7440"},
		{"a pusher whose gateway does not answer", newDoltGatewaySection(
			unreachable.Stats, doltGatewayHost(unreachable.URL)).read, "127.0.0.1:7440"},
		{"a real transport error that repeats the whole request URL", newDoltGatewaySection(
			func(context.Context) (bdgateway.GatewayStats, error) {
				return bdgateway.GatewayStats{}, &url.Error{Op: "Get", URL: "http://user:pw" + "@bd-gateway.gt.svc:7440/v1/stats?token=abc", Err: errors.New("connection refused")}
			},
			"bd-gateway.gt.svc:7440").read, "bd-gateway.gt.svc:7440"},
		{"no gateway url configured", newDoltGatewayReader(func(string) string { return "" }), "no gateway url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.read(context.Background(), now)
			if err == nil {
				t.Fatal("read = nil error, want the unreadable gateway refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to name %q", err, tc.want)
			}
			for _, leak := range []string{"http://", "https://", "/v1/stats", "token=abc", "user:pw"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("err = %q, want the host only: it holds %q of the request URL", err, leak)
				}
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("err = %q, want no bearer token", err)
			}
		})
	}
}

// The section is registered under `gateway` by its own file.
func TestDoltGatewaySectionRegisters(t *testing.T) {
	t.Parallel()

	for _, r := range doltSections.snapshot() {
		if r.key == doltGatewaySectionKey {
			return
		}
	}
	t.Fatalf("the Dolt panel has no %s section registered", doltGatewaySectionKey)
}

func uint64p(n uint64) *uint64 { return &n }

// withConflicts is a stats answer carrying the cap counters and a conflict count.
func withConflicts(refusals, conflicts uint64) bdgateway.GatewayStats {
	s := gatewayCaps(1, 2, refusals)
	s.Conflicts = uint64p(conflicts)
	return s
}

// TestDoltGatewaySectionCountsConflicts: the conflict counter is counted like
// the refusals, in its own column: the first read is a baseline and the second
// puts what the counter gained in the bucket it falls in.
func TestDoltGatewaySectionCountsConflicts(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	stats := &fakeDoltGatewayStats{answers: []bdgateway.GatewayStats{withConflicts(5, 2), withConflicts(6, 5)}}
	section := newDoltGatewaySection(stats.read, "bd-gateway.gt.svc:7440")

	got, err := section.read(context.Background(), start)
	if err != nil {
		t.Fatalf("the first read: %v", err)
	}
	if want := []string{"ten minutes", "429 refusals", "conflicts"}; !slices.Equal(got.Head, want) {
		t.Errorf("Head = %v, want %v", got.Head, want)
	}
	got, err = section.read(context.Background(), start.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("the second read: %v", err)
	}
	if last, want := got.Rows[len(got.Rows)-1], []string{"15:30-15:40", "1", "3"}; !slices.Equal(last, want) {
		t.Errorf("the bucket the deltas fell in = %v, want %v", last, want)
	}
}

// TestDoltGatewaySectionOmitsTheConflictColumn: an answer without the conflict
// count leaves exactly that column out rather than showing zeros, and the note
// says so.
func TestDoltGatewaySectionOmitsTheConflictColumn(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	stats := &fakeDoltGatewayStats{answers: []bdgateway.GatewayStats{gatewayCaps(1, 2, 3)}}
	got, err := newDoltGatewaySection(stats.read, "bd-gateway.gt.svc:7440").read(context.Background(), now)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := []string{"ten minutes", "429 refusals"}; !slices.Equal(got.Head, want) {
		t.Errorf("Head = %v, want %v", got.Head, want)
	}
	for _, row := range got.Rows {
		if len(row) != 2 {
			t.Errorf("row %v has %d cells, want 2 for the head", row, len(row))
		}
	}
	if !strings.Contains(got.Note, "no conflict count") {
		t.Errorf("Note = %q, want it to say the stats carry no conflict count", got.Note)
	}
}

// TestDoltGatewaySectionConflictGapAddsNothing: an answer that drops the conflict
// count clears its baseline, so the read after the gap adds nothing to it however
// far apart the answers that carried it were, and the read after that counts its
// delta again.
func TestDoltGatewaySectionConflictGapAddsNothing(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	stats := &fakeDoltGatewayStats{answers: []bdgateway.GatewayStats{withConflicts(1, 5), gatewayCaps(1, 2, 1), withConflicts(1, 9), withConflicts(1, 12)}}
	section := newDoltGatewaySection(stats.read, "bd-gateway.gt.svc:7440")
	var got dashboard.DoltSection
	for i := range 3 {
		var err error
		if got, err = section.read(context.Background(), start.Add(time.Duration(i)*10*time.Minute)); err != nil {
			t.Fatalf("read %d: %v", i+1, err)
		}
	}
	for _, row := range got.Rows {
		if row[2] != "0" {
			t.Errorf("the read after a gap in the conflict count added %v; it is a new baseline and adds nothing", row)
		}
	}
	got, err := section.read(context.Background(), start.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("read 4: %v", err)
	}
	if last := got.Rows[len(got.Rows)-1]; last[2] != "3" {
		t.Errorf("the bucket after the new baseline = %v, want 3 in the conflicts column", last)
	}
}

// withLanes is a stats answer carrying the cap counters and the lane counters;
// a nil cap hit count leaves that field out of the answer.
func withLanes(refusals, waits uint64, capHits *uint64) bdgateway.GatewayStats {
	s := gatewayCaps(1, 2, refusals)
	s.LaneWaits, s.LaneCapHits = uint64p(waits), capHits
	return s
}

// TestDoltGatewaySectionCountsLanes: the lane wait and lane cap hit counters are
// counted like the refusals, each in its own column.
func TestDoltGatewaySectionCountsLanes(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	stats := &fakeDoltGatewayStats{answers: []bdgateway.GatewayStats{withLanes(5, 1, uint64p(0)), withLanes(6, 4, uint64p(1))}}
	section := newDoltGatewaySection(stats.read, "bd-gateway.gt.svc:7440")

	got, err := section.read(context.Background(), start)
	if err != nil {
		t.Fatalf("the first read: %v", err)
	}
	if want := []string{"ten minutes", "429 refusals", "lane waits", "lane cap hits"}; !slices.Equal(got.Head, want) {
		t.Errorf("Head = %v, want %v", got.Head, want)
	}
	got, err = section.read(context.Background(), start.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("the second read: %v", err)
	}
	if last, want := got.Rows[len(got.Rows)-1], []string{"15:30-15:40", "1", "3", "1"}; !slices.Equal(last, want) {
		t.Errorf("the bucket the deltas fell in = %v, want %v", last, want)
	}
}

// TestDoltGatewaySectionOmitsTheLaneColumns: an answer without lane waits leaves
// both lane columns out, one with lane waits and no cap hits leaves only the cap
// hit column out, and the note names each.
func TestDoltGatewaySectionOmitsTheLaneColumns(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		with bdgateway.GatewayStats
		head []string
		note string
	}{
		{"no lane waits", gatewayCaps(1, 2, 3), []string{"ten minutes", "429 refusals"}, "no lane counts"},
		{"no cap hits", withLanes(3, 1, nil), []string{"ten minutes", "429 refusals", "lane waits"}, "no lane cap hits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stats := &fakeDoltGatewayStats{answers: []bdgateway.GatewayStats{tc.with}}
			got, err := newDoltGatewaySection(stats.read, "bd-gateway.gt.svc:7440").read(context.Background(), now)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !slices.Equal(got.Head, tc.head) {
				t.Errorf("Head = %v, want %v", got.Head, tc.head)
			}
			for _, row := range got.Rows {
				if len(row) != len(tc.head) {
					t.Errorf("row %v has %d cells, want %d for the head", row, len(row), len(tc.head))
				}
			}
			if !strings.Contains(got.Note, tc.note) {
				t.Errorf("Note = %q, want it to say %q", got.Note, tc.note)
			}
		})
	}
}

// TestDoltGatewaySectionLaneGapAddsNothing: an answer that drops the lane counters
// clears their baseline, so the read after the gap adds nothing to them and the
// read after that counts the delta again.
func TestDoltGatewaySectionLaneGapAddsNothing(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 22, 15, 20, 0, 0, time.UTC)
	stats := &fakeDoltGatewayStats{answers: []bdgateway.GatewayStats{
		withLanes(1, 5, uint64p(5)), gatewayCaps(1, 2, 1), withLanes(1, 9, uint64p(9)), withLanes(1, 12, uint64p(12))}}
	section := newDoltGatewaySection(stats.read, "bd-gateway.gt.svc:7440")
	var got dashboard.DoltSection
	for i := range 3 {
		var err error
		if got, err = section.read(context.Background(), start.Add(time.Duration(i)*10*time.Minute)); err != nil {
			t.Fatalf("read %d: %v", i+1, err)
		}
	}
	for _, row := range got.Rows {
		if row[2] != "0" || row[3] != "0" {
			t.Errorf("the read after a gap in the lane counters added %v; it is a new baseline and adds nothing", row)
		}
	}
	got, err := section.read(context.Background(), start.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("read 4: %v", err)
	}
	if last := got.Rows[len(got.Rows)-1]; last[2] != "3" || last[3] != "3" {
		t.Errorf("the bucket after the new baseline = %v, want 3 in both lane columns", last)
	}
}
