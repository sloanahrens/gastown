package daemon

// The deepseek_balance patrol (gt-whaxw7): a ticker of its own on the cadence
// daemon.patrols.deepseek_balance names, and a tick that reads the DeepSeek key
// from the key file the config names. A town with no key file is unconfigured:
// the tick says so once and fetches nothing, so a missing Secret is one line
// rather than one per interval.
//
// The key reaches only the fetch seam. Every line this file logs names the key
// file's path, never its contents, and so does every error the reader returns.
// A configured tick fetches the balance and writes it as a deepseek_balance row
// in cost_samples, whose note carries the alerts the reading raised (gt-a5c8ld,
// gt-v1axxb). A tick whose seams carry no fetch refuses rather than reporting a
// balance it did not read.
//
// The alerts themselves are deepseekbalance.Evaluate's lines, filed as
// escalations under one stable key per condition and cleared when the condition
// ends, so a balance that stays low leaves one open escalation rather than one
// per tick (gt-vwry). The previous good reading lives on the daemon: Evaluate
// compares against it, and a lost tick sequence resets the comparison rather
// than inventing a spend.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deepseekbalance"
)

const (
	// deepSeekBalancePatrol is the patrol's name in daemon.patrols and in
	// daemon.container_off.
	deepSeekBalancePatrol = "deepseek_balance"

	// deepSeekBalanceAPI is the DeepSeek API the balance is read from.
	deepSeekBalanceAPI = "https://api.deepseek.com"

	// deepSeekBalanceTickTimeout bounds a whole tick, its fetch, row write and
	// alerts included, so a stuck request frees the patrol's single-flight
	// guard.
	deepSeekBalanceTickTimeout = 2 * time.Minute

	// deepSeekBalanceJoinWait bounds how long Run waits for a tick still
	// running after the ticker stopped. A tick left past it finishes one
	// bounded request, and the next start needs nothing it wrote.
	deepSeekBalanceJoinWait = 10 * time.Second
)

// Alert keys for the patrol's three conditions: the stable identity of each
// one, so gt escalate records a repeat on the open escalation instead of
// minting a bead per tick (gt-vwry).
const (
	alertKeyDeepSeekSpendJump   = "deepseek_balance:spend-jump"
	alertKeyDeepSeekLowBalance  = "deepseek_balance:low-balance"
	alertKeyDeepSeekUnreachable = "deepseek_balance:unreachable"
)

// deepSeekBalanceAlertKeys is every key the patrol files a condition under, so
// a tick that read the balance clears the ones it did not raise.
var deepSeekBalanceAlertKeys = []string{
	alertKeyDeepSeekSpendJump,
	alertKeyDeepSeekLowBalance,
	alertKeyDeepSeekUnreachable,
}

// The first words of the line each condition raises. Evaluate returns prose,
// not a typed alert, so this is how a line is matched to its key.
// TestDeepSeekPatrolAlertKeyMap pins each prefix against a real Evaluate line:
// a reworded alert in internal/deepseekbalance fails there rather than losing
// its escalation here.
const (
	deepSeekBalanceSpendJumpPrefix   = "DeepSeek spent $"
	deepSeekBalanceLowBalancePrefix  = "DeepSeek balance is $"
	deepSeekBalanceUnreachablePrefix = "DeepSeek balance endpoint unreachable for "
)

// deepSeekBalanceAlertKey returns the stable key for one line of Evaluate's
// answer, and false for a line no key names.
func deepSeekBalanceAlertKey(line string) (string, bool) {
	switch {
	case strings.HasPrefix(line, deepSeekBalanceSpendJumpPrefix):
		return alertKeyDeepSeekSpendJump, true
	case strings.HasPrefix(line, deepSeekBalanceLowBalancePrefix):
		return alertKeyDeepSeekLowBalance, true
	case strings.HasPrefix(line, deepSeekBalanceUnreachablePrefix):
		return alertKeyDeepSeekUnreachable, true
	}
	return "", false
}

// deepSeekBalanceState is what the patrol remembers between ticks: the last
// good reading Evaluate compares against, when it was taken, and the run of
// failed fetches since. deepSeekBalanceStateMu guards it.
type deepSeekBalanceState struct {
	last     *deepseekbalance.Sample
	lastAt   time.Time
	failures int
}

// deepSeekBalanceInterval returns the period between two ticks, from the config
// the loader already judged: a value it refused never reaches the daemon.
func deepSeekBalanceInterval(cfg *DaemonPatrolConfig) (time.Duration, error) {
	var c *agentconfig.DeepSeekBalanceConfig
	if cfg != nil && cfg.Patrols != nil {
		c = cfg.Patrols.DeepSeekBalance
	}
	return c.IntervalDuration()
}

// deepSeekBalanceKeyFile returns the file the tick reads the key from: the
// configured path, or the one the daemon pod mounts it at. A setting the loader
// already refused never reaches here; a config built in code is judged again,
// since a relative path would resolve against the daemon's working directory.
func deepSeekBalanceKeyFile(cfg *DaemonPatrolConfig) (string, error) {
	var c *agentconfig.DeepSeekBalanceConfig
	if cfg != nil && cfg.Patrols != nil {
		c = cfg.Patrols.DeepSeekBalance
	}
	return c.ParseKeyFile()
}

// startDeepSeekBalanceTicker starts the patrol's own ticker when the patrol is
// active, and returns a nil channel and stop func when it is not: a town that
// names no key starts nothing. A configured interval the patrol cannot
// schedule starts no ticker either, and says so once, rather than running on a
// cadence the town never asked for.
func (d *Daemon) startDeepSeekBalanceTicker() (<-chan time.Time, func()) {
	if !d.isPatrolActive(deepSeekBalancePatrol) {
		return nil, nil
	}
	every, err := deepSeekBalanceInterval(d.patrol())
	if err != nil {
		d.logger.Printf("DeepSeek balance patrol not started: %v", err)
		return nil, nil
	}
	newTicker := d.newDeepSeekBalanceTicker
	if newTicker == nil {
		newTicker = func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		}
	}
	ticks, stop := newTicker(every)
	d.logger.Printf("DeepSeek balance ticker started (interval %v)", every)
	return ticks, stop
}

// deepSeekBalanceTickSeams are one tick's key reader, the fetch and the
// evaluator.
type deepSeekBalanceTickSeams struct {
	// readKey reads the key the file at path sets, and reports whether it set
	// one: an absent file is the unconfigured town, not a failure. An error it
	// returns names the path and never the key.
	readKey func(path string) (key string, configured bool, err error)
	// fetch reads the balance with key. It is given the key, never the path,
	// and must keep the key out of its errors. Nil refuses a configured tick.
	fetch func(ctx context.Context, key string) (deepseekbalance.Sample, error)
	// evaluate is deepseekbalance.Evaluate: the lines the reading raises. A
	// test hands in a line the key map does not know to drive that refusal.
	evaluate func(prev *deepseekbalance.Sample, cur deepseekbalance.Sample, consecutiveFailures int, interval time.Duration) []string
}

// check returns the error naming the seam the tick was not given, so an
// unwired tick fails before its first read rather than at the read that needed
// it. The fetch is the one seam not named: a tick that finds no key needs none.
func (s deepSeekBalanceTickSeams) check() error {
	if s.readKey == nil {
		return errors.New("the tick has no key reader, so it cannot tell a configured town from an unconfigured one")
	}
	if s.evaluate == nil {
		return errors.New("the tick has no reading evaluator, so it cannot tell a balance within its alerts from one that raises them")
	}
	return nil
}

// deepSeekBalanceSeamsOf returns the seams a tick runs with: production's key
// reader, fetch and evaluator, or the test's replacements when it set them.
func (d *Daemon) deepSeekBalanceSeamsOf() deepSeekBalanceTickSeams {
	if d.newDeepSeekBalanceSeams != nil {
		return d.newDeepSeekBalanceSeams()
	}
	return deepSeekBalanceTickSeams{
		readKey:  deepSeekBalanceReadKey,
		fetch:    d.fetchDeepSeekBalance,
		evaluate: deepseekbalance.Evaluate,
	}
}

// fetchDeepSeekBalance reads the balance with key. The key travels only in the
// request's Authorization header, so no error it returns carries it.
func (d *Daemon) fetchDeepSeekBalance(ctx context.Context, key string) (deepseekbalance.Sample, error) {
	client := d.deepSeekBalanceHTTP
	if client == nil {
		client = http.DefaultClient
	}
	return deepseekbalance.Fetch(ctx, client, deepSeekBalanceAPI, key)
}

// writeDeepSeekBalanceRow records reading as a deepseek_balance row stamped at
// at, its note the alert lines the reading raised. A failed write is returned
// and the tick logs it; the alerts it raised stand either way.
func (d *Daemon) writeDeepSeekBalanceRow(ctx context.Context, at time.Time, reading deepseekbalance.Sample, note string) error {
	total, granted, toppedUp := reading.Total, reading.Granted, reading.ToppedUp
	row := beads.Sample{
		Kind:     beads.SampleKindDeepSeekBalance,
		At:       at,
		Total:    &total,
		Granted:  &granted,
		ToppedUp: &toppedUp,
		Note:     note,
	}
	if err := d.costSampleBeads().WriteCostSample(ctx, row); err != nil {
		return fmt.Errorf("writing the deepseek_balance row: %w", err)
	}
	return nil
}

// costSampleBeads opens the store the balance row is written to: the town
// database, unless a test hands one in.
func (d *Daemon) costSampleBeads() beads.Client {
	if d.openCostSampleBeads != nil {
		return d.openCostSampleBeads()
	}
	return beads.New(d.config.TownRoot)
}

// startDeepSeekBalanceTick runs one tick on its own goroutine: a tick reads the
// key file and makes an HTTPS request, neither of which belongs on the main
// select loop. A tick that arrives while the previous one
// still runs is skipped and logged, since two ticks at once would spend the
// account's requests twice.
func (d *Daemon) startDeepSeekBalanceTick() {
	if !d.deepSeekBalanceRunning.CompareAndSwap(false, true) {
		d.logger.Printf("deepseek_balance: previous tick still running, skipping")
		return
	}
	d.deepSeekBalanceCycles.Add(1)
	go func() {
		defer d.deepSeekBalanceCycles.Done()
		defer d.deepSeekBalanceRunning.Store(false)
		d.triggerDeepSeekBalance()
	}()
}

// triggerDeepSeekBalance runs one tick over the town's own seams: the key it
// read, and the line a failure or an unconfigured town owes the operator.
func (d *Daemon) triggerDeepSeekBalance() {
	if d.config == nil || !d.isPatrolActive(deepSeekBalancePatrol) {
		return
	}
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, deepSeekBalanceTickTimeout)
	defer cancel()
	keyFile, err := deepSeekBalanceKeyFile(d.patrol())
	if err != nil {
		d.logger.Printf("deepseek_balance: %v", err)
		return
	}
	if err := d.runDeepSeekBalanceTick(ctx, d.deepSeekBalanceSeamsOf(), keyFile); err != nil {
		d.logger.Printf("deepseek_balance: %v", err)
	}
}

// runDeepSeekBalanceTick runs one tick. A town with no key file is
// unconfigured: the tick says so, and that is no error, because nothing is
// wrong. A key the file sets but nothing fetches is a refusal, never a clean
// tick.
//
// A good reading is recorded, with the alerts it raised as its row's note. A
// failed fetch is not a reading: the tick counts it, and only the unreachable
// rule is judged, so the alerts a reading would have re-checked stay open.
func (d *Daemon) runDeepSeekBalanceTick(ctx context.Context, s deepSeekBalanceTickSeams, keyFile string) error {
	if err := s.check(); err != nil {
		return err
	}
	key, configured, err := s.readKey(keyFile)
	if err != nil {
		return fmt.Errorf("reading the key file: %w", err)
	}
	if !configured {
		d.deepSeekBalanceUnconfigured(keyFile)
		return nil
	}
	// A key was read, so a later absence owes the operator one more line.
	d.deepSeekBalanceKeyRead()
	if s.fetch == nil {
		return fmt.Errorf("the key file %s sets a key but this build wires no fetch, so no balance was read", keyFile)
	}
	every, err := deepSeekBalanceInterval(d.patrol())
	if err != nil {
		return err
	}
	reading, err := s.fetch(ctx, key)
	if err != nil {
		lines := s.evaluate(nil, deepseekbalance.Sample{}, d.deepSeekBalanceRecordFailure(), every)
		_, alertErr := d.raiseDeepSeekBalanceAlerts(ctx, lines)
		return errors.Join(err, alertErr)
	}

	prev, prevAt := d.deepSeekBalancePrevious()
	now := d.clk().Now()
	// The spend is spread over the time the two readings are apart, which is
	// longer than the cadence when failed fetches interrupted it. A first
	// reading, or a clock that did not move, has no spacing to measure.
	spread := every
	if !prevAt.IsZero() {
		if elapsed := now.Sub(prevAt); elapsed > 0 {
			spread = elapsed
		}
	}
	lines := s.evaluate(prev, reading, 0, spread)
	d.deepSeekBalanceRecordReading(reading, now)
	writeErr := d.writeDeepSeekBalanceRow(ctx, now, reading, strings.Join(lines, "\n"))
	raised, alertErr := d.raiseDeepSeekBalanceAlerts(ctx, lines)
	clearErr := d.clearDeepSeekBalanceAlerts(ctx, raised)
	return errors.Join(writeErr, alertErr, clearErr)
}

// deepSeekBalancePrevious returns the last good reading and when it was taken,
// or nil and the zero time before the first.
func (d *Daemon) deepSeekBalancePrevious() (*deepseekbalance.Sample, time.Time) {
	d.deepSeekBalanceStateMu.Lock()
	defer d.deepSeekBalanceStateMu.Unlock()
	return d.deepSeekBalanceState.last, d.deepSeekBalanceState.lastAt
}

// deepSeekBalanceRecordReading records a good reading, which resets the run of
// failed fetches.
func (d *Daemon) deepSeekBalanceRecordReading(reading deepseekbalance.Sample, at time.Time) {
	d.deepSeekBalanceStateMu.Lock()
	defer d.deepSeekBalanceStateMu.Unlock()
	d.deepSeekBalanceState.last = &reading
	d.deepSeekBalanceState.lastAt = at
	d.deepSeekBalanceState.failures = 0
}

// deepSeekBalanceRecordFailure counts one failed fetch and returns the run it
// belongs to.
func (d *Daemon) deepSeekBalanceRecordFailure() int {
	d.deepSeekBalanceStateMu.Lock()
	defer d.deepSeekBalanceStateMu.Unlock()
	d.deepSeekBalanceState.failures++
	return d.deepSeekBalanceState.failures
}

// raiseDeepSeekBalanceAlerts files each line as an escalation under the key its
// condition has, and returns the keys it filed. A line no key names is
// reported, never filed under a key derived from the line: a key that varies
// with the amounts would mint a new bead per firing (gt-vwry).
func (d *Daemon) raiseDeepSeekBalanceAlerts(ctx context.Context, lines []string) (map[string]bool, error) {
	filed := make(map[string]bool, len(lines))
	var err error
	for _, line := range lines {
		key, ok := deepSeekBalanceAlertKey(line)
		if !ok {
			err = errors.Join(err, fmt.Errorf("no alert key names the balance alert %q, so it was not filed", line))
			continue
		}
		filed[key] = true
		if e := d.escalateAlertCtx(ctx, key, deepSeekBalancePatrol, line); e != nil {
			err = errors.Join(err, fmt.Errorf("filing the %s alert: %w", key, e))
		}
	}
	return filed, err
}

// clearDeepSeekBalanceAlerts closes the keys a good reading re-checked and did
// not raise. Only a good reading calls it: a failed fetch re-checks no
// condition but reachability, and the unreachable key stays open while the
// failures continue.
func (d *Daemon) clearDeepSeekBalanceAlerts(ctx context.Context, raised map[string]bool) error {
	var clear []string
	for _, key := range deepSeekBalanceAlertKeys {
		if !raised[key] {
			clear = append(clear, key)
		}
	}
	if len(clear) == 0 {
		return nil
	}
	return d.clearAlertsCtx(ctx, "the balance was read and this alert did not fire", clear...)
}

// deepSeekBalanceUnconfigured logs the unconfigured line once per absence: a
// town that never mounts the key gets one line, and a key that goes away after
// a good read gets one more.
func (d *Daemon) deepSeekBalanceUnconfigured(path string) {
	d.deepSeekBalanceStateMu.Lock()
	said := d.deepSeekBalanceUnconfiguredSaid
	d.deepSeekBalanceUnconfiguredSaid = true
	d.deepSeekBalanceStateMu.Unlock()
	if said {
		return
	}
	d.logger.Printf("deepseek_balance: unconfigured: no key at %s; not fetching (mount the gt-deepseek-balance Secret, or set patrols.deepseek_balance.key_file)", path)
}

// deepSeekBalanceKeyRead records that the tick read a key, so a later absence
// is logged once more rather than passing silently.
func (d *Daemon) deepSeekBalanceKeyRead() {
	d.deepSeekBalanceStateMu.Lock()
	d.deepSeekBalanceUnconfiguredSaid = false
	d.deepSeekBalanceStateMu.Unlock()
}

// deepSeekBalanceReadKey reads the key the file at path sets under
// agentconfig.DeepSeekBalanceKeyEnv. An absent file is the unconfigured town
// (configured false, no error); a file that sets no key is an error, since its
// operator meant it to hold one. A read error names the path only.
func deepSeekBalanceReadKey(path string) (string, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the operator's key_file setting
	// An absent file is the unconfigured town, which the caller reports once. It
	// is a state, not a failure, so it is the one read that returns no error.
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%s: %w", path, err)
	}
	key := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// An "export " prefix is what a shell-sourced file carries; the town's
		// deepseek.env is written either way.
		name, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok || strings.TrimSpace(name) != agentconfig.DeepSeekBalanceKeyEnv {
			continue
		}
		if v := deepSeekBalanceUnquote(strings.TrimSpace(value)); v != "" {
			key = v
		}
	}
	if key == "" {
		return "", false, fmt.Errorf("%s sets no %s", path, agentconfig.DeepSeekBalanceKeyEnv)
	}
	return key, true, nil
}

// deepSeekBalanceUnquote strips one pair of matching quotes around s, so a key
// written as KEY="sk-..." reads the same as a bare one. A lone quote is not a
// pair and is kept: it is part of the key.
func deepSeekBalanceUnquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// stopDeepSeekBalancePatrol stops the patrol's ticker and joins the tick it may
// have running, bounded, so Run's return leaves no tick reading the town after
// the daemon is gone.
func (d *Daemon) stopDeepSeekBalancePatrol(stop func()) {
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() {
		d.deepSeekBalanceCycles.Wait()
		close(done)
	}()
	bound := d.deepSeekBalanceJoinWait
	if bound <= 0 {
		bound = deepSeekBalanceJoinWait
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		d.logger.Printf("deepseek_balance: a tick is still running %s after the ticker stopped; Run leaves without it", bound)
	}
}
