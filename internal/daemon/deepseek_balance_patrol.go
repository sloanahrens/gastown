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
// in cost_samples (gt-a5c8ld). A tick whose seams carry no fetch refuses rather
// than reporting a balance it did not read.

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

	// deepSeekBalanceTickTimeout bounds a whole tick, the fetch and the row
	// write included, so a stuck request frees the patrol's single-flight guard.
	deepSeekBalanceTickTimeout = 2 * time.Minute

	// deepSeekBalanceJoinWait bounds how long Run waits for a tick still
	// running after the ticker stopped. A tick left past it finishes one
	// bounded request, and the next start needs nothing it wrote.
	deepSeekBalanceJoinWait = 10 * time.Second
)

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

// deepSeekBalanceTickSeams are one tick's key reader and the fetch.
type deepSeekBalanceTickSeams struct {
	// readKey reads the key the file at path sets, and reports whether it set
	// one: an absent file is the unconfigured town, not a failure. An error it
	// returns names the path and never the key.
	readKey func(path string) (key string, configured bool, err error)
	// fetch reads the balance with key and writes the row. It is given the key,
	// never the path, and must keep the key out of its errors. Nil refuses a
	// configured tick.
	fetch func(ctx context.Context, key string) error
}

// check returns the error naming the seam the tick was not given, so an
// unwired tick fails before its first read rather than at the read that needed
// it. The fetch is not named: a tick that finds no key needs none.
func (s deepSeekBalanceTickSeams) check() error {
	if s.readKey == nil {
		return errors.New("the tick has no key reader, so it cannot tell a configured town from an unconfigured one")
	}
	return nil
}

// deepSeekBalanceSeamsOf returns the seams a tick runs with: production's key
// reader and fetch, or the test's replacements when it set them.
func (d *Daemon) deepSeekBalanceSeamsOf() deepSeekBalanceTickSeams {
	if d.newDeepSeekBalanceSeams != nil {
		return d.newDeepSeekBalanceSeams()
	}
	return deepSeekBalanceTickSeams{readKey: deepSeekBalanceReadKey, fetch: d.fetchDeepSeekBalance}
}

// fetchDeepSeekBalance reads the balance with key and writes it as a
// deepseek_balance row stamped with the daemon's clock. A failed fetch writes no
// row: its error is returned and the tick logs it. The key travels only in the
// request's Authorization header, so neither the row nor an error carries it.
func (d *Daemon) fetchDeepSeekBalance(ctx context.Context, key string) error {
	client := d.deepSeekBalanceHTTP
	if client == nil {
		client = http.DefaultClient
	}
	s, err := deepseekbalance.Fetch(ctx, client, deepSeekBalanceAPI, key)
	if err != nil {
		return err
	}
	total, granted, toppedUp := s.Total, s.Granted, s.ToppedUp
	row := beads.Sample{
		Kind:     beads.SampleKindDeepSeekBalance,
		At:       d.clk().Now(),
		Total:    &total,
		Granted:  &granted,
		ToppedUp: &toppedUp,
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
	keyRead, err := runDeepSeekBalanceTick(ctx, d.deepSeekBalanceSeamsOf(), keyFile)
	if err != nil {
		d.logger.Printf("deepseek_balance: %v", err)
	}
	switch {
	case keyRead:
		d.deepSeekBalanceKeyRead()
	case err == nil:
		d.deepSeekBalanceUnconfigured(keyFile)
	}
}

// runDeepSeekBalanceTick runs one tick and reports whether it read a key. A
// town with no key file is unconfigured: the caller says so, and this is no
// error, because nothing is wrong. A key the file sets but nothing fetches is
// a refusal, never a clean tick.
func runDeepSeekBalanceTick(ctx context.Context, s deepSeekBalanceTickSeams, keyFile string) (keyRead bool, err error) {
	if err := s.check(); err != nil {
		return false, err
	}
	key, configured, err := s.readKey(keyFile)
	if err != nil {
		return false, fmt.Errorf("reading the key file: %w", err)
	}
	if !configured {
		return false, nil
	}
	if s.fetch == nil {
		return true, fmt.Errorf("the key file %s sets a key but this build wires no fetch, so no balance was read", keyFile)
	}
	return true, s.fetch(ctx, key)
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
