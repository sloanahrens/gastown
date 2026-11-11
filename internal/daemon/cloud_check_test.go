package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
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
		if i := slices.Index(args, cloudCheckJSONArg); i >= 0 && i+1 < len(args) {
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

// TestCloudCheckKeyNeverReachesALogEscalationOrStatus: the key's value stays in
// the file. A failing run escalates once under one key, a repeat refreshes that
// key, and a good run clears it.
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

	d.runCloudCheckPatrol()
	f.now = f.now.Add(10 * time.Minute)
	d.runCloudCheckPatrol()
	if len(f.calls) != 2 {
		t.Fatalf("ran %d checks, want 2", len(f.calls))
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
