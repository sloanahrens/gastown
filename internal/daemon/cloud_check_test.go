package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// cloudCheckNow is a fixed instant: every case that does not move the clock
// reads the same time, so an interval comparison is the only thing under test.
var cloudCheckNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// cloudCheckCall is one check the patrol ran: the program, its arguments and the
// extra environment entries.
type cloudCheckCall struct {
	name string
	args []string
	env  []string
}

// cloudCheckFakes are the patrol's injected clock, runner, key lookup and the
// state the test reads back.
type cloudCheckFakes struct {
	now   time.Time
	calls []cloudCheckCall
	// statusBody is what the fake binary writes through --json; empty writes a
	// healthy report stamped with the fake clock.
	statusBody string
	// noStatus makes the next run write no status at all, as a run that exits 0
	// without reaching its report does.
	noStatus bool
	// err is what the next run returns.
	err            error
	escalations    []string
	escalationKeys []string
	clears         []string
}

// cloudCheckTestDaemon builds a daemon whose patrol runs inline, on the fake
// clock, with the given key lookup and stat.
func cloudCheckTestDaemon(t *testing.T, cfg *DaemonPatrolConfig, keyFile func(*agentconfig.CloudCheckConfig) string, stat func(string) (fs.FileInfo, error)) (*Daemon, *bytes.Buffer, *cloudCheckFakes) {
	t.Helper()
	f := &cloudCheckFakes{now: cloudCheckNow}
	var buf bytes.Buffer
	d := &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(&buf, "", 0),
		patrolConfig: cfg,
	}
	d.cloudCheck.now = func() time.Time { return f.now }
	d.cloudCheck.run = func(_ context.Context, name string, args, env []string) error {
		f.calls = append(f.calls, cloudCheckCall{name: name, args: slices.Clone(args), env: slices.Clone(env)})
		// The real binary writes the status through --json; the fake does the
		// same, so a tick's output is the file a reader would see.
		if i := slices.Index(args, cloudCheckJSONArg); i >= 0 && i+1 < len(args) && !f.noStatus {
			path := args[i+1]
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("making the status directory: %v", err)
			}
			body := f.statusBody
			if body == "" {
				body = fmt.Sprintf(`{"at":%d,"ok":1,"warnings":0,"failed":0,"items":[]}`, f.now.Unix())
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("writing the status: %v", err)
			}
			// The file's mtime is the fake clock's, so each run's write leaves a
			// known mtime that moves with the clock, and the test does not depend
			// on the host's timestamp resolution.
			if err := os.Chtimes(path, f.now, f.now); err != nil {
				t.Fatalf("stamping the status: %v", err)
			}
		}
		return f.err
	}
	d.cloudCheck.stat = stat
	d.cloudCheck.keyFile = keyFile
	d.cloudCheck.dispatch = func(fn func()) { fn() }
	d.cloudCheck.escalate = func(key, message string) error {
		f.escalationKeys = append(f.escalationKeys, key)
		f.escalations = append(f.escalations, message)
		return nil
	}
	d.cloudCheck.clear = func(key string) error {
		f.clears = append(f.clears, key)
		return nil
	}
	return d, &buf, f
}

// cloudCheckConfig is a cloud_check block with the given fields; empty ones are
// left for the patrol's own defaults.
func cloudCheckConfig(enabled bool, interval, command, keyFile string) *DaemonPatrolConfig {
	return &DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &PatrolsConfig{
			CloudCheck: &agentconfig.CloudCheckConfig{Enabled: enabled, Interval: interval, Command: command, KeyFile: keyFile},
		},
	}
}

// cloudCheckPresentKey is a key lookup that always finds the file, so a case can
// test the run itself without a real key.
func cloudCheckPresentKey() (string, func(string) (fs.FileInfo, error)) {
	return "/keys/gcp-key", func(string) (fs.FileInfo, error) { return nil, nil }
}

// TestCloudCheckRunsEachTickWithTheKeyAndRewritesTheStatus: two ticks run the
// binary with the key path from the setting and the status path the dashboard
// reads, and each run's output replaces the status file.
func TestCloudCheckRunsEachTickWithTheKeyAndRewritesTheStatus(t *testing.T) {
	t.Parallel()
	const command = "/usr/local/bin/cloudcheck"
	key, stat := cloudCheckPresentKey()
	d, _, f := cloudCheckTestDaemon(t, cloudCheckConfig(true, "10m", command, key),
		func(*agentconfig.CloudCheckConfig) string { return key }, stat)
	status := cloudCheckStatusPath(d.config.TownRoot)

	for i := 1; i <= 2; i++ {
		if i > 1 {
			// The ticker fires one interval after the previous run ended.
			f.now = f.now.Add(10 * time.Minute)
		}
		f.statusBody = fmt.Sprintf(`{"at":%d,"ok":%d,"warnings":0,"failed":0,"items":[]}`, cloudCheckNow.Unix(), i)
		d.runCloudCheckPatrol()

		if len(f.calls) != i {
			t.Fatalf("tick %d ran %d checks, want %d", i, len(f.calls), i)
		}
		call := f.calls[i-1]
		if call.name != command {
			t.Errorf("tick %d ran %q, want %q", i, call.name, command)
		}
		wantArgs := []string{cloudCheckQuietArg, cloudCheckJSONArg, status}
		if !slices.Equal(call.args, wantArgs) {
			t.Errorf("tick %d argv = %q, want %q", i, call.args, wantArgs)
		}
		if want := []string{cloudCheckKeyEnv + "=" + key}; !slices.Equal(call.env, want) {
			t.Errorf("tick %d env = %q, want %q", i, call.env, want)
		}
		body, err := os.ReadFile(status)
		if err != nil {
			t.Fatalf("tick %d: reading the status: %v", i, err)
		}
		want := fmt.Sprintf(`"ok":%d`, i)
		if !strings.Contains(string(body), want) {
			t.Errorf("tick %d status = %q, want the run's own output carrying %s", i, body, want)
		}
	}
}

// TestCloudCheckWithoutTheKeyLogsOnceAndRunsNothing: a town without the key
// runs nothing and gets one log line, not one per interval.
func TestCloudCheckWithoutTheKeyLogsOnceAndRunsNothing(t *testing.T) {
	t.Parallel()
	key, _ := cloudCheckPresentKey()
	d, logs, f := cloudCheckTestDaemon(t, cloudCheckConfig(true, "10m", "", key),
		func(*agentconfig.CloudCheckConfig) string { return "/keys/absent-gcp-key" },
		func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist })

	d.runCloudCheckPatrol()
	d.runCloudCheckPatrol()

	if len(f.calls) != 0 {
		t.Errorf("ran %d checks with no key file, want none", len(f.calls))
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("logged %d lines, want one:\n%s", len(lines), logs.String())
	}
	if !strings.Contains(lines[0], "no key file") {
		t.Errorf("the line %q does not say the key file is missing", lines[0])
	}
}

// TestCloudCheckRefusesAnUnreadableKeyFile: a key path the patrol cannot stat is
// a named log line and no run, never a check that reports the cloud unreachable.
func TestCloudCheckRefusesAnUnreadableKeyFile(t *testing.T) {
	t.Parallel()
	key, _ := cloudCheckPresentKey()
	d, logs, f := cloudCheckTestDaemon(t, cloudCheckConfig(true, "10m", "", key),
		func(*agentconfig.CloudCheckConfig) string { return key },
		func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission })

	d.runCloudCheckPatrol()

	if len(f.calls) != 0 {
		t.Errorf("ran %d checks on an unreadable key file, want none", len(f.calls))
	}
	if !strings.Contains(logs.String(), "cannot read the key file") {
		t.Errorf("log = %q, want the unreadable key file named", logs.String())
	}
}

// cloudCheckKeyPathOf returns the key path the patrol put in the child's
// environment, or "" when it put none.
func cloudCheckKeyPathOf(env []string) string {
	for _, e := range env {
		if path, ok := strings.CutPrefix(e, cloudCheckKeyEnv+"="); ok {
			return path
		}
	}
	return ""
}

// TestCloudCheckKeyNeverReachesALogEscalationOrStatus: the key's value stays in
// the file. The runner reads the key from the path the patrol passes in its
// environment, as the binary does, and its failing run's error quotes the key it
// read: the patrol must forward neither. A failing run escalates once under one
// key, a repeat refreshes that key, and a good run clears it.
func TestCloudCheckKeyNeverReachesALogEscalationOrStatus(t *testing.T) {
	t.Parallel()
	const keyValue = "gcp-service-account-private-key"
	dir := t.TempDir()
	key := filepath.Join(dir, "gcp-key")
	if err := os.WriteFile(key, []byte(keyValue), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	d, logs, f := cloudCheckTestDaemon(t, cloudCheckConfig(true, "10m", "", key),
		func(*agentconfig.CloudCheckConfig) string { return key }, os.Stat)
	f.err = errors.New("exit status 1")
	inner := d.cloudCheck.run
	var read string
	d.cloudCheck.run = func(ctx context.Context, name string, args, env []string) error {
		body, err := os.ReadFile(cloudCheckKeyPathOf(env))
		if err != nil {
			return fmt.Errorf("the runner cannot read its key: %w", err)
		}
		read = string(body)
		err = inner(ctx, name, args, env)
		if err != nil {
			return fmt.Errorf("%w (key %s)", err, body)
		}
		return nil
	}

	d.runCloudCheckPatrol()
	f.now = f.now.Add(10 * time.Minute)
	d.runCloudCheckPatrol()
	if len(f.calls) != 2 {
		t.Fatalf("ran %d checks, want 2", len(f.calls))
	}
	if read != keyValue {
		t.Fatalf("the runner read the key as %q, want the key file's text", read)
	}
	if len(f.escalations) != 2 || !slices.Equal(f.escalationKeys, []string{cloudCheckAlertKey, cloudCheckAlertKey}) {
		t.Fatalf("escalations = %q under keys %q, want two refreshes of %q", f.escalations, f.escalationKeys, cloudCheckAlertKey)
	}

	f.err = nil
	f.now = f.now.Add(10 * time.Minute)
	d.runCloudCheckPatrol()
	if len(f.clears) != 1 || f.clears[0] != cloudCheckAlertKey {
		t.Errorf("clears = %q, want one clear of %q after a good run", f.clears, cloudCheckAlertKey)
	}

	var seen []string
	seen = append(seen, logs.String())
	seen = append(seen, f.escalations...)
	status, err := os.ReadFile(cloudCheckStatusPath(d.config.TownRoot))
	if err != nil {
		t.Fatalf("reading the status: %v", err)
	}
	seen = append(seen, string(status))
	for _, s := range seen {
		if strings.Contains(s, keyValue) {
			t.Errorf("the key's value reached %q", s)
		}
	}
}

// TestCloudCheckDisabledByDefault: the patrol is off unless the config says
// enabled, whether the block is absent, present-but-off, or there is no config.
func TestCloudCheckDisabledByDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		config *DaemonPatrolConfig
	}{
		{"no config", nil},
		{"no patrols section", &DaemonPatrolConfig{Type: "daemon-patrol-config", Version: 1}},
		{"no cloud_check block", &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}},
		{"block present and off", cloudCheckConfig(false, "10m", "", "")},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			key, stat := cloudCheckPresentKey()
			d, logs, f := cloudCheckTestDaemon(t, tt.config,
				func(*agentconfig.CloudCheckConfig) string { return key }, stat)

			d.runCloudCheckPatrol()

			if len(f.calls) != 0 {
				t.Errorf("ran %v, want no check: the patrol is off", f.calls)
			}
			if logs.Len() != 0 {
				t.Errorf("logged %q, want silence from a disabled patrol", logs.String())
			}
		})
	}
}

// TestCloudCheckRefusesAnIntervalItCannotSchedule: a config built in code with a
// bad interval runs nothing and says why once.
func TestCloudCheckRefusesAnIntervalItCannotSchedule(t *testing.T) {
	t.Parallel()
	key, stat := cloudCheckPresentKey()
	d, logs, f := cloudCheckTestDaemon(t, cloudCheckConfig(true, "soon", "", key),
		func(*agentconfig.CloudCheckConfig) string { return key }, stat)

	d.runCloudCheckPatrol()

	if len(f.calls) != 0 {
		t.Errorf("ran %d checks on an unschedulable interval, want none", len(f.calls))
	}
	if !strings.Contains(logs.String(), "cloud_check: not running") {
		t.Errorf("log = %q, want the interval refusal named", logs.String())
	}
}

// TestCloudCheckExitZeroWithoutANewStatusKeepsTheAlert: an exit 0 that leaves
// the status file as the last run wrote it is a failed run. It must not clear
// the alert the failure raised, and it must refresh that alert as a failure.
func TestCloudCheckExitZeroWithoutANewStatusKeepsTheAlert(t *testing.T) {
	t.Parallel()
	key, stat := cloudCheckPresentKey()
	d, logs, f := cloudCheckTestDaemon(t, cloudCheckConfig(true, "10m", "", key),
		func(*agentconfig.CloudCheckConfig) string { return key }, stat)

	f.err = cloudCheckExitErr(3)
	d.runCloudCheckPatrol()
	if len(f.escalationKeys) != 1 {
		t.Fatalf("the failing run escalated %d times, want 1", len(f.escalationKeys))
	}

	f.now = f.now.Add(10 * time.Minute)
	f.err = nil
	f.noStatus = true
	d.runCloudCheckPatrol()

	if len(f.clears) != 0 {
		t.Errorf("clears = %q after an exit 0 that wrote no status, want none", f.clears)
	}
	if !slices.Equal(f.escalationKeys, []string{cloudCheckAlertKey, cloudCheckAlertKey}) {
		t.Errorf("escalations under keys %q, want the alert refreshed as a failure", f.escalationKeys)
	}
	if !strings.Contains(logs.String(), "without a new status") {
		t.Errorf("log = %q, want the run reported as exit 0 without a new status", logs.String())
	}
}

// TestCloudCheckStatusWrittenByARun: the status file counts as written by a run
// when it is present afterward and either was absent before or its state
// changed. A file the run left as it was, or removed, was not written by it.
func TestCloudCheckStatusWrittenByARun(t *testing.T) {
	t.Parallel()
	old := time.Unix(1_000_000, 0)
	// put writes body and stamps it with mod, so the state the run starts with
	// is known, whatever the host's clock says.
	put := func(body string, mod time.Time) func(path string) error {
		return func(path string) error {
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				return err
			}
			return os.Chtimes(path, mod, mod)
		}
	}
	nothing := func(string) error { return nil }
	remove := func(path string) error { return os.Remove(path) }
	cases := []struct {
		name string
		// before sets up the status file the run starts with; nil leaves it absent.
		before func(path string) error
		// run is what the run does to the status file.
		run  func(path string) error
		want bool
	}{
		{"absent before, written by the run", nil, put("{}", cloudCheckNow), true},
		{"absent before and after", nil, nothing, false},
		{"left by an earlier run, not rewritten", put("{}", old), nothing, false},
		{"left by an earlier run, rewritten", put("{}", old), put("{}", cloudCheckNow), true},
		{"rewritten with the same modification time", put("{}", old), put(`{"at":1}`, old), true},
		{"rewritten with the same size and modification time", put(`{"at":1}`, old), put(`{"at":2}`, old), true},
		{"rewritten with the same bytes, size and modification time", put(`{"at":1}`, old), put(`{"at":1}`, old), false},
		{"left by an earlier run, removed", put("{}", old), remove, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "status.json")
			if tt.before != nil {
				if err := tt.before(path); err != nil {
					t.Fatalf("setting up the status: %v", err)
				}
			}
			before := cloudCheckStatusFileOf(path)
			if err := tt.run(path); err != nil {
				t.Fatalf("running: %v", err)
			}
			if got := cloudCheckWritten(before, cloudCheckStatusFileOf(path)); got != tt.want {
				t.Errorf("cloudCheckWritten = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCloudCheckUnreadableStatusBeforeTheRunIsNotAWrite: a status file the daemon
// cannot stat before the run has no known state to compare against, so a run
// that leaves it readable does not count as having written it.
func TestCloudCheckUnreadableStatusBeforeTheRunIsNotAWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("writing the blocker: %v", err)
	}
	before := cloudCheckStatusFileOf(filepath.Join(blocker, "status.json"))
	if before.err == nil || errors.Is(before.err, fs.ErrNotExist) {
		t.Fatalf("setup: stat before the run = %v, want an unreadable file", before.err)
	}
	path := filepath.Join(dir, "status.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("writing the status: %v", err)
	}
	if cloudCheckWritten(before, cloudCheckStatusFileOf(path)) {
		t.Errorf("cloudCheckWritten = true for a status unreadable before the run, want false")
	}
}

// cloudCheckExitErr is a child's failure that reports its exit status, as an
// *exec.ExitError does.
type cloudCheckExitErr int

func (e cloudCheckExitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e cloudCheckExitErr) ExitCode() int { return int(e) }

// TestCloudCheckFailedRunEscalatesWithItsExitStatus: the production run path
// turns a child's non-zero exit into one escalation that names the command and
// its exit status. The child's output, which can carry an app URL with a
// credential, reaches neither the escalation nor the log.
func TestCloudCheckFailedRunEscalatesWithItsExitStatus(t *testing.T) {
	t.Parallel()
	const command = "/usr/local/bin/cloudcheck"
	const leak = "user:app-credential@app.example.test"
	key := filepath.Join(t.TempDir(), "gcp-key")
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}
	d, logs, f := cloudCheckTestDaemon(t, cloudCheckConfig(true, "10m", command, key),
		func(*agentconfig.CloudCheckConfig) string { return key }, os.Stat)
	// Drop the fake run so the production cloudCheckRun executes over the exec seam.
	d.cloudCheck.run = nil
	d.execCmd = func(*exec.Cmd) ([]byte, []byte, error) {
		return []byte("app https://" + leak + " is down"), nil, cloudCheckExitErr(3)
	}

	d.runCloudCheckPatrol()

	if len(f.escalations) != 1 || !slices.Equal(f.escalationKeys, []string{cloudCheckAlertKey}) {
		t.Fatalf("escalations = %q under keys %q, want one under %q", f.escalations, f.escalationKeys, cloudCheckAlertKey)
	}
	if want := command + " exit 3"; !strings.Contains(f.escalations[0], want) {
		t.Errorf("escalation = %q, want it to name %q", f.escalations[0], want)
	}
	for _, s := range append([]string{logs.String()}, f.escalations...) {
		if strings.Contains(s, leak) {
			t.Errorf("the child's output reached %q", s)
		}
	}
}

// TestCloudCheckRunStoppedByShutdownRaisesNoEscalation: shutdown cancels the
// daemon's context, which is the parent of the run's, and the run dies with it.
// The check did not fail, so no failure escalation is raised, and one log line
// says the run stopped for the shutdown.
func TestCloudCheckRunStoppedByShutdownRaisesNoEscalation(t *testing.T) {
	t.Parallel()
	key, stat := cloudCheckPresentKey()
	d, logs, f := cloudCheckTestDaemon(t, cloudCheckConfig(true, "10m", "", key),
		func(*agentconfig.CloudCheckConfig) string { return key }, stat)
	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	d.ctx = ctx
	d.cloudCheck.run = func(runCtx context.Context, name string, _ []string, _ []string) error {
		shutdown()
		<-runCtx.Done()
		return fmt.Errorf("%s: signal: killed", name)
	}

	d.runCloudCheckPatrol()

	if len(f.escalationKeys) != 0 {
		t.Errorf("a run stopped by shutdown escalated under %q, want none", f.escalationKeys)
	}
	if len(f.clears) != 0 {
		t.Errorf("a run stopped by shutdown cleared %q, want none", f.clears)
	}
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, "cloud_check:") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "shutdown") {
		t.Errorf("cloud_check log lines = %q, want one line naming the shutdown", lines)
	}
}
