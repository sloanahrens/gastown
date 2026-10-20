package dedupe

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
)

// hmJSON is the shape `hm query --json` prints: a bead hit matched on its
// title, one on its description, one on a comment, a commit with no matched
// chunk, and a bead hit whose provenance is missing.
const hmJSON = `[
  {"rank":1,"kind":"bead","rig":"gastown","id":"gt-a","title":"t","child_provenance":{"field":"title"}},
  {"rank":2,"kind":"bead","rig":"gastown","id":"gt-b","title":"t","child_provenance":{"field":"description"}},
  {"rank":3,"kind":"bead","rig":"gastown","id":"gt-c","title":"t","child_provenance":{"field":"comment:3"}},
  {"rank":4,"kind":"commit","rig":"gastown","id":"aa0939cf","title":"t","child_provenance":null},
  {"rank":5,"kind":"bead","rig":"gastown","id":"gt-d","title":"t"}
]`

func TestJevDedupeParseHits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    []Hit
		wantErr bool
	}{
		{
			name: "keeps kind, id and the matched field in rank order",
			in:   hmJSON,
			want: []Hit{
				{Kind: "bead", ID: "gt-a", Field: "title"},
				{Kind: "bead", ID: "gt-b", Field: "description"},
				{Kind: "bead", ID: "gt-c", Field: "comment:3"},
				{Kind: "commit", ID: "aa0939cf"},
				{Kind: "bead", ID: "gt-d"},
			},
		},
		{name: "an empty result is no hits", in: `[]`, want: []Hit{}},
		{name: "text that is not JSON is an error", in: `no results`, wantErr: true},
		{name: "an object where a list belongs is an error", in: `{"hits":[]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseHits([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseHits error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("hits = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Only a bead hit that matched the title or the description is offered: a hit
// with no matched field cannot be told from a comment match, so it is dropped.
func TestJevDedupeCandidatesFromHMOutput(t *testing.T) {
	t.Parallel()
	hits, err := parseHits([]byte(hmJSON))
	if err != nil {
		t.Fatal(err)
	}
	all := []*beads.Issue{
		bead("gt-a", "A", "", 0),
		bead("gt-b", "B", "", 1),
		bead("gt-c", "C", "", 2),
		bead("gt-d", "D", "", 3),
		bead("gt-fnd", "Found: x", "", 10),
	}
	h := newHarness(t, all, map[string][]Hit{"Found: x": hits},
		fakeAnswer{ans: &jev.Answer{Choice: "gt-a", Confidence: 0.9}})

	mustRun(t, h.pass)

	got := h.lines(t)[0]["candidates_offered"]
	if want := []any{"gt-a", "gt-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("candidates_offered = %v, want %v", got, want)
	}
}

func TestJevDedupeHMSearcherRunsHMQuery(t *testing.T) {
	t.Parallel()

	t.Run("passes the rig, the limit and the text after a --", func(t *testing.T) {
		t.Parallel()
		var gotArgs []string
		s := hmSearcher{rig: "gastown", run: func(_ context.Context, args ...string) ([]byte, error) {
			gotArgs = args
			return []byte(hmJSON), nil
		}}

		hits, err := s.Search(context.Background(), "-dash title")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		want := []string{"query", "--json", "--rig", "gastown", "--limit", "20", "--", "-dash title"}
		if !reflect.DeepEqual(gotArgs, want) {
			t.Errorf("hm args = %q, want %q", gotArgs, want)
		}
		if len(hits) != 5 {
			t.Errorf("got %d hits, want 5", len(hits))
		}
	})

	t.Run("an hm failure is an error, not an empty result", func(t *testing.T) {
		t.Parallel()
		s := hmSearcher{rig: "gastown", run: func(context.Context, ...string) ([]byte, error) {
			return nil, errors.New("exit status 1")
		}}
		if _, err := s.Search(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "hm query") {
			t.Errorf("Search error = %v, want one naming hm query", err)
		}
	})

	t.Run("output that is not JSON is an error", func(t *testing.T) {
		t.Parallel()
		s := hmSearcher{rig: "gastown", run: func(context.Context, ...string) ([]byte, error) {
			return []byte("warming index..."), nil
		}}
		if _, err := s.Search(context.Background(), "x"); err == nil {
			t.Error("Search of non-JSON output returned no error")
		}
	})

	t.Run("each query is bounded by its own timeout", func(t *testing.T) {
		t.Parallel()
		s := hmSearcher{rig: "gastown", run: func(ctx context.Context, _ ...string) ([]byte, error) {
			dl, ok := ctx.Deadline()
			if !ok || time.Until(dl) > hmTimeout {
				t.Errorf("hm ran with deadline %v (set %v), want one within %v", dl, ok, hmTimeout)
			}
			return []byte(`[]`), nil
		}}
		if _, err := s.Search(context.Background(), "x"); err != nil {
			t.Fatalf("Search: %v", err)
		}
	})
}

func TestJevDedupeOpenRejectsBadSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		s       Settings
		wantErr string
	}{
		{name: "no price", s: Settings{}, wantErr: "usd_per_billion_tokens"},
		{name: "negative price", s: Settings{USDPerBillionTokens: -1}, wantErr: "usd_per_billion_tokens"},
		{name: "infinite price", s: Settings{USDPerBillionTokens: math.Inf(1)}, wantErr: "usd_per_billion_tokens"},
		{name: "NaN price", s: Settings{USDPerBillionTokens: math.NaN()}, wantErr: "usd_per_billion_tokens"},
		{name: "negative cap", s: Settings{USDPerBillionTokens: 3, MaxUSDPerDay: -1}, wantErr: "max_usd_per_day"},
		{name: "infinite cap", s: Settings{USDPerBillionTokens: 3, MaxUSDPerDay: math.Inf(1)}, wantErr: "max_usd_per_day"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Open(t.TempDir(), tt.s, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Open error = %v, want one naming %s", err, tt.wantErr)
			}
		})
	}
}

// spentToday sums this job's ledger lines since local midnight and no one
// else's.
func TestJevDedupeSpentTodayPricesOnlyThisJobsLinesSinceMidnight(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.Local)
	midnight := time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local)
	rec := func(ts time.Time, caller string, in, out int) string {
		return `{"ts":"` + ts.Format(time.RFC3339) + `","caller":"` + caller +
			`","input_tokens":` + strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `}`
	}
	ledger := jev.UsageLedgerPath(town)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		rec(midnight.Add(-time.Minute), Caller, 5_000_000, 0), // yesterday
		rec(midnight, Caller, 300_000_000, 100_000_000),       // today, this job
		rec(now.Add(-time.Hour), "other-job", 900_000_000, 0), // today, another job
	}, "\n") + "\n"
	if err := os.WriteFile(ledger, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := spentToday(town, 2, func() time.Time { return now })()
	if err != nil {
		t.Fatalf("spentToday: %v", err)
	}
	// 400M tokens at 2 USD per billion.
	if math.Abs(got-0.8) > 1e-9 {
		t.Errorf("spent = %v, want 0.8", got)
	}
}

func TestJevDedupeSpentTodayWithNoLedgerIsZero(t *testing.T) {
	t.Parallel()
	got, err := spentToday(t.TempDir(), 2, time.Now)()
	if err != nil || got != 0 {
		t.Errorf("spentToday = %v, %v, want 0 and no error", got, err)
	}
}
