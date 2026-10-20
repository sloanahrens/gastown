package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/attention/jevreplay"
	"github.com/steveyegge/gastown/internal/jev"
)

// jevReplayTown writes a queue with four item lives and one standing item:
//
//	esc:hq-1   escalation, acked, cleared    (decided by kind, no call)
//	red:one    acked, cleared                (history only: acks.json forgot it)
//	bd:quiet   cleared with no ack
//	bd:slow    cleared with no ack
//	bd:now     standing
func jevReplayTown(t *testing.T) string {
	t.Helper()
	town := attentionTown(t)
	base := time.Now().Add(-5 * time.Hour).Truncate(time.Second)
	mk := func(key string, kind attention.Kind, min int, state attention.EventState) attention.Event {
		return attention.Event{
			TS: base.Add(time.Duration(min) * time.Minute), Class: kind, Severity: attention.SeverityLow,
			Text: "summary of " + key, Key: key, State: state,
		}
	}
	require.NoError(t, attention.AppendEvents(town, []attention.Event{
		mk("esc:hq-1", attention.KindEscalation, 0, attention.EventNew),
		mk("red:one", attention.KindRedMain, 1, attention.EventNew),
		mk("bd:quiet", attention.KindBDSlow, 2, attention.EventNew),
		mk("bd:slow", attention.KindBDSlow, 3, attention.EventNew),
		mk("bd:now", attention.KindBDSlow, 4, attention.EventNew),
		mk("esc:hq-1", attention.KindEscalation, 20, attention.EventCleared),
		mk("red:one", attention.KindRedMain, 21, attention.EventCleared),
		mk("bd:quiet", attention.KindBDSlow, 22, attention.EventCleared),
		mk("bd:slow", attention.KindBDSlow, 23, attention.EventCleared),
	}))
	now := time.Now()
	require.NoError(t, attention.WriteState(town, attention.State{
		Updated: now,
		Items: []attention.Item{{
			Key: "bd:now", Kind: attention.KindBDSlow, Severity: attention.SeverityLow,
			Summary: "summary of bd:now", FirstSeen: base.Add(4 * time.Minute), LastSeen: now,
		}},
	}))
	// Two acks land in the history; their items cleared afterwards.
	var lines []byte
	for i, k := range []struct {
		key  string
		kind attention.Kind
	}{{"esc:hq-1", attention.KindEscalation}, {"red:one", attention.KindRedMain}} {
		line, err := json.Marshal(attention.AckRecord{TS: base.Add(time.Duration(10+i) * time.Minute), Key: k.key, Kind: k.kind})
		require.NoError(t, err)
		lines = append(lines, append(line, '\n')...)
	}
	require.NoError(t, os.WriteFile(attention.AcksHistoryPath(town), lines, 0o644))
	return town
}

// attentionFiles maps every file under the attention directory to a digest
// of its bytes and its modification time.
func attentionFiles(t *testing.T, town string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.Walk(attention.Dir(town), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		sum := sha256.Sum256(data)
		out[p] = hex.EncodeToString(sum[:]) + info.ModTime().String()
		return nil
	}))
	return out
}

// roundTripFunc serves each request in-process, so the real client runs
// without a socket.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// jevTransport answers every Choice with confidence .9: needs-you for red:one,
// noise for everything else. It counts the calls.
func jevTransport() (http.RoundTripper, *atomic.Int32) {
	var n atomic.Int32
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n.Add(1)
		body, _ := io.ReadAll(r.Body)
		choice, other := "noise", "needs-you"
		if strings.Contains(string(body), "summary of red:one") {
			choice, other = "needs-you", "noise"
		}
		data, err := json.Marshal(map[string]any{
			"model": jev.Model,
			"answers": map[string]any{"q": map[string]any{
				"type": "choice", "choice": choice, "confidence": 0.9,
				"probabilities": map[string]float64{choice: 0.9, other: 0.05, "auto-fixable": 0.05},
			}},
			"usage": map[string]int{"input_tokens": 400, "output_tokens": 100},
		})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(bytes.NewReader(data)), Request: r,
		}, nil
	})
	return rt, &n
}

// jevServer is jevTransport behind a client, for the replay tests.
func jevServer(t *testing.T) (*jev.Client, *atomic.Int32) {
	t.Helper()
	rt, n := jevTransport()
	c, err := jev.NewClient(jev.WithKey("test-key"), jev.WithBaseURL("http://jev.test/v1/systemone"),
		jev.WithHTTPClient(&http.Client{Transport: rt}))
	require.NoError(t, err)
	return c, n
}

func TestAttentionJevReplayReportsFixtureNumbers(t *testing.T) {
	t.Parallel()
	town := jevReplayTown(t)
	c, calls := jevServer(t)
	out := filepath.Join(t.TempDir(), "report.txt")
	before := attentionFiles(t, town)

	var buf bytes.Buffer
	opts := jevreplay.Options{Floor: 0.7, MaxSpendUSD: 5, USDPerBillionTokens: 42}
	require.NoError(t, attentionJevReplay(context.Background(), &buf, town, c, opts, out))

	assert.EqualValues(t, 4, calls.Load(), "the escalation is decided by its kind, with no call")
	report := buf.String()
	// Tier 2: esc and red:one acked (history), quiet and slow cleared alone;
	// standing bd:now has no label. Jev: red:one needs-you (TP); esc forced
	// to needs-you (TP); the two noise answers are right (TN).
	assert.Contains(t, report, "labeled items: 4 (2 needed a human by the proxy)")
	assert.Contains(t, report, "TP 2, FP 0, FN 0, TN 2")
	assert.Contains(t, report, "precision: 100.0%")
	assert.Contains(t, report, "recall:    100.0%")
	assert.Contains(t, report, "Forced to needs-you by a fail-safe rule: 1 (escalation kind 1")
	// Four calls of 500 tokens at $42 per billion tokens.
	assert.Contains(t, report, "Spend: $0.0001 of $5.00 cap (4 calls, 1600 input tokens, 400 output tokens")
	assert.NotContains(t, report, "STOPPED")

	// Tier 1 sees no ack once the item cleared, so it finds no human need.
	assert.Contains(t, report, "labeled items: 4 (0 needed a human by the proxy)")

	written, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, report, string(written), "--out holds the report that was printed")
	assert.Equal(t, before, attentionFiles(t, town), "no file under the attention directory changed")
}

func TestAttentionJevReplaySpendCapStopsTheRun(t *testing.T) {
	t.Parallel()
	town := jevReplayTown(t)
	c, calls := jevServer(t)
	var buf bytes.Buffer
	// 500 tokens a call at $4e6 per billion is $2 a call: two fit under $5.
	opts := jevreplay.Options{Floor: 0.7, MaxSpendUSD: 5, USDPerBillionTokens: 4e6}
	require.NoError(t, attentionJevReplay(context.Background(), &buf, town, c, opts, ""))
	assert.EqualValues(t, 2, calls.Load())
	// The escalation is reached free, ahead of the two calls.
	assert.Contains(t, buf.String(), "STOPPED at the spend cap after 3 of 5 items")
	assert.Contains(t, buf.String(), "Spend: $4.0000 of $5.00 cap")
}

func TestAttentionJevReplayRefusesOptionsBeforeBuildingTheClient(t *testing.T) {
	t.Parallel()
	town := jevReplayTown(t)
	dir := attention.Dir(town)
	before := attentionFiles(t, town)
	good := jevreplay.Options{Floor: 0.7, MaxSpendUSD: 5, USDPerBillionTokens: 42}
	elsewhere := filepath.Join(t.TempDir(), "report.txt")

	for name, tc := range map[string]struct {
		mutate func(*jevreplay.Options)
		out    string
		want   string
	}{
		"no price (the flag's default)": {func(o *jevreplay.Options) { o.USDPerBillionTokens = 0 }, elsewhere, "price per billion tokens"},
		"NaN cap":                       {func(o *jevreplay.Options) { o.MaxSpendUSD = math.NaN() }, elsewhere, "spend cap"},
		"zero cap":                      {func(o *jevreplay.Options) { o.MaxSpendUSD = 0 }, elsewhere, "spend cap"},
		"floor above one":               {func(o *jevreplay.Options) { o.Floor = 1.5 }, elsewhere, "confidence-floor"},
		"out under the attention dir":   {func(*jevreplay.Options) {}, filepath.Join(dir, "report.txt"), "attention directory"},
	} {
		opts := good
		tc.mutate(&opts)
		newClassifier := func() (jevreplay.Classifier, error) {
			t.Errorf("%s: the client was built though the options were refused", name)
			return nil, errors.New("not reached")
		}
		var buf bytes.Buffer
		err := jevReplayWithFlags(context.Background(), &buf, town, newClassifier, opts, tc.out)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), tc.want, name)
		assert.Empty(t, buf.String(), "%s: nothing is printed", name)
	}
	assert.Equal(t, before, attentionFiles(t, town))
	_, err := os.Stat(elsewhere)
	assert.True(t, os.IsNotExist(err), "no report is written when the options are refused")

	// Good options build the client and replay.
	c, _ := jevServer(t)
	var buf bytes.Buffer
	require.NoError(t, jevReplayWithFlags(context.Background(), &buf, town,
		func() (jevreplay.Classifier, error) { return c, nil }, good, elsewhere))
	assert.Contains(t, buf.String(), "TP 2, FP 0, FN 0, TN 2")
}

func TestAttentionJevReplayRefusesOutUnderTheAttentionDirectory(t *testing.T) {
	t.Parallel()
	town := jevReplayTown(t)
	dir := attention.Dir(town)
	for _, out := range []string{
		filepath.Join(dir, "report.txt"),
		attention.StatePath(town),
		filepath.Join(dir, "sub", "report.txt"),
		filepath.Join(dir, "..", "attention", "report.txt"),
	} {
		assert.Error(t, checkJevReplayOut(town, out), out)
	}
	assert.NoError(t, checkJevReplayOut(town, ""))
	assert.NoError(t, checkJevReplayOut(town, filepath.Join(t.TempDir(), "report.txt")))
	assert.NoError(t, checkJevReplayOut(town, dir+"-report.txt"),
		"a sibling whose name starts with the directory's is not under it")

	// A symlink out of the temp area into the attention directory is under it.
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))
	assert.Error(t, checkJevReplayOut(town, filepath.Join(link, "report.txt")))
}

func TestAttentionJevReplayRecordsUsageInTheTownLedger(t *testing.T) {
	t.Parallel()
	town := jevReplayTown(t)
	rt, _ := jevTransport()
	var warnings bytes.Buffer
	c, err := newJevReplayClient(town, &warnings,
		jev.WithKey("test-key"), jev.WithBaseURL("http://jev.test/v1/systemone"),
		jev.WithHTTPClient(&http.Client{Transport: rt}))
	require.NoError(t, err)

	var buf bytes.Buffer
	opts := jevreplay.Options{Floor: 0.7, MaxSpendUSD: 5, USDPerBillionTokens: 42}
	require.NoError(t, attentionJevReplay(context.Background(), &buf, town, c, opts, ""))

	path := jev.UsageLedgerPath(town)
	assert.Equal(t, filepath.Join(town, ".runtime", "jev", "usage.jsonl"), path,
		"the ledger sits under the town, so a test town never writes the real one")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	require.Len(t, lines, 4, "one line per call, and the escalation is decided by kind with no call")
	for _, line := range lines {
		var rec jev.LedgerRecord
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		assert.Equal(t, jevReplayCaller, rec.Caller)
		assert.Equal(t, 400, rec.InputTokens)
		assert.Equal(t, 100, rec.OutputTokens)
		assert.False(t, rec.TS.IsZero())
	}
	assert.Empty(t, warnings.String())
}

// TestAttentionJevReplayCommandWiresItsClientToTheTown runs the command's
// wiring with a stub client factory, so the closure the command hands the
// replay is exercised: the factory gets the town root and the command's
// stderr, and the replay's report reaches the command's stdout.
func TestAttentionJevReplayCommandWiresItsClientToTheTown(t *testing.T) {
	t.Parallel()
	town := jevReplayTown(t)
	c, _ := jevServer(t)
	var gotTown string
	var gotWarn io.Writer
	newClient := func(townRoot string, warn io.Writer, _ ...jev.Option) (*jev.Client, error) {
		gotTown, gotWarn = townRoot, warn
		return c, nil
	}

	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	opts := jevreplay.Options{Floor: 0.7, MaxSpendUSD: 5, USDPerBillionTokens: 42}
	require.NoError(t, attentionJevReplayIn(cmd, town, opts, "", newClient))

	assert.Equal(t, town, gotTown, "the client is built for the town being replayed")
	assert.Equal(t, &stderr, gotWarn, "ledger warnings go to the command's stderr")
	assert.Contains(t, stdout.String(), "TP 2, FP 0, FN 0, TN 2")
}

func TestAttentionJevReplayIsHiddenUnderAttention(t *testing.T) {
	t.Parallel()
	assert.True(t, attentionJevReplayCmd.Hidden)
	assert.Equal(t, attentionCmd, attentionJevReplayCmd.Parent())
}
