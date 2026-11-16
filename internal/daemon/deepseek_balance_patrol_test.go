package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deepseekbalance"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// deepSeekBalanceTestDaemon builds a daemon whose patrol ticks inline, over the
// key file the test names. Its rows and escalations go to an in-memory store
// and notifier, so a tick that reads a balance spawns no bd or gt.
func deepSeekBalanceTestDaemon(t *testing.T, logged io.Writer, keyFile string) *Daemon {
	t.Helper()
	store := beadsfake.New()
	return &Daemon{
		config:              &Config{TownRoot: t.TempDir()},
		logger:              log.New(logged, "", 0),
		notifier:            notifyfake.New(),
		openCostSampleBeads: func() beads.Client { return store },
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			DeepSeekBalance: &agentconfig.DeepSeekBalanceConfig{Enabled: true, KeyFile: keyFile},
		}},
	}
}

// deepSeekBalanceFetches is the tick's fetch seam: the keys it was handed and
// the reading or error it answers.
type deepSeekBalanceFetches struct {
	keys    []string
	reading deepseekbalance.Sample
	err     error
}

// seams is the tick's seams over the production key reader and this recorder.
func (f *deepSeekBalanceFetches) seams() deepSeekBalanceTickSeams {
	return deepSeekBalanceTickSeams{
		readKey: deepSeekBalanceReadKey,
		fetch: func(_ context.Context, key string) (deepseekbalance.Sample, error) {
			f.keys = append(f.keys, key)
			return f.reading, f.err
		},
		evaluate: deepseekbalance.Evaluate,
	}
}

// TestDeepSeekPatrolTicker: the patrol gets a ticker of its own at the interval
// its block names, and none when the town does not name it, when it is
// disabled, or when the interval is under the floor.
func TestDeepSeekPatrolTicker(t *testing.T) {
	t.Parallel()
	var intervals []time.Duration
	ticks := make(chan time.Time)
	stops := 0
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
	}
	d.newDeepSeekBalanceTicker = func(interval time.Duration) (<-chan time.Time, func()) {
		intervals = append(intervals, interval)
		return ticks, func() { stops++ }
	}

	if channel, stop := d.startDeepSeekBalanceTicker(); channel != nil || stop != nil {
		t.Error("a town that names no deepseek_balance started its ticker")
	}
	if len(intervals) != 0 {
		t.Fatalf("a town that names no deepseek_balance asked for tickers at %v, want none", intervals)
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		DeepSeekBalance: &agentconfig.DeepSeekBalanceConfig{Enabled: false},
	}}
	if channel, stop := d.startDeepSeekBalanceTicker(); channel != nil || stop != nil {
		t.Error("a disabled deepseek_balance started its ticker")
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		DeepSeekBalance: &agentconfig.DeepSeekBalanceConfig{Enabled: true},
	}}
	channel, stop := d.startDeepSeekBalanceTicker()
	if channel == nil || stop == nil {
		t.Fatal("an enabled deepseek_balance started no ticker")
	}
	if len(intervals) != 1 || intervals[0] != 10*time.Minute {
		t.Errorf("the ticker asks for an interval of %v, want the 10m default", intervals)
	}
	stop()
	if stops != 1 {
		t.Errorf("the stop func ran %d times, want the ticker stopped once", stops)
	}

	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		DeepSeekBalance: &agentconfig.DeepSeekBalanceConfig{Enabled: true, Interval: "45m"},
	}}
	channel, stop = d.startDeepSeekBalanceTicker()
	if channel == nil || stop == nil {
		t.Fatal("an enabled deepseek_balance with an interval started no ticker")
	}
	if len(intervals) != 2 || intervals[1] != 45*time.Minute {
		t.Errorf("the ticker asks for an interval of %v, want the configured 45m", intervals)
	}
	stop()

	// An interval under the floor starts no ticker rather than one the town did
	// not ask for, and says so once.
	var logged bytes.Buffer
	d.logger = log.New(&logged, "", 0)
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		DeepSeekBalance: &agentconfig.DeepSeekBalanceConfig{Enabled: true, Interval: "20s"},
	}}
	if channel, stop := d.startDeepSeekBalanceTicker(); channel != nil || stop != nil {
		t.Error("an interval under the floor started a ticker")
	}
	if len(intervals) != 2 {
		t.Errorf("an interval under the floor asked for a ticker at %v, want none", intervals[2:])
	}
	if !strings.Contains(logged.String(), "patrols.deepseek_balance.interval") {
		t.Errorf("an interval under the floor logged %q, want the interval key named", logged.String())
	}

	// The production ticker, with no seam: the patrol still gets a channel and
	// a stop func, so Run's defer has a real ticker to stop.
	plain := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			DeepSeekBalance: &agentconfig.DeepSeekBalanceConfig{Enabled: true},
		}},
	}
	channel, stop = plain.startDeepSeekBalanceTicker()
	if channel == nil || stop == nil {
		t.Fatal("the patrol started no ticker without a ticker seam")
	}
	stop()
}

// TestDeepSeekPatrolUnconfiguredLogsOnceAndFetchesNothing: a town that mounts
// no key gets one line over two ticks and no fetch, rather than one line per
// interval.
func TestDeepSeekPatrolUnconfiguredLogsOnceAndFetchesNothing(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, keyFile)
	fetches := &deepSeekBalanceFetches{}
	d.newDeepSeekBalanceSeams = fetches.seams

	d.triggerDeepSeekBalance()
	d.triggerDeepSeekBalance()

	if got := len(fetches.keys); got != 0 {
		t.Errorf("an unconfigured town fetched %d times, want none", got)
	}
	lines := 0
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "deepseek_balance: unconfigured:") {
			lines++
			if !strings.Contains(line, keyFile) {
				t.Errorf("the unconfigured line %q does not name the key file %s", line, keyFile)
			}
		}
	}
	if lines != 1 {
		t.Errorf("two ticks over no key file logged %d unconfigured lines, want one (log: %q)", lines, logged.String())
	}
}

// TestDeepSeekPatrolUnconfiguredLogsOnceAgainAfterTheKeyReturns: a key that
// goes away after a good read is one more unconfigured line, not silence.
func TestDeepSeekPatrolUnconfiguredLogsOnceAgainAfterTheKeyReturns(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, keyFile)
	fetches := &deepSeekBalanceFetches{reading: deepseekbalance.Sample{Total: 25}}
	d.newDeepSeekBalanceSeams = fetches.seams

	d.triggerDeepSeekBalance()
	if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY=sk-returned\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	d.triggerDeepSeekBalance()
	if err := os.Remove(keyFile); err != nil {
		t.Fatalf("removing the key file: %v", err)
	}
	d.triggerDeepSeekBalance()

	if got := len(fetches.keys); got != 1 {
		t.Errorf("the ticks fetched %d times, want the one configured tick", got)
	}
	if got := strings.Count(logged.String(), "deepseek_balance: unconfigured:"); got != 2 {
		t.Errorf("the key came and went over %d unconfigured lines, want one per absence (2): %q", got, logged.String())
	}
}

// TestDeepSeekPatrolKeyNeverLogged: the key reaches the fetch and no log line,
// whether the fetch succeeds or fails.
func TestDeepSeekPatrolKeyNeverLogged(t *testing.T) {
	t.Parallel()
	const key = "sk-very-secret-value-9d2f"
	cases := []struct {
		name     string
		fetchErr error
	}{
		{"a good fetch", nil},
		{"a failed fetch", errors.New("the balance endpoint answered HTTP 500")},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			keyFile := filepath.Join(t.TempDir(), "deepseek.env")
			if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY="+key+"\n"), 0o600); err != nil {
				t.Fatalf("writing the key file: %v", err)
			}
			var logged bytes.Buffer
			d := deepSeekBalanceTestDaemon(t, &logged, keyFile)
			fetches := &deepSeekBalanceFetches{reading: deepseekbalance.Sample{Total: 25}, err: tt.fetchErr}
			d.newDeepSeekBalanceSeams = fetches.seams

			d.triggerDeepSeekBalance()

			if len(fetches.keys) != 1 || fetches.keys[0] != key {
				t.Fatalf("the tick fetched with %q, want the key the file sets", fetches.keys)
			}
			if strings.Contains(logged.String(), key) {
				t.Errorf("the key appears in the log: %q", logged.String())
			}
			if tt.fetchErr != nil && !strings.Contains(logged.String(), tt.fetchErr.Error()) {
				t.Errorf("a failed fetch logged %q, want the failure named", logged.String())
			}
		})
	}
}

// TestDeepSeekPatrolKeyFile: the production reader's branches. An absent file
// is the unconfigured town; a file that sets no key, or cannot be read, is an
// error rather than a town seen as unconfigured.
func TestDeepSeekPatrolKeyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return path
	}
	cases := []struct {
		name           string
		path           string
		wantKey        string
		wantConfigured bool
		wantErr        string
	}{
		{"an absent file", filepath.Join(dir, "absent.env"), "", false, ""},
		{"a bare assignment", file("bare.env", "DEEPSEEK_API_KEY=sk-bare\n"), "sk-bare", true, ""},
		{"a quoted assignment", file("quoted.env", "DEEPSEEK_API_KEY=\"sk-quoted\"\n"), "sk-quoted", true, ""},
		{"an exported assignment", file("export.env", "export DEEPSEEK_API_KEY=sk-export\n"), "sk-export", true, ""},
		{"comments and blanks", file("comments.env", "# the key\n\nDEEPSEEK_API_KEY=sk-comment\n"), "sk-comment", true, ""},
		{"the last assignment", file("twice.env", "DEEPSEEK_API_KEY=sk-first\nDEEPSEEK_API_KEY=sk-second\n"), "sk-second", true, ""},
		{"an empty value", file("empty.env", "DEEPSEEK_API_KEY=\n"), "", false, "sets no DEEPSEEK_API_KEY"},
		{"another key only", file("other.env", "OTHER=1\n"), "", false, "sets no DEEPSEEK_API_KEY"},
		{"no equals sign", file("bare-name.env", "DEEPSEEK_API_KEY\n"), "", false, "sets no DEEPSEEK_API_KEY"},
		{"a directory", dir, "", false, "is a directory"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			key, configured, err := deepSeekBalanceReadKey(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("readKey(%s) error = %v, want one naming %q", tt.path, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readKey(%s): %v", tt.path, err)
			}
			if key != tt.wantKey || configured != tt.wantConfigured {
				t.Errorf("readKey(%s) = (%q, %v), want (%q, %v)", tt.path, key, configured, tt.wantKey, tt.wantConfigured)
			}
		})
	}
}

// TestDeepSeekPatrolKeyFileErrorIsNotUnconfigured: a key file that cannot be
// read is a failed tick, never a town seen as unconfigured, so a broken mount
// is an error rather than one benign line.
func TestDeepSeekPatrolKeyFileErrorIsNotUnconfigured(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, t.TempDir())
	fetches := &deepSeekBalanceFetches{}
	d.newDeepSeekBalanceSeams = fetches.seams

	d.triggerDeepSeekBalance()

	if len(fetches.keys) != 0 {
		t.Errorf("a tick that could not read the key file fetched %d times, want none", len(fetches.keys))
	}
	if strings.Contains(logged.String(), "deepseek_balance: unconfigured:") {
		t.Errorf("an unreadable key file logged the unconfigured line: %q", logged.String())
	}
	if !strings.Contains(logged.String(), "reading the key file") {
		t.Errorf("an unreadable key file logged %q, want the read failure named", logged.String())
	}
}

// TestDeepSeekPatrolRelativeKeyFileRefuses: a key_file the loader never saw,
// because the config was built in code, is judged before it reaches the file
// read, since a relative path resolves against the daemon's working directory.
func TestDeepSeekPatrolRelativeKeyFileRefuses(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, "deepseek.env")
	fetches := &deepSeekBalanceFetches{}
	d.newDeepSeekBalanceSeams = fetches.seams

	d.triggerDeepSeekBalance()

	if !strings.Contains(logged.String(), "want an absolute path") {
		t.Errorf("a relative key file logged %q, want the refusal named", logged.String())
	}
	if len(fetches.keys) != 0 {
		t.Errorf("a relative key file fetched %d times, want none", len(fetches.keys))
	}
}

// TestDeepSeekPatrolConfiguredWithoutAFetchRefuses: a tick whose seams carry no
// fetch holds a key and says so rather than passing as a clean read.
func TestDeepSeekPatrolConfiguredWithoutAFetchRefuses(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY=sk-nowhere-to-go\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, keyFile)
	d.newDeepSeekBalanceSeams = func() deepSeekBalanceTickSeams {
		return deepSeekBalanceTickSeams{readKey: deepSeekBalanceReadKey, evaluate: deepseekbalance.Evaluate}
	}

	d.triggerDeepSeekBalance()

	if !strings.Contains(logged.String(), "wires no fetch") {
		t.Errorf("a configured tick with no fetch logged %q, want the refusal named", logged.String())
	}
	if strings.Contains(logged.String(), "sk-nowhere-to-go") {
		t.Errorf("the refusal logged the key: %q", logged.String())
	}
}

// balanceTransport answers one http.Client's requests in process, so a test
// fakes the balance endpoint without a socket.
type balanceTransport func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f balanceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeDeepSeekBalance is the balance endpoint as an in-process client: request n
// (from 1) gets the status and body answer returns. A request that is not GET
// /user/balance with the bearer token key is refused, so a tick that fetched
// with the wrong key reads nothing.
func fakeDeepSeekBalance(key string, answer func(n int) (int, string)) *http.Client {
	var requests atomic.Int64
	return &http.Client{Transport: balanceTransport(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusUnauthorized, "unexpected request"
		if r.Method == http.MethodGet && r.URL.Path == "/user/balance" && r.Header.Get("Authorization") == "Bearer "+key {
			status, body = answer(int(requests.Add(1)))
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
}

// deepSeekBalanceBody is the GET /user/balance answer for one reading.
func deepSeekBalanceBody(total, granted, toppedUp string) string {
	return `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"` + total +
		`","granted_balance":"` + granted + `","topped_up_balance":"` + toppedUp + `"}]}`
}

// sameReading reports whether the reading p holds is want.
func sameReading(p *float64, want float64) bool {
	return p != nil && *p == want
}

// TestDeepSeekPatrolTwoTicksWriteTwoRows: two ticks over the balance endpoint
// write two deepseek_balance rows, read back newest first with each reading's
// fields and the instant the daemon's clock gave it.
func TestDeepSeekPatrolTwoTicksWriteTwoRows(t *testing.T) {
	t.Parallel()
	const key = "sk-two-ticks-7c1e"
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY="+key+"\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	client := fakeDeepSeekBalance(key, func(n int) (int, string) {
		if n == 1 {
			return http.StatusOK, deepSeekBalanceBody("10.00", "4.00", "6.00")
		}
		return http.StatusOK, deepSeekBalanceBody("8.50", "3.50", "5.00")
	})
	first := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	clk := clockwork.NewFakeClockAt(first)
	store := beadsfake.New(beadsfake.WithClock(clk))
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, keyFile)
	d.clock = clk
	d.deepSeekBalanceHTTP = client
	d.openCostSampleBeads = func() beads.Client { return store }

	d.triggerDeepSeekBalance()
	clk.Advance(10 * time.Minute)
	d.triggerDeepSeekBalance()

	rows, err := store.ReadCostSamples(context.Background(), beads.SampleKindDeepSeekBalance, time.Time{}, 0)
	if err != nil {
		t.Fatalf("reading the cost samples: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("two ticks wrote %d rows, want 2 (log: %q)", len(rows), logged.String())
	}
	want := []struct {
		at                       time.Time
		total, granted, toppedUp float64
	}{
		{first.Add(10 * time.Minute), 8.50, 3.50, 5.00},
		{first, 10.00, 4.00, 6.00},
	}
	for i, w := range want {
		r := rows[i]
		if r.Kind != beads.SampleKindDeepSeekBalance || !r.At.Equal(w.at) {
			t.Errorf("row %d is kind %q at %v, want deepseek_balance at %v", i, r.Kind, r.At, w.at)
		}
		if !sameReading(r.Total, w.total) || !sameReading(r.Granted, w.granted) || !sameReading(r.ToppedUp, w.toppedUp) {
			t.Errorf("row %d reads total %v, granted %v, topped up %v; want %v, %v, %v",
				i, deref(r.Total), deref(r.Granted), deref(r.ToppedUp), w.total, w.granted, w.toppedUp)
		}
	}
	if strings.Contains(logged.String(), key) {
		t.Errorf("the key appears in the log: %q", logged.String())
	}
}

// TestDeepSeekPatrolFailedFetchWritesNoRow: a balance endpoint that answers an
// error writes no row, and the tick logs the failure.
func TestDeepSeekPatrolFailedFetchWritesNoRow(t *testing.T) {
	t.Parallel()
	const key = "sk-failed-fetch-4a9b"
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY="+key+"\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	client := fakeDeepSeekBalance(key, func(int) (int, string) {
		return http.StatusInternalServerError, "upstream down"
	})
	store := beadsfake.New()
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, keyFile)
	d.deepSeekBalanceHTTP = client
	d.openCostSampleBeads = func() beads.Client { return store }

	d.triggerDeepSeekBalance()

	rows, err := store.ReadCostSamples(context.Background(), beads.SampleKindDeepSeekBalance, time.Time{}, 0)
	if err != nil {
		t.Fatalf("reading the cost samples: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a failed fetch wrote %d rows, want none", len(rows))
	}
	if !strings.Contains(logged.String(), "HTTP 500") {
		t.Errorf("a failed fetch logged %q, want the failed status named", logged.String())
	}
	if strings.Contains(logged.String(), key) {
		t.Errorf("the key appears in the log: %q", logged.String())
	}
}

// TestDeepSeekPatrolFailedWriteIsLogged: a balance read that the store refuses
// to record is logged as a failed tick, not passed as a written row.
func TestDeepSeekPatrolFailedWriteIsLogged(t *testing.T) {
	t.Parallel()
	const key = "sk-failed-write-b08e"
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY="+key+"\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	client := fakeDeepSeekBalance(key, func(int) (int, string) {
		return http.StatusOK, deepSeekBalanceBody("10.00", "4.00", "6.00")
	})
	store := beadsfake.New()
	store.FailWith("sample add", errors.New("store refused the row"))
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, keyFile)
	d.deepSeekBalanceHTTP = client
	d.openCostSampleBeads = func() beads.Client { return store }

	d.triggerDeepSeekBalance()

	if !strings.Contains(logged.String(), "writing the deepseek_balance row") {
		t.Errorf("a refused row logged %q, want the failed write named", logged.String())
	}
	if strings.Contains(logged.String(), key) {
		t.Errorf("the key appears in the log: %q", logged.String())
	}
}

// deepSeekBalanceAlertDaemon is a daemon whose balance endpoint answers the
// readings in order, one per good tick, and whose rows, escalations and clears
// are recorded in memory.
func deepSeekBalanceAlertDaemon(t *testing.T, logged io.Writer, key string, readings ...string) (*Daemon, *beadsfake.Fake, *notifyfake.Recorder, *clockwork.FakeClock) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY="+key+"\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	client := fakeDeepSeekBalance(key, func(n int) (int, string) {
		if n > len(readings) {
			return http.StatusInternalServerError, "the test asked for more readings than it listed"
		}
		return http.StatusOK, deepSeekBalanceBody(readings[n-1], "0.00", readings[n-1])
	})
	clk := clockwork.NewFakeClockAt(time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC))
	store := beadsfake.New(beadsfake.WithClock(clk))
	rec := notifyfake.New()
	d := deepSeekBalanceTestDaemon(t, logged, keyFile)
	d.clock = clk
	d.notifier = rec
	d.deepSeekBalanceHTTP = client
	d.openCostSampleBeads = func() beads.Client { return store }
	return d, store, rec, clk
}

// assertDeepSeekBalanceKeyAbsent fails when the API key appears in the run's
// log, in a notify call, or in a row: the key reaches the request header and
// nowhere else.
func assertDeepSeekBalanceKeyAbsent(t *testing.T, key, logText string, rec *notifyfake.Recorder, rows []beads.Sample) {
	t.Helper()
	if strings.Contains(logText, key) {
		t.Errorf("the key appears in the log: %q", logText)
	}
	for _, c := range rec.Calls() {
		if strings.Contains(fmt.Sprintf("%+v", c), key) {
			t.Errorf("the key appears in a notify call: %+v", c)
		}
	}
	for _, r := range rows {
		written := fmt.Sprintf("%v %v %v %q", deref(r.Total), deref(r.Granted), deref(r.ToppedUp), r.Note)
		if strings.Contains(written, key) {
			t.Errorf("the key appears in the row: %s", written)
		}
	}
}

// TestDeepSeekPatrolAlertKeyMap: each line Evaluate raises is matched to its own
// stable key, pinned against the package's own text so a reworded alert fails
// here rather than losing its escalation.
func TestDeepSeekPatrolAlertKeyMap(t *testing.T) {
	t.Parallel()
	spent := deepseekbalance.Sample{Total: 25.00}
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"a spend jump", deepseekbalance.Evaluate(&spent, deepseekbalance.Sample{Total: 22.00}, 0, 10*time.Minute), alertKeyDeepSeekSpendJump},
		{"a low balance", deepseekbalance.Evaluate(nil, deepseekbalance.Sample{Total: 15.00}, 0, 10*time.Minute), alertKeyDeepSeekLowBalance},
		{"an unreachable endpoint", deepseekbalance.Evaluate(nil, deepseekbalance.Sample{}, 3, 10*time.Minute), alertKeyDeepSeekUnreachable},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if len(tt.lines) != 1 {
				t.Fatalf("Evaluate raised %q, want the one line for this condition alone", tt.lines)
			}
			key, ok := deepSeekBalanceAlertKey(tt.lines[0])
			if !ok || key != tt.want {
				t.Errorf("deepSeekBalanceAlertKey(%q) = %q, %v; want %q", tt.lines[0], key, ok, tt.want)
			}
		})
	}
}

// TestDeepSeekPatrolAlertsOneEscalationEachAndTheRowCarriesThem: a spend jump
// and a low balance each file one escalation, a repeat of either counts an
// occurrence on that same escalation rather than filing another, the row's note
// carries the alert text, a quiet reading closes the key it ends, and the key
// reaches none of it.
func TestDeepSeekPatrolAlertsOneEscalationEachAndTheRowCarriesThem(t *testing.T) {
	t.Parallel()
	const key = "sk-alerts-5e11"
	var logged bytes.Buffer
	d, store, rec, clk := deepSeekBalanceAlertDaemon(t, &logged, key, "25.00", "22.00", "17.00", "17.50")

	d.triggerDeepSeekBalance()
	clk.Advance(10 * time.Minute)
	// $3.00 spent in ten minutes, over the $1.00 that alerts.
	d.triggerDeepSeekBalance()
	clk.Advance(10 * time.Minute)
	// $5.00 more spent, and $17.00 is under the $20.00 low-balance floor.
	d.triggerDeepSeekBalance()

	spend := rec.OpenEscalations(alertKeyDeepSeekSpendJump)
	if len(spend) != 1 || spend[0].Occurrences != 2 {
		t.Fatalf("the spend jump is open as %+v, want one escalation with 2 occurrences", spend)
	}
	if want := "DeepSeek spent $5.00 in the last 10 min, a pace of $30.00/h"; spend[0].Reason != want {
		t.Errorf("the spend escalation's reason = %q, want %q", spend[0].Reason, want)
	}
	if !strings.HasPrefix(spend[0].Title, "deepseek_balance: DeepSeek spent $") {
		t.Errorf("the spend escalation is titled %q, want the source and the alert line", spend[0].Title)
	}
	low := rec.OpenEscalations(alertKeyDeepSeekLowBalance)
	if len(low) != 1 || low[0].Occurrences != 1 {
		t.Fatalf("the low balance is open as %+v, want one escalation", low)
	}
	if want := "DeepSeek balance is $17.00, below $20.00"; low[0].Reason != want {
		t.Errorf("the low-balance escalation's reason = %q, want %q", low[0].Reason, want)
	}
	// The dedupe is the fingerprint: each firing carries the key of its
	// condition, never the line, or a repeat would be recorded on a new bead.
	for _, c := range rec.Escalations() {
		if fp := c.Escalation.Fingerprint; fp != alertKeyDeepSeekSpendJump && fp != alertKeyDeepSeekLowBalance {
			t.Errorf("an escalation is filed under %q, want one of the patrol's stable keys", fp)
		}
	}

	rows, err := store.ReadCostSamples(context.Background(), beads.SampleKindDeepSeekBalance, time.Time{}, 0)
	if err != nil {
		t.Fatalf("reading the cost samples: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("three ticks wrote %d rows, want 3 (log: %q)", len(rows), logged.String())
	}
	wantNotes := []string{
		"DeepSeek spent $5.00 in the last 10 min, a pace of $30.00/h\nDeepSeek balance is $17.00, below $20.00",
		"DeepSeek spent $3.00 in the last 10 min, a pace of $18.00/h",
		"",
	}
	for i, want := range wantNotes {
		if rows[i].Note != want {
			t.Errorf("row %d notes %q, want %q", i, rows[i].Note, want)
		}
	}

	// A quiet interval under the floor: the spend key ends, the low balance is
	// one occurrence more.
	clk.Advance(10 * time.Minute)
	d.triggerDeepSeekBalance()
	if open := rec.OpenEscalations(alertKeyDeepSeekSpendJump); len(open) != 0 {
		t.Errorf("the spend jump is open as %+v after a quiet interval, want it cleared", open)
	}
	if open := rec.OpenEscalations(alertKeyDeepSeekLowBalance); len(open) != 1 || open[0].Occurrences != 2 {
		t.Errorf("the low balance is open as %+v, want one escalation with 2 occurrences", open)
	}
	clears := rec.Clears()
	if len(clears) == 0 {
		t.Fatal("a quiet interval recorded no clear")
	}
	last := clears[len(clears)-1]
	if !slices.Contains(last.Fingerprints, alertKeyDeepSeekSpendJump) {
		t.Errorf("the quiet interval cleared %v, want the spend key it ended closed", last.Fingerprints)
	}
	if slices.Contains(last.Fingerprints, alertKeyDeepSeekLowBalance) {
		t.Errorf("the quiet interval cleared %v, including the low-balance key it raised", last.Fingerprints)
	}
	assertDeepSeekBalanceKeyAbsent(t, key, logged.String(), rec, rows)
}

// TestDeepSeekPatrolUnreachableAlertsOnTheThirdFailureAndClearsOnRecovery: the
// unreachable alert files on the third consecutive failed fetch, stays the one
// open escalation while the failures continue, and closes when the endpoint
// answers.
func TestDeepSeekPatrolUnreachableAlertsOnTheThirdFailureAndClearsOnRecovery(t *testing.T) {
	t.Parallel()
	const key = "sk-unreachable-77a0"
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY="+key+"\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	var down bool
	client := fakeDeepSeekBalance(key, func(n int) (int, string) {
		if down {
			return http.StatusInternalServerError, "upstream down"
		}
		if n == 1 {
			return http.StatusOK, deepSeekBalanceBody("25.00", "0.00", "25.00")
		}
		return http.StatusOK, deepSeekBalanceBody("22.00", "0.00", "22.00")
	})
	clk := clockwork.NewFakeClockAt(time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC))
	store := beadsfake.New(beadsfake.WithClock(clk))
	rec := notifyfake.New()
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, keyFile)
	d.clock = clk
	d.notifier = rec
	d.deepSeekBalanceHTTP = client
	d.openCostSampleBeads = func() beads.Client { return store }

	d.triggerDeepSeekBalance() // a good reading, no previous one to alert against
	down = true
	for i := 0; i < 2; i++ {
		clk.Advance(10 * time.Minute)
		d.triggerDeepSeekBalance()
	}
	if got := rec.Escalations(); len(got) != 0 {
		t.Errorf("two failed fetches filed %+v, want nothing before the third", got)
	}

	clk.Advance(10 * time.Minute)
	d.triggerDeepSeekBalance()
	open := rec.OpenEscalations(alertKeyDeepSeekUnreachable)
	if len(open) != 1 || open[0].Occurrences != 1 {
		t.Fatalf("the third failed fetch left %+v, want one unreachable escalation", open)
	}
	if want := "DeepSeek balance endpoint unreachable for 3 consecutive checks"; open[0].Reason != want {
		t.Errorf("the unreachable escalation's reason = %q, want %q", open[0].Reason, want)
	}

	clk.Advance(10 * time.Minute)
	d.triggerDeepSeekBalance()
	if open := rec.OpenEscalations(alertKeyDeepSeekUnreachable); len(open) != 1 || open[0].Occurrences != 1 {
		t.Fatalf("the fourth failed fetch left %+v, want the one escalation still open", open)
	}

	down = false
	clk.Advance(10 * time.Minute)
	d.triggerDeepSeekBalance()
	if open := rec.OpenEscalations(alertKeyDeepSeekUnreachable); len(open) != 0 {
		t.Errorf("the endpoint answered but the unreachable escalation is open as %+v, want it cleared", open)
	}

	rows, err := store.ReadCostSamples(context.Background(), beads.SampleKindDeepSeekBalance, time.Time{}, 0)
	if err != nil {
		t.Fatalf("reading the cost samples: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("the two good readings wrote %d rows, want 2 (log: %q)", len(rows), logged.String())
	}
	// The spend is spread over the fifty minutes between the two readings, not
	// over the ten-minute cadence the outage interrupted.
	wantLast := "DeepSeek spent $3.00 in the last 50 min, a pace of $3.60/h"
	if len(rows) == 2 && rows[0].Note != wantLast {
		t.Errorf("the recovery row notes %q, want %q", rows[0].Note, wantLast)
	}
	assertDeepSeekBalanceKeyAbsent(t, key, logged.String(), rec, rows)
}

// TestDeepSeekPatrolUnknownAlertLineIsReported: a line no key names is reported
// rather than filed under a key derived from the line, and the reading is still
// written with the line in its note.
func TestDeepSeekPatrolUnknownAlertLineIsReported(t *testing.T) {
	t.Parallel()
	const key = "sk-unknown-line-31d0"
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY="+key+"\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	const line = "DeepSeek balance went sideways"
	store := beadsfake.New()
	rec := notifyfake.New()
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, keyFile)
	d.notifier = rec
	d.openCostSampleBeads = func() beads.Client { return store }
	d.newDeepSeekBalanceSeams = func() deepSeekBalanceTickSeams {
		return deepSeekBalanceTickSeams{
			readKey: deepSeekBalanceReadKey,
			fetch: func(context.Context, string) (deepseekbalance.Sample, error) {
				return deepseekbalance.Sample{Total: 25}, nil
			},
			evaluate: func(*deepseekbalance.Sample, deepseekbalance.Sample, int, time.Duration) []string {
				return []string{line}
			},
		}
	}

	d.triggerDeepSeekBalance()

	if !strings.Contains(logged.String(), "no alert key names the balance alert") {
		t.Errorf("an unknown alert line logged %q, want the refusal named", logged.String())
	}
	if got := rec.Escalations(); len(got) != 0 {
		t.Errorf("an unknown alert line filed %+v, want nothing filed", got)
	}
	rows, err := store.ReadCostSamples(context.Background(), beads.SampleKindDeepSeekBalance, time.Time{}, 0)
	if err != nil {
		t.Fatalf("reading the cost samples: %v", err)
	}
	if len(rows) != 1 || rows[0].Note != line {
		t.Errorf("the reading's rows = %+v, want one row noting %q", rows, line)
	}
}

// TestDeepSeekPatrolNoEvaluatorRefuses: a tick handed no evaluator refuses
// before reading, rather than writing rows whose alerts nothing judged.
func TestDeepSeekPatrolNoEvaluatorRefuses(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY=sk-no-evaluator\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, keyFile)
	fetches := &deepSeekBalanceFetches{}
	d.newDeepSeekBalanceSeams = func() deepSeekBalanceTickSeams {
		return deepSeekBalanceTickSeams{readKey: deepSeekBalanceReadKey, fetch: fetches.seams().fetch}
	}

	d.triggerDeepSeekBalance()

	if !strings.Contains(logged.String(), "no reading evaluator") {
		t.Errorf("a tick with no evaluator logged %q, want the missing seam named", logged.String())
	}
	if len(fetches.keys) != 0 {
		t.Errorf("a tick with no evaluator fetched %d times, want none", len(fetches.keys))
	}
}

// TestDeepSeekPatrolRefusedIntervalFetchesNothing: a cadence the config refuses
// (here a block built in code, which the loader never judged) stops the tick
// before the fetch, since Evaluate has no interval to spread the spend over.
func TestDeepSeekPatrolRefusedIntervalFetchesNothing(t *testing.T) {
	t.Parallel()
	const key = "sk-refused-interval-8c2f"
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY="+key+"\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, keyFile)
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		DeepSeekBalance: &agentconfig.DeepSeekBalanceConfig{Enabled: true, KeyFile: keyFile, Interval: "20s"},
	}}
	fetches := &deepSeekBalanceFetches{}
	d.newDeepSeekBalanceSeams = fetches.seams

	d.triggerDeepSeekBalance()

	if !strings.Contains(logged.String(), "patrols.deepseek_balance.interval") {
		t.Errorf("a refused interval logged %q, want the interval key named", logged.String())
	}
	if len(fetches.keys) != 0 {
		t.Errorf("a refused interval fetched %d times, want none", len(fetches.keys))
	}
}

// deref returns the reading p holds, or "unset" when it holds none.
func deref(p *float64) any {
	if p == nil {
		return "unset"
	}
	return *p
}

// deepSeekBalanceBlockingDaemon is a daemon whose tick waits on release before
// the key read answers, so a test can hold a tick open.
func deepSeekBalanceBlockingDaemon(t *testing.T, logged io.Writer, release <-chan struct{}, reached chan<- struct{}) *Daemon {
	t.Helper()
	d := deepSeekBalanceTestDaemon(t, logged, filepath.Join(t.TempDir(), "deepseek.env"))
	d.ctx = context.Background()
	d.newDeepSeekBalanceSeams = func() deepSeekBalanceTickSeams {
		return deepSeekBalanceTickSeams{
			readKey: func(string) (string, bool, error) {
				reached <- struct{}{}
				<-release
				return "", false, nil
			},
			evaluate: deepseekbalance.Evaluate,
		}
	}
	return d
}

// TestDeepSeekPatrolTickSingleFlight: a tick that arrives while one runs is
// skipped and logged, so two ticks never read the account at once.
func TestDeepSeekPatrolTickSingleFlight(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	reached := make(chan struct{}, 1)
	release := make(chan struct{})
	d := deepSeekBalanceBlockingDaemon(t, &logged, release, reached)

	d.startDeepSeekBalanceTick()
	waitForSignal(t, reached, "the first tick never reached the key read")
	d.startDeepSeekBalanceTick()
	if !strings.Contains(logged.String(), "previous tick still running, skipping") {
		t.Errorf("the second tick was not logged as skipped: %q", logged.String())
	}
	close(release)
	d.deepSeekBalanceCycles.Wait()
	if d.deepSeekBalanceRunning.Load() {
		t.Error("the guard stayed set after the tick ended, so no later tick could run")
	}
}

// TestDeepSeekPatrolStopJoinsTheRunningTick: Run's return stops the ticker and
// waits for the tick it started, and a tick still running at the bound is
// logged and left.
func TestDeepSeekPatrolStopJoinsTheRunningTick(t *testing.T) {
	t.Parallel()
	t.Run("the tick the ticker started is joined", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		reached := make(chan struct{}, 1)
		release := make(chan struct{})
		d := deepSeekBalanceBlockingDaemon(t, &logged, release, reached)
		d.deepSeekBalanceJoinWait = time.Hour

		d.startDeepSeekBalanceTick()
		waitForSignal(t, reached, "the tick never reached the key read")

		stopped := make(chan struct{})
		stopCalled := make(chan struct{})
		go func() {
			d.stopDeepSeekBalancePatrol(func() { close(stopCalled) })
			close(stopped)
		}()
		waitForSignal(t, stopCalled, "the stop func never ran")
		select {
		case <-stopped:
			t.Fatal("stopDeepSeekBalancePatrol returned while the tick still ran")
		default:
		}
		close(release)
		waitForSignal(t, stopped, "stopDeepSeekBalancePatrol never returned after the tick finished")
		if d.deepSeekBalanceRunning.Load() {
			t.Error("the join returned with the tick's guard still set")
		}
	})

	t.Run("a tick past the bound is logged and left", func(t *testing.T) {
		t.Parallel()
		var logged bytes.Buffer
		reached := make(chan struct{}, 1)
		release := make(chan struct{})
		d := deepSeekBalanceBlockingDaemon(t, &logged, release, reached)
		d.deepSeekBalanceJoinWait = 30 * time.Millisecond

		d.startDeepSeekBalanceTick()
		waitForSignal(t, reached, "the tick never reached the key read")

		stopped := make(chan struct{})
		go func() {
			d.stopDeepSeekBalancePatrol(nil)
			close(stopped)
		}()
		waitForSignal(t, stopped, "stopDeepSeekBalancePatrol never returned past the bound")
		if !strings.Contains(logged.String(), "still running") {
			t.Errorf("a tick left past the bound was not logged: %q", logged.String())
		}
		close(release)
		d.deepSeekBalanceCycles.Wait()
	})
}

// TestDeepSeekPatrolNoKeyReaderRefuses: a tick handed no key reader refuses
// before reading, rather than treating the town as unconfigured.
func TestDeepSeekPatrolNoKeyReaderRefuses(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, filepath.Join(t.TempDir(), "deepseek.env"))
	fetches := &deepSeekBalanceFetches{}
	d.newDeepSeekBalanceSeams = func() deepSeekBalanceTickSeams {
		return deepSeekBalanceTickSeams{fetch: fetches.seams().fetch}
	}

	d.triggerDeepSeekBalance()

	if !strings.Contains(logged.String(), "no key reader") {
		t.Errorf("a tick with no key reader logged %q, want the missing seam named", logged.String())
	}
	if strings.Contains(logged.String(), "deepseek_balance: unconfigured:") {
		t.Errorf("a tick with no key reader logged the unconfigured line: %q", logged.String())
	}
	if len(fetches.keys) != 0 {
		t.Errorf("a tick with no key reader fetched %d times, want none", len(fetches.keys))
	}
}

// TestDeepSeekPatrolConfigDecode: the block's defaults and the values the
// strict decoder refuses rather than reading as the default. The decoder is the
// config kernel's, so this drives the daemon's own reading of a decoded block.
func TestDeepSeekPatrolConfigDecode(t *testing.T) {
	t.Parallel()
	block := func(body string) string {
		return `{"patrols":{"deepseek_balance":` + body + `}}`
	}
	cases := []struct {
		name        string
		document    string
		wantEvery   time.Duration
		wantKeyFile string
		wantErr     string
	}{
		{"no block", `{}`, 10 * time.Minute, agentconfig.DeepSeekBalanceDefaultKeyFile, ""},
		{"enabled only", block(`{"enabled":true}`), 10 * time.Minute, agentconfig.DeepSeekBalanceDefaultKeyFile, ""},
		{"a configured interval", block(`{"enabled":true,"interval":"45m"}`), 45 * time.Minute, agentconfig.DeepSeekBalanceDefaultKeyFile, ""},
		{"the floor itself", block(`{"enabled":true,"interval":"1m"}`), time.Minute, agentconfig.DeepSeekBalanceDefaultKeyFile, ""},
		{"a configured key file", block(`{"enabled":true,"key_file":"/run/k/deepseek.env"}`), 10 * time.Minute, "/run/k/deepseek.env", ""},
		{"an empty key file", block(`{"enabled":true,"key_file":""}`), 10 * time.Minute, agentconfig.DeepSeekBalanceDefaultKeyFile, ""},
		{"an interval under the floor", block(`{"enabled":true,"interval":"20s"}`), 0, "", "patrols.deepseek_balance.interval"},
		{"an unknown key", block(`{"enabled":true,"fetch_every":"1m"}`), 0, "", "patrols.deepseek_balance"},
		{"a relative key file", block(`{"enabled":true,"key_file":"deepseek.env"}`), 0, "", "want an absolute path"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var cfg DaemonPatrolConfig
			err := json.Unmarshal([]byte(tt.document), &cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("decoding %s error = %v, want one naming %q", tt.document, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decoding %s: %v", tt.document, err)
			}
			every, err := deepSeekBalanceInterval(&cfg)
			if err != nil {
				t.Fatalf("deepSeekBalanceInterval: %v", err)
			}
			if every != tt.wantEvery {
				t.Errorf("the interval is %v, want %v", every, tt.wantEvery)
			}
			got, err := deepSeekBalanceKeyFile(&cfg)
			if err != nil {
				t.Fatalf("deepSeekBalanceKeyFile: %v", err)
			}
			if got != tt.wantKeyFile {
				t.Errorf("the key file is %q, want %q", got, tt.wantKeyFile)
			}
		})
	}
}
