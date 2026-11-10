package usageattrib

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// asstLines mirrors usage_fixtures.asst: one line per content block, all with the same message id.
// With stream, output_tokens grows block by block and only the last line carries the final count.
func asstLines(t *testing.T, mid, model, ts string, inp, cc, cr, out int64, blocks int, stream bool) []string {
	t.Helper()
	var lines []string
	for k := 0; k < blocks; k++ {
		o := out
		if stream {
			o = out * int64(k+1) / int64(blocks)
		}
		line := map[string]any{
			"type":      "assistant",
			"timestamp": ts,
			"uuid":      fmt.Sprintf("u-%s-%d", mid, k),
			"message": map[string]any{
				"id":      mid,
				"model":   model,
				"role":    "assistant",
				"content": []any{},
				"usage": map[string]any{
					"input_tokens":                inp,
					"cache_creation_input_tokens": cc,
					"cache_read_input_tokens":     cr,
					"output_tokens":               o,
				},
			},
		}
		b, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("marshal fixture line: %v", err)
		}
		lines = append(lines, string(b))
	}
	return lines
}

// writeTranscript writes lines, one per line, to a file under t.TempDir() and returns its path.
func writeTranscript(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	body := ""
	for _, ln := range lines {
		body += ln + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

func strPtr(s string) *string { return &s }

func idsOf(msgs []Message) []string {
	var out []string
	for _, m := range msgs {
		if m.ID == nil {
			out = append(out, "<nil>")
		} else {
			out = append(out, *m.ID)
		}
	}
	return out
}

// failingReader returns first, then err on every later read.
type failingReader struct {
	first []byte
	err   error
	read  bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, r.first), nil
	}
	return 0, r.err
}

func TestReaderRepeatedMessageIDCountsOnce(t *testing.T) {
	t.Parallel()
	// Python: fx.asst("m1", "claude-opus-5-5", "2026-10-12T10:00:00Z", 1, 2, 3, 4, blocks=3), then m2.
	lines := asstLines(t, "m1", "claude-opus-5-5", "2026-10-12T10:00:00Z", 1, 2, 3, 4, 3, false)
	lines = append(lines, asstLines(t, "m2", "claude-opus-5-5", "2026-10-12T10:00:02Z", 10, 20, 30, 40, 1, false)...)
	msgs, bad, err := IterAssistantUsage(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("IterAssistantUsage: %v", err)
	}
	if bad != 0 {
		t.Errorf("bad = %d, want 0", bad)
	}
	if got := idsOf(msgs); !reflect.DeepEqual(got, []string{"m1", "m2"}) {
		t.Fatalf("ids = %v, want [m1 m2]", got)
	}
	want := Message{ID: strPtr("m1"), Model: "claude-opus-5-5", TS: "2026-10-12T10:00:00Z",
		Input: 1, CacheCreation: 2, CacheRead: 3, Output: 4}
	if !reflect.DeepEqual(msgs[0], want) {
		t.Errorf("msgs[0] = %+v, want %+v", msgs[0], want)
	}
}

func TestReaderStreamedOutputTakesFinalCountNotFirstBlock(t *testing.T) {
	t.Parallel()
	// Python: fx.asst("m1", ..., 1, 2, 3, 900, blocks=3, stream=True), then one more m1 line with 900.
	// Block outputs are 300, 600, 900; the first line's timestamp and model are kept.
	lines := asstLines(t, "m1", "claude-opus-5-5", "2026-10-12T10:00:00Z", 1, 2, 3, 900, 3, true)
	lines = append(lines, asstLines(t, "m1", "claude-opus-5-5", "2026-10-12T10:00:09Z", 1, 2, 3, 900, 1, false)...)
	msgs, _, err := IterAssistantUsage(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("IterAssistantUsage: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].Output != 900 {
		t.Errorf("Output = %d, want 900", msgs[0].Output)
	}
	if msgs[0].TS != "2026-10-12T10:00:00Z" {
		t.Errorf("TS = %q, want the first line's 2026-10-12T10:00:00Z", msgs[0].TS)
	}
	if got := [3]int64{msgs[0].Input, msgs[0].CacheCreation, msgs[0].CacheRead}; got != [3]int64{1, 2, 3} {
		t.Errorf("input, cache_creation, cache_read = %v, want [1 2 3]", got)
	}
}

func TestReaderLinesWithNoIDAreKeptApart(t *testing.T) {
	t.Parallel()
	// Python: the same line twice, with no message id and no uuid. Two messages, neither merged.
	line := `{"type":"assistant","timestamp":"t","message":{"model":"m","usage":{"input_tokens":1}}}`
	msgs, _, err := IterAssistantUsage(writeTranscript(t, []string{line, line}))
	if err != nil {
		t.Fatalf("IterAssistantUsage: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].ID != nil || msgs[1].ID != nil {
		t.Errorf("ids = %v, want both nil", idsOf(msgs))
	}
}

func TestReaderLinesWithoutUsageSkippedAndBadLinesCounted(t *testing.T) {
	t.Parallel()
	// Python: "{broken" and "[1,2]" are bad; the assistant line without usage, the user line and the
	// blank line are not records and are not counted.
	noUsage := `{"type":"assistant","timestamp":"t","message":{"id":"x","model":"m"}}`
	user := `{"type":"user","message":{"content":"hello"}}`
	lines := []string{"{broken", noUsage, user, "", "[1,2]"}
	lines = append(lines, asstLines(t, "m1", "m", "2026-10-12T10:00:00Z", 1, 1, 1, 1, 1, false)...)
	msgs, bad, err := IterAssistantUsage(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("IterAssistantUsage: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("len(msgs) = %d, want 1", len(msgs))
	}
	if bad != 2 {
		t.Errorf("bad = %d, want 2", bad)
	}
}

func TestReaderUnreadableFileIsError(t *testing.T) {
	t.Parallel()
	msgs, bad, err := IterAssistantUsage(filepath.Join(t.TempDir(), "missing.jsonl"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want a not-exist error", err)
	}
	if msgs != nil || bad != 0 {
		t.Errorf("got msgs=%v bad=%d alongside the error, want none", msgs, bad)
	}
}

func TestReaderReadErrorIsErrorWithNoPartialResult(t *testing.T) {
	t.Parallel()
	// The first read yields one complete usage line, so a swallowed error would return one message.
	boom := errors.New("boom")
	r := &failingReader{first: []byte(asstLines(t, "m1", "m", "2026-10-12T10:00:00Z", 1, 1, 1, 1, 1, false)[0] + "\n"), err: boom}
	msgs, bad, err := ReadAssistantUsage(r)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if msgs != nil || bad != 0 {
		t.Errorf("got msgs=%v bad=%d alongside the error, want none", msgs, bad)
	}
}

func TestReaderSumUsage(t *testing.T) {
	t.Parallel()
	// Python: two messages, then an empty list.
	msgs := []Message{{Input: 1, CacheCreation: 2, CacheRead: 3, Output: 4}, {Input: 10, CacheCreation: 20, CacheRead: 30, Output: 40}}
	want := Totals{Calls: 2, Input: 11, CacheCreation: 22, CacheRead: 33, Output: 44, Uncached: 77}
	if got := SumUsage(msgs); got != want {
		t.Errorf("SumUsage = %+v, want %+v", got, want)
	}
	if got := SumUsage(nil).Calls; got != 0 {
		t.Errorf("SumUsage(nil).Calls = %d, want 0", got)
	}
}

// assertCost checks an EstCost result against an expected value to the 9th decimal place, as the
// Python assertAlmostEqual(places=9) does.
func assertCost(t *testing.T, got float64, ok bool, err error, want float64) {
	t.Helper()
	if err != nil {
		t.Fatalf("EstCost error: %v", err)
	}
	if !ok {
		t.Fatalf("EstCost ok = false, want a price")
	}
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("EstCost = %.12f, want %.12f", got, want)
	}
}

func TestCostFlashOffPeak(t *testing.T) {
	t.Parallel()
	// Python: 10h CDT, (1M*0.15 + 2M*0.003 + 0.5M*0.60) = 0.456
	c, ok, err := EstCost("deepseek-flash", 1000000, 0, 2000000, 500000, "2026-10-12T15:00:00Z")
	assertCost(t, c, ok, err, 0.456)
}

func TestCostPeakDoubles21hCDT(t *testing.T) {
	t.Parallel()
	// Python: 02:00Z is 21h CDT, the same tokens cost twice the off-peak 0.456.
	c, ok, err := EstCost("deepseek-flash", 1000000, 0, 2000000, 500000, "2026-10-12T02:00:00Z")
	assertCost(t, c, ok, err, 0.912)
}

func TestCostPeakWindowEdgesMatchDeepseekAttrib(t *testing.T) {
	t.Parallel()
	// Python: CDT is fixed UTC-5. Peak hours are 20-22 and 1-4 CDT, which is UTC 1,2,3 and 6,7,8,9.
	peakUTC := map[int]bool{1: true, 2: true, 3: true, 6: true, 7: true, 8: true, 9: true}
	for h := 0; h < 24; h++ {
		ts := fmt.Sprintf("2026-10-12T%02d:30:00Z", h)
		c, ok, err := EstCost("deepseek-flash", 1000000, 0, 0, 0, ts)
		want := 0.15
		if peakUTC[h] {
			want = 0.30
		}
		if err != nil || !ok {
			t.Fatalf("utc hour %d: ok=%v err=%v", h, ok, err)
		}
		if math.Abs(c-want) > 1e-9 {
			t.Errorf("utc hour %d: cost = %.12f, want %.12f", h, c, want)
		}
	}
}

func TestCostMissIncludesCacheCreationAndProRates(t *testing.T) {
	t.Parallel()
	// Python: miss = 500000 input + 500000 cache creation = 1M at 0.66; hit 1M at 0.033; output 100k at 1.98.
	c, ok, err := EstCost("deepseek-v4-pro", 500000, 500000, 1000000, 100000, "2026-10-12T15:00:00Z")
	assertCost(t, c, ok, err, 0.66+0.033+0.198)
}

func TestCostModelSuffixIgnored(t *testing.T) {
	t.Parallel()
	// Python: "deepseek-flash[1m]" is priced as "deepseek-flash". One input token at 0.15 per million.
	c, ok, err := EstCost("deepseek-flash[1m]", 1, 0, 0, 0, "2026-10-12T15:00:00Z")
	assertCost(t, c, ok, err, 0.15/1e6)
}

func TestCostClaudeAndUnknownAreNone(t *testing.T) {
	t.Parallel()
	// Python: Claude and unknown models return None before the timestamp is parsed, so a bad
	// timestamp is not an error for them either.
	cases := []struct{ model, ts string }{
		{"claude-opus-5-5", "2026-10-12T15:00:00Z"},
		{"", "2026-10-12T15:00:00Z"},
		{"claude-opus-5-5", "garbage"},
	}
	for _, tc := range cases {
		c, ok, err := EstCost(tc.model, 1, 1, 1, 1, tc.ts)
		if err != nil || ok || c != 0 {
			t.Errorf("EstCost(%q, %q) = (%v, %v, %v), want no cost and no error", tc.model, tc.ts, c, ok, err)
		}
	}
}

func TestCostBadTimestampErrors(t *testing.T) {
	t.Parallel()
	// Python: parse_ts raises ValueError for anything that is not a timestamp. One row per refusal.
	for _, ts := range []string{"garbage", "", "2026-13-01T00:00:00Z", "2026-10-12T25:00:00Z", "2026-10-12T10:00:60Z"} {
		c, ok, err := EstCost("deepseek-flash", 1, 0, 0, 0, ts)
		if err == nil {
			t.Errorf("EstCost(%q) error = nil, want an error", ts)
		}
		if ok || c != 0 {
			t.Errorf("EstCost(%q) = (%v, %v) alongside the error, want no cost", ts, c, ok)
		}
	}
}

func TestCostParseTSMirrorsStrptime(t *testing.T) {
	t.Parallel()
	// Python strptime accepts one-digit fields. A 17-character string has no 19-character prefix to cut.
	got, err := ParseTS("2026-10-2T1:00:00")
	if err != nil {
		t.Fatalf("ParseTS one-digit fields: %v", err)
	}
	if want := time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("ParseTS = %v, want %v", got, want)
	}
	got, err = ParseTS("2026-10-12T18:53:57.123456Z")
	if err != nil {
		t.Fatalf("ParseTS with fraction: %v", err)
	}
	if want := time.Date(2026, 10, 12, 18, 53, 57, 0, time.UTC); !got.Equal(want) {
		t.Errorf("ParseTS = %v, want %v", got, want)
	}
}

func TestCostBaseModelDropsBracketSuffix(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"deepseek-flash[1m]": "deepseek-flash", "deepseek-flash": "deepseek-flash", "": ""} {
		if got := BaseModel(in); got != want {
			t.Errorf("BaseModel(%q) = %q, want %q", in, got, want)
		}
	}
}
