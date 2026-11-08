package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// daemonConfigFixture is a settings/config.json the daemon loads: a
// town-settings header and a daemon section of the identity it requires.
const daemonConfigFixture = `{
  "type": "town-settings",
  "version": 1,
  "cli_theme": "dark",
  "default_agent": "claude",
  "daemon": {
    "type": "daemon-patrol-config",
    "version": 1
  }
}
`

// daemonConfigTown writes body as the settings/config.json of a temp town.
func daemonConfigTown(t *testing.T, body string) (townRoot, path string) {
	t.Helper()
	townRoot = t.TempDir()
	path = config.TownSettingsPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return townRoot, path
}

// daemonConfigTestEnv is the command's environment over a temp town and buffers,
// with the clock the backup stamp reads.
func daemonConfigTestEnv(townRoot string, now time.Time) (env configDaemonEnv, out, errOut *bytes.Buffer) {
	out, errOut = new(bytes.Buffer), new(bytes.Buffer)
	return configDaemonEnv{
		findTown: func() (string, error) { return townRoot, nil },
		out:      out,
		errOut:   errOut,
		in:       strings.NewReader(""),
		now:      func() time.Time { return now },
	}, out, errOut
}

// daemonConfigBackups names the backups beside path, in directory order.
func daemonConfigBackups(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	prefix := filepath.Base(path) + ".bak-"
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			names = append(names, entry.Name())
		}
	}
	return names
}

// daemonConfigDocument reads path as a JSON object.
func daemonConfigDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return doc
}

func TestConfigDaemonSetValidatesWithTheDaemonsLoader(t *testing.T) {
	t.Parallel()

	t.Run("a valid change is written whole", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigFixture)
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())

		if err := configDaemonSet(env, "cli_theme", `"light"`, "theme"); err != nil {
			t.Fatalf("set cli_theme = %v", err)
		}
		if got := daemonConfigDocument(t, path)["cli_theme"]; got != "light" {
			t.Errorf("cli_theme = %v, want light", got)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want the file's 0600", fi.Mode().Perm())
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), ".tmp.") {
				t.Errorf("the write left its temp file %s", entry.Name())
			}
		}
	})

	t.Run("a refused document names the daemon's error and the file", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigFixture)
		env, out, _ := daemonConfigTestEnv(townRoot, time.Now())
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		err = configDaemonSet(env, "version", "99", "bump")
		if err == nil {
			t.Fatal("set version 99 = nil, want the version the daemon refuses")
		}
		if !strings.Contains(err.Error(), "settings/config.json") {
			t.Errorf("error = %v, want the file named", err)
		}
		if !strings.Contains(err.Error(), "unsupported config version") {
			t.Errorf("error = %v, want the loader's own text", err)
		}
		if after, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(after, before) {
			t.Errorf("file after the refused write = %q, %v; want it unchanged", after, readErr)
		}
		if backups := daemonConfigBackups(t, path); len(backups) != 0 {
			t.Errorf("backups after the refused write = %v, want none", backups)
		}
		if out.Len() != 0 {
			t.Errorf("output after the refused write = %q, want nothing", out.String())
		}
	})

	t.Run("the daemon section's identity is checked", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigFixture)
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())

		err := configDaemonSet(env, "daemon.type", `"bogus"`, "daemon")
		if err == nil || !strings.Contains(err.Error(), "daemon-patrol-config") {
			t.Fatalf("set daemon.type = %v, want the patrol loader's type error", err)
		}
		if got := daemonConfigDocument(t, path)["daemon"].(map[string]any)["type"]; got != "daemon-patrol-config" {
			t.Errorf("daemon.type = %v, want it unchanged", got)
		}
	})

	t.Run("a scheduled run_agent with no seat is refused", func(t *testing.T) {
		t.Parallel()
		const body = `{
  "type": "town-settings",
  "version": 1,
  "agents": {"claude-sonnet": {"command": "claude"}},
  "polecat_pool": {"seats": {"claude-sonnet": {"cap": 1}}},
  "daemon": {
    "type": "daemon-patrol-config",
    "version": 1,
    "patrols": {"scheduled_slings": {"enabled": true, "entries": [
      {"name": "deep-review", "rig": "gastown", "formula": "mol-deep-review", "interval": "1h", "dispatch": "spec"}
    ]}}
  }
}
`
		townRoot, path := daemonConfigTown(t, body)
		env, out, _ := daemonConfigTestEnv(townRoot, time.Now())
		before := daemonConfigMustRead(t, path)

		err := configDaemonSet(env, "daemon.patrols.scheduled_slings.entries.0.run_agent", `"claude-opus"`, "pin")
		if err == nil || !strings.Contains(err.Error(), `scheduled_slings[deep-review].run_agent "claude-opus": no seat`) {
			t.Fatalf("set an unseated run_agent = %v, want the loader's seat error", err)
		}
		if !strings.Contains(err.Error(), "settings/config.json") {
			t.Errorf("error = %v, want the file named", err)
		}
		if after := daemonConfigMustRead(t, path); !bytes.Equal(after, before) {
			t.Errorf("file after the refused write = %q, want it unchanged", after)
		}
		if backups := daemonConfigBackups(t, path); len(backups) != 0 {
			t.Errorf("backups after the refused write = %v, want none", backups)
		}
		if out.Len() != 0 {
			t.Errorf("output after the refused write = %q, want nothing", out.String())
		}
	})

	// Each of these is a document the daemon's own validators refuse; the
	// command must refuse it too, naming the validator's text and the file,
	// with the file's bytes untouched and no backup left (gt-yjzepk.4).
	const allPatrols = `{
  "type": "town-settings",
  "version": 1,
  "agents": {"claude-sonnet": {"command": "claude"}},
  "polecat_pool": {"seats": {"claude-sonnet": {"cap": 1}}},
  "daemon": {
    "type": "daemon-patrol-config",
    "version": 1,
    "container_off": [],
    "patrols": {
      "scheduled_slings": {"enabled": true, "entries": [
        {"name": "deep-review", "rig": "gastown", "formula": "mol-deep-review", "interval": "1h", "dispatch": "spec"}
      ]},
      "tier_sweep": {"enabled": false, "runner": {"backend": "kubernetes", "cpus": 8, "memory": 24, "jobs": 4, "output_root": "/tmp/gt-sweep", "context": "ctx", "dind": true}},
      "disk_maintenance": {"enabled": false, "interval": "daily", "mode": "dry_run"}
    }
  }
}
`
	cases := []struct {
		name  string
		path  string
		value string
		want  string
	}{
		{
			name:  "an invalid scheduled_slings entry",
			path:  "daemon.patrols.scheduled_slings.entries.0.interval",
			value: `"soon"`,
			want:  `scheduled_slings[deep-review]: interval "soon" must be a positive Go duration`,
		},
		{
			name:  "a tier_sweep runner with dind and memory under 6",
			path:  "daemon.patrols.tier_sweep.runner.memory",
			value: `5`,
			want:  `patrols.tier_sweep.runner.memory = 5: want at least 6 GB with dind`,
		},
		{
			name:  "a cache_trim_early_percent of 20",
			path:  "daemon.patrols.disk_maintenance.cache_trim_early_percent",
			value: `20`,
			want:  `patrols.disk_maintenance.cache_trim_early_percent = 20: want a percent from 50 to 95`,
		},
		{
			name:  "an unknown container_off name",
			path:  "daemon.container_off",
			value: `["mystery-step"]`,
			want:  `mystery-step: in daemon.container_off but unknown to the audit table`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			townRoot, path := daemonConfigTown(t, allPatrols)
			env, out, _ := daemonConfigTestEnv(townRoot, time.Now())
			before := daemonConfigMustRead(t, path)

			err := configDaemonSet(env, c.path, c.value, "change")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("set %s = %s: %v, want %q", c.path, c.value, err, c.want)
			}
			if !strings.Contains(err.Error(), "settings/config.json") {
				t.Errorf("error = %v, want the file named", err)
			}
			if after := daemonConfigMustRead(t, path); !bytes.Equal(after, before) {
				t.Errorf("file after the refused write = %q, want it unchanged", after)
			}
			if backups := daemonConfigBackups(t, path); len(backups) != 0 {
				t.Errorf("backups after the refused write = %v, want none", backups)
			}
			if out.Len() != 0 {
				t.Errorf("output after the refused write = %q, want nothing", out.String())
			}
		})
	}
}

func TestConfigDaemonBacksUpAndDiffs(t *testing.T) {
	t.Parallel()

	t.Run("a write leaves the old document in a timestamped copy and prints a diff", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		townRoot, path := daemonConfigTown(t, daemonConfigFixture)
		env, out, _ := daemonConfigTestEnv(townRoot, now)

		if err := configDaemonSet(env, "default_agent", `"claude-haiku"`, "switch"); err != nil {
			t.Fatalf("set default_agent = %v", err)
		}

		backup := path + ".bak-20261108T120000.000000000Z-switch"
		got, err := os.ReadFile(backup)
		if err != nil {
			t.Fatalf("backup = %v", err)
		}
		if string(got) != daemonConfigFixture {
			t.Errorf("backup = %q, want the document it replaced", got)
		}

		diff := out.String()
		for _, want := range []string{
			"--- settings/config.json (before)",
			"+++ settings/config.json (after)",
			"@@ -",
			"-  \"default_agent\": \"claude\",",
			"+  \"default_agent\": \"claude-haiku\",",
		} {
			if !strings.Contains(diff, want) {
				t.Errorf("diff does not contain %q:\n%s", want, diff)
			}
		}
	})

	t.Run("a write with no label is refused and leaves nothing", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigFixture)
		env, out, _ := daemonConfigTestEnv(townRoot, time.Now())

		err := configDaemonSet(env, "default_agent", `"claude-haiku"`, "")
		if err == nil || !strings.Contains(err.Error(), "--label is required") {
			t.Fatalf("set with no label = %v, want --label refused", err)
		}
		if got := daemonConfigDocument(t, path)["default_agent"]; got != "claude" {
			t.Errorf("default_agent = %v, want it unchanged", got)
		}
		if backups := daemonConfigBackups(t, path); len(backups) != 0 {
			t.Errorf("backups after the refused write = %v, want none", backups)
		}
		if out.Len() != 0 {
			t.Errorf("output after the refused write = %q, want nothing", out.String())
		}
	})

	t.Run("a label that is not safe in a file name is refused", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigFixture)
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())
		before := daemonConfigMustRead(t, path)

		err := configDaemonSet(env, "default_agent", `"claude-haiku"`, "../escape")
		if err == nil || !strings.Contains(err.Error(), "letters, digits") {
			t.Fatalf("set with an unsafe label = %v, want --label refused", err)
		}
		if after := daemonConfigMustRead(t, path); !bytes.Equal(after, before) {
			t.Errorf("file after the refused write = %q, want it unchanged", after)
		}
	})

	t.Run("two writes in one second keep distinct copies and the earlier one is the baseline", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigFixture)
		first := time.Date(2026, 11, 8, 12, 0, 0, 10, time.UTC)
		second := time.Date(2026, 11, 8, 12, 0, 0, 20, time.UTC)
		env, _, _ := daemonConfigTestEnv(townRoot, first)

		// The second write's label sorts before the first one's, so a lookup
		// that ordered by name without the stamp would pick the wrong copy.
		if err := configDaemonSet(env, "cli_theme", `"first"`, "zzz"); err != nil {
			t.Fatalf("the first write = %v", err)
		}
		env.now = func() time.Time { return second }
		env.out = new(bytes.Buffer)
		if err := configDaemonSet(env, "cli_theme", `"second"`, "aaa"); err != nil {
			t.Fatalf("the second write = %v", err)
		}

		if backups := daemonConfigBackups(t, path); len(backups) != 2 {
			t.Errorf("backups after two writes = %v, want two distinct copies", backups)
		}
		// The last change's baseline is the document the first write left.
		env.out.(*bytes.Buffer).Reset()
		if err := configDaemonDiff(env); err != nil {
			t.Fatalf("diff = %v", err)
		}
		diff := env.out.(*bytes.Buffer).String()
		if !strings.Contains(diff, `-  "cli_theme": "first",`) || !strings.Contains(diff, `+  "cli_theme": "second",`) {
			t.Errorf("diff = %q, want the first write's document as the baseline", diff)
		}
	})

	t.Run("a write that fails after the backup removes the backup it made", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigFixture)
		env, out, _ := daemonConfigTestEnv(townRoot, time.Now())
		env.update = func(path string, perm os.FileMode, mutate func(*json.RawMessage, bool) error) error {
			return config.UpdateConfigJSONNoWait(path, perm, func(cur *json.RawMessage, exists bool) error {
				if err := mutate(cur, exists); err != nil {
					return err
				}
				return errors.New("the write failed")
			})
		}
		before := daemonConfigMustRead(t, path)

		err := configDaemonSet(env, "cli_theme", `"light"`, "theme")
		if err == nil || !strings.Contains(err.Error(), "the write failed") {
			t.Fatalf("set with a failing write = %v, want the write's error", err)
		}
		if after := daemonConfigMustRead(t, path); !bytes.Equal(after, before) {
			t.Errorf("file after the failed write = %q, want it unchanged", after)
		}
		if backups := daemonConfigBackups(t, path); len(backups) != 0 {
			t.Errorf("backups after the failed write = %v, want the backup removed", backups)
		}
		if out.Len() != 0 {
			t.Errorf("output after the failed write = %q, want nothing", out.String())
		}
	})

	t.Run("a failed write whose backup cannot be removed reports both", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigFixture)
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())
		env.update = func(path string, perm os.FileMode, mutate func(*json.RawMessage, bool) error) error {
			return config.UpdateConfigJSONNoWait(path, perm, func(cur *json.RawMessage, exists bool) error {
				if err := mutate(cur, exists); err != nil {
					return err
				}
				// A non-empty directory where the copy is cannot be unlinked,
				// so the cleanup's own failure has to be reported.
				entries, err := os.ReadDir(filepath.Dir(path))
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if !strings.HasPrefix(entry.Name(), filepath.Base(path)+".bak-") {
						continue
					}
					full := filepath.Join(filepath.Dir(path), entry.Name())
					if err := os.Remove(full); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(full, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(full, "keep"), []byte("x"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("the write failed")
			})
		}
		before := daemonConfigMustRead(t, path)

		err := configDaemonSet(env, "cli_theme", `"light"`, "theme")
		if err == nil || !strings.Contains(err.Error(), "the write failed") || !strings.Contains(err.Error(), "removing the backup") {
			t.Fatalf("set = %v, want the write's error and the failed removal", err)
		}
		if after := daemonConfigMustRead(t, path); !bytes.Equal(after, before) {
			t.Errorf("file after the failed write = %q, want it unchanged", after)
		}
	})
}

func TestConfigDaemonRefusesSecrets(t *testing.T) {
	t.Parallel()

	const token = "sk-ant-0123456789abcdef0123"
	const withAgent = `{
  "type": "town-settings",
  "version": 1,
  "agents": {
    "worker": {
      "command": "claude",
      "env": {
        "API_BASE": "https://example.invalid"
      }
    }
  },
  "default_agent": "worker"
}
`

	t.Run("a literal token is refused by key and never printed", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, withAgent)
		env, out, errOut := daemonConfigTestEnv(townRoot, time.Now())
		// Standard input is the way past the argv guard, so this drives the
		// literal-secret refusal itself.
		env.in = strings.NewReader(`"` + token + `"`)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		err = configDaemonSet(env, "agents.worker.env.ANTHROPIC_AUTH_TOKEN", "-", "token")
		if err == nil {
			t.Fatal("set of a literal token = nil, want it refused")
		}
		if !strings.Contains(err.Error(), "agents.worker.env.ANTHROPIC_AUTH_TOKEN") {
			t.Errorf("error = %v, want the key named", err)
		}
		for name, text := range map[string]string{"error": err.Error(), "output": out.String(), "stderr": errOut.String()} {
			if strings.Contains(text, token) {
				t.Errorf("%s prints the token:\n%s", name, text)
			}
		}
		if after, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(after, before) {
			t.Errorf("file after the refused write = %q, %v; want it unchanged", after, readErr)
		}
		if backups := daemonConfigBackups(t, path); len(backups) != 0 {
			t.Errorf("backups after the refused write = %v, want none", backups)
		}
	})

	t.Run("the diff redacts a token the file already holds", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, strings.Replace(withAgent,
			`"API_BASE": "https://example.invalid"`,
			`"API_BASE": "https://example.invalid",`+"\n        "+`"ANTHROPIC_AUTH_TOKEN": "`+token+`"`, 1))
		env, out, _ := daemonConfigTestEnv(townRoot, time.Now())

		// Removing the token is allowed: the new document holds none. The
		// removed line is where the value would be printed if it were not
		// redacted.
		if err := configDaemonUnset(env, "agents.worker.env.ANTHROPIC_AUTH_TOKEN", "token"); err != nil {
			t.Fatalf("unset the token = %v", err)
		}
		diff := out.String()
		if strings.Contains(diff, token) {
			t.Errorf("the diff prints the token:\n%s", diff)
		}
		if !strings.Contains(diff, `-        "ANTHROPIC_AUTH_TOKEN": "<redacted: before>"`) {
			t.Errorf("the diff does not redact the token:\n%s", diff)
		}
		if _, ok := daemonConfigDocument(t, path)["agents"].(map[string]any); !ok {
			t.Error("the document lost its agents block")
		}
	})

	t.Run("a secret-looking value is refused on argv and from stdin, and never written", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, withAgent)
		env, out, errOut := daemonConfigTestEnv(townRoot, time.Now())
		before := daemonConfigMustRead(t, path)
		const key = "daemon.env.SOME_SERVICE_TOKEN"

		for name, run := range map[string]func() error{
			"argv": func() error { return configDaemonSet(env, key, `"`+token+`"`, "token") },
			"stdin": func() error {
				env.in = strings.NewReader(`"` + token + `"`)
				return configDaemonSet(env, key, "-", "token")
			},
		} {
			err := run()
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "settings/daemon.env") {
				t.Fatalf("set of a token from %s = %v, want it refused naming %s and settings/daemon.env", name, err, key)
			}
			for what, text := range map[string]string{"error": err.Error(), "output": out.String(), "stderr": errOut.String()} {
				if strings.Contains(text, token) {
					t.Errorf("%s: the %s prints the token:\n%s", name, what, text)
				}
			}
			if after := daemonConfigMustRead(t, path); !bytes.Equal(after, before) {
				t.Errorf("%s: file after the refused write = %q, want it unchanged", name, after)
			}
			if backups := daemonConfigBackups(t, path); len(backups) != 0 {
				t.Errorf("%s: backups after the refused write = %v, want none", name, backups)
			}
		}
	})

	t.Run("get never prints a token the file already holds", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, withAgent)
		// A token placed in the file by hand, the way an older edit left it.
		doc := daemonConfigDocument(t, path)
		daemon, _ := doc["daemon"].(map[string]any)
		if daemon == nil {
			daemon = map[string]any{}
			doc["daemon"] = daemon
		}
		daemon["env"] = map[string]any{"SOME_SERVICE_TOKEN": token, "PLAIN": "1"}
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		for name, args := range map[string][]string{"the whole document": nil, "the env table": {"daemon.env"}, "the token itself": {"daemon.env.SOME_SERVICE_TOKEN"}} {
			env, out, errOut := daemonConfigTestEnv(townRoot, time.Now())
			if err := configDaemonGet(env, args); err != nil {
				t.Fatalf("get %s = %v", name, err)
			}
			if strings.Contains(out.String(), token) || strings.Contains(errOut.String(), token) {
				t.Errorf("get %s prints the token:\n%s", name, out.String())
			}
			if !strings.Contains(out.String(), "<redacted>") {
				t.Errorf("get %s = %q, want the token shown as <redacted>", name, out.String())
			}
		}
		env, out, _ := daemonConfigTestEnv(townRoot, time.Now())
		if err := configDaemonGet(env, []string{"daemon.env.PLAIN"}); err != nil || !strings.Contains(out.String(), "1") {
			t.Errorf("get of a plain value = %q (%v), want it printed", out.String(), err)
		}
	})

	t.Run("a changed token shows as a change without either value", func(t *testing.T) {
		t.Parallel()
		const second = "sk-ant-9876543210fedcba9876"
		// The command no longer writes a literal token, so the two documents the
		// diff compares are built by hand, the way an older edit left them.
		tree := func(value string) map[string]any {
			return map[string]any{"daemon": map[string]any{"env": map[string]any{"SOME_SERVICE_TOKEN": value}}}
		}

		diff := daemonConfigDiff("settings/config.json", tree(token), tree(second))
		for _, secret := range []string{token, second} {
			if strings.Contains(diff, secret) {
				t.Errorf("the diff prints a token:\n%s", diff)
			}
		}
		// Each side carries its own marker, so the change is visible even
		// though neither value is printed.
		for _, want := range []string{
			`-      "SOME_SERVICE_TOKEN": "<redacted: before>"`,
			`+      "SOME_SERVICE_TOKEN": "<redacted: after>"`,
		} {
			if !strings.Contains(diff, want) {
				t.Errorf("the diff does not show the token change as %q:\n%s", want, diff)
			}
		}
	})
}

// daemonConfigOldBackups writes n copies of path beside it, aged one to n days
// before now, and returns their names oldest first.
func daemonConfigOldBackups(t *testing.T, path string, now time.Time, n int) []string {
	t.Helper()
	dir := filepath.Dir(path)
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("config.json.bak-20260101T000000Z-%02d", i)
		names = append(names, name)
		full := filepath.Join(dir, name)
		if err := os.WriteFile(full, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(-time.Duration(n-i) * 24 * time.Hour)
		if err := os.Chtimes(full, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	return names
}

func TestConfigDaemonPrunesOldBackups(t *testing.T) {
	t.Parallel()

	now := time.Now()
	townRoot, path := daemonConfigTown(t, daemonConfigFixture)
	env, _, errOut := daemonConfigTestEnv(townRoot, now)

	// Sixty copies aged one to sixty days. The write makes the sixty-first and
	// newest, so the retention keeps the newest fifty and removes the eleven
	// oldest; those are also past the 30-day window.
	dir := filepath.Dir(path)
	old := daemonConfigOldBackups(t, path, now, 60)
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherCopy := filepath.Join(dir, "daemon.json.bak-20260101T000000Z-old")
	if err := os.WriteFile(otherCopy, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := configDaemonSet(env, "cli_theme", `"light"`, "theme"); err != nil {
		t.Fatalf("set cli_theme = %v", err)
	}

	// The write's own copy is the newest 1 of the 61; the newest 49 old
	// copies fill the 50, and the 11 oldest go.
	remaining := daemonConfigBackups(t, path)
	if len(remaining) != 50 {
		t.Fatalf("backups after the write = %d, want 50: %v", len(remaining), remaining)
	}
	for _, name := range old[:11] {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s survived, want it pruned", name)
		}
	}
	lines := strings.Split(strings.TrimSpace(errOut.String()), "\n")
	if len(lines) != 11 {
		t.Fatalf("removal lines = %d, want one per removal:\n%s", len(lines), errOut.String())
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "removed backup ") {
			t.Errorf("removal line = %q, want it to name the backup", line)
		}
	}
	if data, err := os.ReadFile(notes); err != nil || string(data) != "keep" {
		t.Errorf("notes.txt = %q, %v; want it untouched", data, err)
	}
	if data, err := os.ReadFile(otherCopy); err != nil || string(data) != "keep" {
		t.Errorf("another file's backup = %q, %v; want it untouched", data, err)
	}
}

// A write that fails after its backup prunes nothing: no older copy is removed
// for a change that did not land (gt-yjzepk.4).
func TestConfigDaemonFailedWritePrunesNothing(t *testing.T) {
	t.Parallel()

	now := time.Now()
	townRoot, path := daemonConfigTown(t, daemonConfigFixture)
	env, _, _ := daemonConfigTestEnv(townRoot, now)
	old := daemonConfigOldBackups(t, path, now, 60)
	env.update = func(path string, perm os.FileMode, mutate func(*json.RawMessage, bool) error) error {
		return config.UpdateConfigJSONNoWait(path, perm, func(cur *json.RawMessage, exists bool) error {
			if err := mutate(cur, exists); err != nil {
				return err
			}
			return errors.New("the write failed")
		})
	}

	err := configDaemonSet(env, "cli_theme", `"light"`, "theme")
	if err == nil || !strings.Contains(err.Error(), "the write failed") {
		t.Fatalf("set with a failing write = %v, want the write's error", err)
	}
	for _, name := range old {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); err != nil {
			t.Errorf("the failed write removed %s: %v", name, err)
		}
	}
	if backups := daemonConfigBackups(t, path); len(backups) != len(old) {
		t.Errorf("backups after the failed write = %d, want the %d that were there", len(backups), len(old))
	}
}

// daemonConfigPathFixture holds a list and no polecat_pool, so a test drives a
// list index and a parent 'set' creates.
const daemonConfigPathFixture = `{
  "type": "town-settings",
  "version": 1,
  "disabled_patrols": ["doctor_dog", "compactor_dog"]
}
`

func TestConfigDaemonGetPrintsJSON(t *testing.T) {
	t.Parallel()

	t.Run("a path prints its value", func(t *testing.T) {
		t.Parallel()
		townRoot, _ := daemonConfigTown(t, daemonConfigPathFixture)
		env, out, _ := daemonConfigTestEnv(townRoot, time.Now())

		if err := configDaemonGet(env, []string{"disabled_patrols.0"}); err != nil {
			t.Fatalf("get = %v", err)
		}
		if got := out.String(); got != "\"doctor_dog\"\n" {
			t.Errorf("get = %q, want the value as JSON", got)
		}
	})

	t.Run("no path prints the whole document", func(t *testing.T) {
		t.Parallel()
		townRoot, _ := daemonConfigTown(t, daemonConfigPathFixture)
		env, out, _ := daemonConfigTestEnv(townRoot, time.Now())

		if err := configDaemonGet(env, nil); err != nil {
			t.Fatalf("get = %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("get printed %q, want a JSON document: %v", out.String(), err)
		}
		if doc["type"] != "town-settings" {
			t.Errorf("type = %v, want town-settings", doc["type"])
		}
	})

	t.Run("a missing path is refused", func(t *testing.T) {
		t.Parallel()
		townRoot, _ := daemonConfigTown(t, daemonConfigPathFixture)
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())

		err := configDaemonGet(env, []string{"no.such"})
		if err == nil || !strings.Contains(err.Error(), `no key "no"`) {
			t.Fatalf("get of a missing path = %v, want the missing key named", err)
		}
	})

	t.Run("a file the daemon refuses is not read", func(t *testing.T) {
		t.Parallel()
		townRoot, _ := daemonConfigTown(t, `{"type": "town-settings", "version": 99}`)
		env, out, _ := daemonConfigTestEnv(townRoot, time.Now())

		err := configDaemonGet(env, nil)
		if err == nil || !strings.Contains(err.Error(), "unsupported config version") {
			t.Fatalf("get of a document the daemon refuses = %v, want it refused", err)
		}
		if out.Len() != 0 {
			t.Errorf("get printed %q, want nothing", out.String())
		}
	})
}

func TestConfigDaemonPaths(t *testing.T) {
	t.Parallel()

	t.Run("set creates a missing parent", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigPathFixture)
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())

		if err := configDaemonSet(env, "polecat_pool.max_total", "3", "pool"); err != nil {
			t.Fatalf("set = %v", err)
		}
		pool, ok := daemonConfigDocument(t, path)["polecat_pool"].(map[string]any)
		if !ok || pool["max_total"] != float64(3) {
			t.Errorf("polecat_pool = %v, want the created parent holding 3", pool)
		}
	})

	t.Run("unset removes a list element", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigPathFixture)
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())

		if err := configDaemonUnset(env, "disabled_patrols.0", "patrols"); err != nil {
			t.Fatalf("unset = %v", err)
		}
		got := daemonConfigDocument(t, path)["disabled_patrols"].([]any)
		if len(got) != 1 || got[0] != "compactor_dog" {
			t.Errorf("disabled_patrols = %v, want only compactor_dog", got)
		}
	})

	t.Run("set creates the file when the town has none", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())

		if err := configDaemonSet(env, "cli_theme", `"dark"`, "theme"); err != nil {
			t.Fatalf("set without a file = %v", err)
		}
		path := config.TownSettingsPath(townRoot)
		if got := daemonConfigDocument(t, path)["cli_theme"]; got != "dark" {
			t.Errorf("cli_theme = %v, want dark", got)
		}
		if backups := daemonConfigBackups(t, path); len(backups) != 0 {
			t.Errorf("backups for a file that did not exist = %v, want none", backups)
		}
	})

	t.Run("unset of a missing path is refused", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigPathFixture)
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		err = configDaemonUnset(env, "disabled_patrols.9", "patrols")
		if err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("unset past the list = %v, want it refused", err)
		}
		if after, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(after, before) {
			t.Errorf("file after the refused unset = %q, %v; want it unchanged", after, readErr)
		}
	})
}

func TestConfigDaemonSetFromStdin(t *testing.T) {
	t.Parallel()

	townRoot, path := daemonConfigTown(t, daemonConfigFixture)
	env, _, _ := daemonConfigTestEnv(townRoot, time.Now())
	env.in = strings.NewReader(`{"GT_A": "1"}`)

	if err := configDaemonSet(env, "daemon.env", "-", "env"); err != nil {
		t.Fatalf("set from stdin = %v", err)
	}
	daemonSection, ok := daemonConfigDocument(t, path)["daemon"].(map[string]any)
	if !ok {
		t.Fatal("the document lost its daemon section")
	}
	got, ok := daemonSection["env"].(map[string]any)
	if !ok || got["GT_A"] != "1" {
		t.Errorf("daemon.env = %v, want GT_A=1 from standard input", got)
	}
}

func TestConfigDaemonDiff(t *testing.T) {
	t.Parallel()

	t.Run("prints the last write's change", func(t *testing.T) {
		t.Parallel()
		townRoot, _ := daemonConfigTown(t, daemonConfigFixture)
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())
		if err := configDaemonSet(env, "default_agent", `"claude-haiku"`, "switch"); err != nil {
			t.Fatalf("set = %v", err)
		}

		env.out = new(bytes.Buffer)
		if err := configDaemonDiff(env); err != nil {
			t.Fatalf("diff = %v", err)
		}
		diff := env.out.(*bytes.Buffer).String()
		if !strings.Contains(diff, `-  "default_agent": "claude",`) || !strings.Contains(diff, `+  "default_agent": "claude-haiku",`) {
			t.Errorf("diff = %q, want the last change", diff)
		}
	})

	t.Run("refuses a town with no backup", func(t *testing.T) {
		t.Parallel()
		townRoot, _ := daemonConfigTown(t, daemonConfigFixture)
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())

		err := configDaemonDiff(env)
		if err == nil || !strings.Contains(err.Error(), "no change has been written") {
			t.Fatalf("diff with no backup = %v, want it refused", err)
		}
	})
}

// daemonConfigFailingReader fails every read, for the value set reads from standard input.
type daemonConfigFailingReader struct{}

func (daemonConfigFailingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestConfigDaemonNeedsTheTown(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	for name, run := range map[string]func(configDaemonEnv) error{
		"get":   func(e configDaemonEnv) error { return configDaemonGet(e, nil) },
		"set":   func(e configDaemonEnv) error { return configDaemonSet(e, "a", "1", "label") },
		"unset": func(e configDaemonEnv) error { return configDaemonUnset(e, "a", "label") },
		"diff":  configDaemonDiff,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, _, _ := daemonConfigTestEnv(townRoot, time.Now())
			env.findTown = func() (string, error) { return "", errors.New("no town here") }
			if err := run(env); err == nil || !strings.Contains(err.Error(), "finding town root") {
				t.Fatalf("%s without a town = %v, want the town lookup's error", name, err)
			}
		})
	}
}

func TestConfigDaemonRefusesABadValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		arg   string
		stdin io.Reader
		want  string
	}{
		{name: "text that is not JSON", arg: "{not json", want: "the value is not JSON"},
		{name: "two JSON values", arg: `1 2`, want: "not one JSON value"},
		{name: "standard input that cannot be read", arg: "-", stdin: daemonConfigFailingReader{}, want: "reading the value from standard input"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			townRoot, path := daemonConfigTown(t, daemonConfigFixture)
			env, _, _ := daemonConfigTestEnv(townRoot, time.Now())
			if c.stdin != nil {
				env.in = c.stdin
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			err = configDaemonSet(env, "cli_theme", c.arg, "theme")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("set %q = %v, want %q", c.arg, err, c.want)
			}
			if after, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(after, before) {
				t.Errorf("file after the refused write = %q, %v; want it unchanged", after, readErr)
			}
		})
	}
}

func TestConfigDaemonRefusesAFileItCannotRead(t *testing.T) {
	t.Parallel()

	t.Run("a town with no settings file", func(t *testing.T) {
		t.Parallel()
		env, _, _ := daemonConfigTestEnv(t.TempDir(), time.Now())
		if err := configDaemonGet(env, nil); err == nil {
			t.Fatal("get without a settings file = nil, want the read's error")
		}
	})

	t.Run("a document that is not an object", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, `[1, 2]`)
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())

		err := configDaemonSet(env, "cli_theme", `"dark"`, "theme")
		if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
			t.Fatalf("set in a list document = %v, want it refused", err)
		}
		if got := string(daemonConfigMustRead(t, path)); got != "[1, 2]" {
			t.Errorf("file after the refused write = %q, want it unchanged", got)
		}
	})
}

// daemonConfigMustRead reads path, failing the test when it cannot.
func daemonConfigMustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestConfigDaemonPathRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		op   string
		path string
		want string
	}{
		{name: "get an empty path", op: "get", path: "", want: "empty path"},
		{name: "get an empty segment", op: "get", path: "a..b", want: "empty segment"},
		{name: "get through a scalar", op: "get", path: "type.x", want: "not an object or a list"},
		{name: "get a list by key", op: "get", path: "disabled_patrols.first", want: "is not an index"},
		{name: "get past the list", op: "get", path: "disabled_patrols.9", want: "out of range"},
		{name: "get a missing key", op: "get", path: "nope", want: `no key "nope"`},
		{name: "set an empty path", op: "set", path: "", want: "empty path"},
		{name: "set through a scalar", op: "set", path: "type.x", want: "not an object or a list"},
		{name: "set a list by key", op: "set", path: "disabled_patrols.first", want: "is not an index"},
		{name: "set past the list", op: "set", path: "disabled_patrols.9", want: "out of range"},
		{name: "set past a list in the middle", op: "set", path: "disabled_patrols.9.x", want: "out of range"},
		{name: "unset an empty path", op: "unset", path: "", want: "empty path"},
		{name: "unset a missing key", op: "unset", path: "nope", want: `no key "nope"`},
		{name: "unset under a missing parent", op: "unset", path: "nope.x", want: `no key "nope"`},
		{name: "unset through a scalar", op: "unset", path: "type.x", want: "not an object or a list"},
		{name: "unset a list by key", op: "unset", path: "disabled_patrols.first", want: "is not an index"},
		{name: "unset past the list", op: "unset", path: "disabled_patrols.9", want: "out of range"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			townRoot, path := daemonConfigTown(t, daemonConfigPathFixture)
			env, _, _ := daemonConfigTestEnv(townRoot, time.Now())
			before := daemonConfigMustRead(t, path)

			var err error
			switch c.op {
			case "get":
				err = configDaemonGet(env, []string{c.path})
			case "set":
				err = configDaemonSet(env, c.path, `"x"`, "test")
			case "unset":
				err = configDaemonUnset(env, c.path, "test")
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s %q = %v, want %q", c.op, c.path, err, c.want)
			}
			if after := daemonConfigMustRead(t, path); !bytes.Equal(after, before) {
				t.Errorf("file after the refused %s = %q, want it unchanged", c.op, after)
			}
		})
	}
}

func TestConfigDaemonDiffRefusals(t *testing.T) {
	t.Parallel()

	t.Run("a settings directory it cannot read", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(townRoot, "settings"), []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())
		if err := configDaemonDiff(env); err == nil || !strings.Contains(err.Error(), "reading ") {
			t.Fatalf("diff over a settings path that is not a directory = %v, want the read's error", err)
		}
	})

	t.Run("a backup that does not parse", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigFixture)
		if err := os.WriteFile(path+".bak-20260101T000000Z-bad", []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())
		if err := configDaemonDiff(env); err == nil || !strings.Contains(err.Error(), "reading the last backup") {
			t.Fatalf("diff over a broken backup = %v, want it refused", err)
		}
	})

	t.Run("a current document that does not load", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, "{")
		if err := os.WriteFile(path+".bak-20260101T000000Z-good", []byte(daemonConfigFixture), 0o600); err != nil {
			t.Fatal(err)
		}
		env, _, _ := daemonConfigTestEnv(townRoot, time.Now())
		if err := configDaemonDiff(env); err == nil {
			t.Fatal("diff over a current document that does not load = nil, want it refused")
		}
	})

	t.Run("nothing changed since the backup", func(t *testing.T) {
		t.Parallel()
		townRoot, path := daemonConfigTown(t, daemonConfigFixture)
		if err := os.WriteFile(path+".bak-20260101T000000Z-copy", []byte(daemonConfigFixture), 0o600); err != nil {
			t.Fatal(err)
		}
		env, out, _ := daemonConfigTestEnv(townRoot, time.Now())
		if err := configDaemonDiff(env); err != nil {
			t.Fatalf("diff over an unchanged document = %v", err)
		}
		if !strings.Contains(out.String(), "is unchanged since") {
			t.Errorf("diff = %q, want it to say nothing changed", out.String())
		}
	})
}

// The command reference has a gt config daemon section that names every
// subcommand, the required --label, the lock rule and the argv rule (gt-yjzepk.4).
func TestReferenceDocsCoverTheConfigDaemonCommand(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	const heading = "#### gt config daemon"
	start := strings.Index(doc, heading)
	if start < 0 {
		t.Fatalf("docs/reference.md has no %q section", heading)
	}
	section := doc[start:]
	if end := strings.Index(section, "\n### "); end >= 0 {
		section = section[:end]
	}
	for _, want := range []string{
		"gt config daemon get", "gt config daemon set", "gt config daemon unset", "gt config daemon diff",
		"--label", "config.json.lock", "refused, not queued", "argv", "process list",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the gt config daemon section does not name %q:\n%s", want, section)
		}
	}
}

func TestConfigDaemonRefusesAnExhaustedBackupName(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	townRoot, path := daemonConfigTown(t, daemonConfigFixture)
	env, _, _ := daemonConfigTestEnv(townRoot, now)
	base := path + ".bak-20261108T120000.000000000Z-theme"
	for i := 0; i < 101; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i+1)
		}
		if err := os.WriteFile(name, []byte("taken"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := daemonConfigMustRead(t, path)

	err := configDaemonSet(env, "cli_theme", `"light"`, "theme")
	if err == nil || !strings.Contains(err.Error(), "already exist") {
		t.Fatalf("set with every backup name taken = %v, want it refused", err)
	}
	if after := daemonConfigMustRead(t, path); !bytes.Equal(after, before) {
		t.Errorf("file after the refused write = %q, want it unchanged", after)
	}
}
