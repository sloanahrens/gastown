package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// deepSeekBalanceTestDaemon builds a daemon whose patrol ticks inline, over the
// key file the test names.
func deepSeekBalanceTestDaemon(t *testing.T, logged io.Writer, keyFile string) *Daemon {
	t.Helper()
	return &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logged, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			DeepSeekBalance: &agentconfig.DeepSeekBalanceConfig{Enabled: true, KeyFile: keyFile},
		}},
	}
}

// deepSeekBalanceFetches is the tick's fetch seam: the keys it was handed and
// the error it answers.
type deepSeekBalanceFetches struct {
	keys []string
	err  error
}

// seams is the tick's seams over the production key reader and this recorder.
func (f *deepSeekBalanceFetches) seams() deepSeekBalanceTickSeams {
	return deepSeekBalanceTickSeams{
		readKey: deepSeekBalanceReadKey,
		fetch: func(_ context.Context, key string) error {
			f.keys = append(f.keys, key)
			return f.err
		},
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
	fetches := &deepSeekBalanceFetches{}
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
			fetches := &deepSeekBalanceFetches{err: tt.fetchErr}
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

// TestDeepSeekPatrolConfiguredWithoutAFetchRefuses: production wires no fetch
// until gt-a5c8ld, and a tick holding a key says so rather than passing as a
// clean read.
func TestDeepSeekPatrolConfiguredWithoutAFetchRefuses(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "deepseek.env")
	if err := os.WriteFile(keyFile, []byte("DEEPSEEK_API_KEY=sk-nowhere-to-go\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	var logged bytes.Buffer
	d := deepSeekBalanceTestDaemon(t, &logged, keyFile)

	d.triggerDeepSeekBalance()

	if !strings.Contains(logged.String(), "wires no fetch") {
		t.Errorf("a configured tick with no fetch logged %q, want the refusal named", logged.String())
	}
	if strings.Contains(logged.String(), "sk-nowhere-to-go") {
		t.Errorf("the refusal logged the key: %q", logged.String())
	}
}

// deepSeekBalanceBlockingDaemon is a daemon whose tick waits on release before
// the key read answers, so a test can hold a tick open.
func deepSeekBalanceBlockingDaemon(t *testing.T, logged io.Writer, release <-chan struct{}, reached chan<- struct{}) *Daemon {
	t.Helper()
	d := deepSeekBalanceTestDaemon(t, logged, filepath.Join(t.TempDir(), "deepseek.env"))
	d.ctx = context.Background()
	d.newDeepSeekBalanceSeams = func() deepSeekBalanceTickSeams {
		return deepSeekBalanceTickSeams{readKey: func(string) (string, bool, error) {
			reached <- struct{}{}
			<-release
			return "", false, nil
		}}
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

// TestDeepSeekPatrolConfigDecode: the block's defaults, and the values the
// strict decoder refuses rather than reading as the default.
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
		{"an interval that does not parse", block(`{"enabled":true,"interval":"soon"}`), 0, "", "patrols.deepseek_balance.interval"},
		{"an interval that is not a string", block(`{"enabled":true,"interval":5}`), 0, "", "patrols.deepseek_balance"},
		{"a relative key file", block(`{"enabled":true,"key_file":"deepseek.env"}`), 0, "", "want an absolute path"},
		{"an unknown key", block(`{"enabled":true,"fetch_every":"1m"}`), 0, "", "patrols.deepseek_balance"},
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
