package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
)

// profileTestSettings is a town's settings with one patrol on, one already off,
// one with no enabled key, and the container supervisor set.
const profileTestSettings = `{
  "daemon": {
    "patrols": {
      "landing_worker": {"enabled": true},
      "tier_sweep": {"enabled": false},
      "wisp_reaper": {}
    },
    "supervisor": "container"
  }
}
`

// The failures a write test makes happen, one per operation.
var (
	errProfileRead   = errors.New("read refused")
	errProfileOpen   = errors.New("open refused")
	errProfileWrite  = errors.New("write refused")
	errProfileClose  = errors.New("close refused")
	errProfileRename = errors.New("rename refused")
)

// profileTestDaemonDoc is a five-file town's mayor/daemon.json: the daemon object
// itself, with one patrol on and the container supervisor set.
const profileTestDaemonDoc = `{
  "patrols": {
    "landing_worker": {"enabled": true},
    "tier_sweep": {"enabled": false}
  },
  "supervisor": "container",
  "type": "daemon-patrol-config",
  "version": 1
}
`

// inPod is the environment of a pod: the variable Kubernetes sets is non-empty.
func inPod(string) string { return "10.43.0.1" }

// outsidePod is the environment of the Mac: the pod variable is unset.
func outsidePod(string) string { return "" }

// writeProfileTown writes settings to a fresh town root and returns the root and
// the settings path.
func writeProfileTown(t *testing.T, settings string) (town, path string) {
	t.Helper()
	town = t.TempDir()
	path = config.TownSettingsPath(town)
	writeProfileFile(t, path, settings)
	return town, path
}

// readProfileFile returns the bytes of path, failing the test when it is unreadable.
func readProfileFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// newProfileEnv is a rehearsal run in a pod over the host filesystem, writing
// its output to out.
func newProfileEnv(town string, out io.Writer) townStateProfileEnv {
	return townStateProfileEnv{town: town, rehearsal: true, getenv: inPod, files: hostProfileFiles(), readPatrols: daemon.ReadPatrolConfig, out: out}
}

// faultFile is a file whose Write or Close fails with the given error.
type faultFile struct {
	io.WriteCloser
	writeErr, closeErr error
}

func (f faultFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.WriteCloser.Write(p)
}

func (f faultFile) Close() error {
	err := f.WriteCloser.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

// wrapFile puts wrap around the handle that opening name returns.
func wrapFile(files profileFiles, name string, wrap func(io.WriteCloser) io.WriteCloser) profileFiles {
	open := files.open
	files.open = func(n string, flag int, perm fs.FileMode) (io.WriteCloser, error) {
		f, err := open(n, flag, perm)
		if n != name {
			return f, err
		}
		return wrap(f), err
	}
	return files
}

// failOpen makes opening name fail.
func failOpen(files profileFiles, name string) profileFiles {
	open := files.open
	files.open = func(n string, flag int, perm fs.FileMode) (io.WriteCloser, error) {
		if n == name {
			return nil, errProfileOpen
		}
		return open(n, flag, perm)
	}
	return files
}

// failWrite makes the writes to name fail.
func failWrite(files profileFiles, name string) profileFiles {
	return wrapFile(files, name, func(f io.WriteCloser) io.WriteCloser {
		return faultFile{WriteCloser: f, writeErr: errProfileWrite}
	})
}

// failClose makes closing name fail.
func failClose(files profileFiles, name string) profileFiles {
	return wrapFile(files, name, func(f io.WriteCloser) io.WriteCloser {
		return faultFile{WriteCloser: f, closeErr: errProfileClose}
	})
}

func TestTownStateProfileFlagReadsRefuseAnUndefinedFlag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		defined []string
		want    string
	}{
		{name: "without --town", want: "reading --town"},
		{name: "without --rehearsal", defined: []string{"town"}, want: "reading --rehearsal"},
		{name: "without --check", defined: []string{"town", "rehearsal"}, want: "reading --check"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			for _, name := range tc.defined {
				if name == "town" {
					cmd.Flags().String(name, "", "")
				} else {
					cmd.Flags().Bool(name, false, "")
				}
			}
			err := runTownStateProfile(cmd, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownStateProfile = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

func TestTownStateProfileRefusesOutsideAPod(t *testing.T) {
	t.Parallel()
	town, path := writeProfileTown(t, profileTestSettings)
	env := newProfileEnv(town, &bytes.Buffer{})
	env.getenv = outsidePod
	err := runTownStateProfileIn(env)
	if err == nil || !strings.Contains(err.Error(), podEnv) {
		t.Fatalf("profile outside a pod = %v, want a refusal naming %s", err, podEnv)
	}
	if got := readProfileFile(t, path); string(got) != profileTestSettings {
		t.Errorf("the settings changed outside a pod:\n%s", got)
	}
	if _, err := os.Stat(path + rehearsalBackupSuffix); !os.IsNotExist(err) {
		t.Errorf("a backup was written outside a pod: stat = %v", err)
	}
}

func TestTownStateProfileCheckAndWrite(t *testing.T) {
	t.Parallel()
	town, path := writeProfileTown(t, profileTestSettings)
	var checkOut bytes.Buffer
	env := newProfileEnv(town, &checkOut)

	// --check on the live settings: exit 1, each patrol still on and the
	// supervisor named, and nothing written.
	env.check = true
	err := runTownStateProfileIn(env)
	var exitErr *ExitCodeError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("--check on live settings = %v, want exit 1", err)
	}
	for _, want := range []string{"daemon.patrols.landing_worker.enabled", "daemon.patrols.wisp_reaper.enabled", "daemon.supervisor"} {
		if !strings.Contains(checkOut.String(), want) {
			t.Errorf("--check output %q does not list %s", checkOut.String(), want)
		}
	}
	if got := readProfileFile(t, path); string(got) != profileTestSettings {
		t.Fatalf("--check wrote the settings:\n%s", got)
	}

	// The write: the old file is kept as the backup and the settings are profiled.
	env.check = false
	if err := runTownStateProfileIn(env); err != nil {
		t.Fatalf("write: %v", err)
	}
	backup := path + rehearsalBackupSuffix
	if got := readProfileFile(t, backup); string(got) != profileTestSettings {
		t.Fatalf("the backup is not the old settings:\n%s", got)
	}
	var profiled map[string]any
	if err := json.Unmarshal(readProfileFile(t, path), &profiled); err != nil {
		t.Fatalf("the written settings are not JSON: %v", err)
	}
	daemon := profiled["daemon"].(map[string]any)
	if _, ok := daemon["supervisor"]; ok {
		t.Errorf("the written settings kept daemon.supervisor")
	}
	for name, p := range daemon["patrols"].(map[string]any) {
		if p.(map[string]any)["enabled"] != false {
			t.Errorf("patrol %s is not disabled in the written settings", name)
		}
	}

	// A second --check on the profiled settings: exit 0.
	env.check = true
	if err := runTownStateProfileIn(env); err != nil {
		t.Fatalf("second --check = %v, want exit 0", err)
	}
}

// TestTownStateProfileProfilesTheDaemonFileOnAFiveFileTown: on the five-file
// layout the daemon reads its patrols from mayor/daemon.json, not from
// settings/config.json, so the profile must rewrite that file.
func TestTownStateProfileProfilesTheDaemonFileOnAFiveFileTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	daemonPath := config.DaemonPatrolConfigPath(town)
	writeProfileFile(t, daemonPath, profileTestDaemonDoc)
	const settings = `{"agents": {}}` + "\n"
	settingsPath := config.TownSettingsPath(town)
	writeProfileFile(t, settingsPath, settings)
	var checkOut bytes.Buffer
	env := newProfileEnv(town, &checkOut)

	env.check = true
	err := runTownStateProfileIn(env)
	var exitErr *ExitCodeError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("--check on a five-file town = %v, want exit 1", err)
	}
	for _, want := range []string{daemonPath + ": daemon.patrols.landing_worker.enabled", daemonPath + ": daemon.supervisor"} {
		if !strings.Contains(checkOut.String(), want) {
			t.Errorf("--check output %q does not list %q", checkOut.String(), want)
		}
	}

	env.check = false
	if err := runTownStateProfileIn(env); err != nil {
		t.Fatalf("write: %v", err)
	}
	env.check = true
	if err := runTownStateProfileIn(env); err != nil {
		t.Fatalf("second --check on the five-file town = %v, want exit 0", err)
	}
	env.check = false
	if got := readProfileFile(t, daemonPath+rehearsalBackupSuffix); string(got) != profileTestDaemonDoc {
		t.Fatalf("the backup is not the old daemon document:\n%s", got)
	}
	var profiled map[string]any
	if err := json.Unmarshal(readProfileFile(t, daemonPath), &profiled); err != nil {
		t.Fatalf("the written daemon document is not JSON: %v", err)
	}
	if _, ok := profiled["supervisor"]; ok {
		t.Errorf("the written daemon document kept supervisor")
	}
	for name, p := range profiled["patrols"].(map[string]any) {
		if p.(map[string]any)["enabled"] != false {
			t.Errorf("patrol %s is not disabled in the written daemon document", name)
		}
	}
	if got := readProfileFile(t, settingsPath); string(got) != settings {
		t.Errorf("the settings file changed on a five-file town:\n%s", got)
	}
}

// TestTownStateProfileCheckRefusesWhenTheDaemonLoadsNoConfig: a loader that
// reports no patrol config at all is a refusal: the daemon would run its
// defaults, so --check must not report the town clean.
func TestTownStateProfileCheckRefusesWhenTheDaemonLoadsNoConfig(t *testing.T) {
	t.Parallel()
	town, _ := writeProfileTown(t, profileTestSettings)
	env := newProfileEnv(town, &bytes.Buffer{})
	env.check = true
	env.readPatrols = func(string) (*daemon.DaemonPatrolConfig, error) { return nil, nil }
	err := runTownStateProfileIn(env)
	var exitErr *ExitCodeError
	if err == nil || errors.As(err, &exitErr) || !strings.Contains(err.Error(), "no patrol config") {
		t.Fatalf("--check when the daemon loads no config = %v, want a refusal (not an exit code)", err)
	}
}

// TestTownStateProfileCheckRefusesADaemonFileTheDaemonCannotLoad: --check reads
// the file back through the daemon's loader, so a file that loader rejects is
// refused rather than reported as clean.
func TestTownStateProfileCheckRefusesADaemonFileTheDaemonCannotLoad(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	daemonPath := config.DaemonPatrolConfigPath(town)
	writeProfileFile(t, daemonPath, strings.Replace(profileTestDaemonDoc, "daemon-patrol-config", "daemon", 1))
	env := newProfileEnv(town, &bytes.Buffer{})
	env.check = true
	err := runTownStateProfileIn(env)
	var exitErr *ExitCodeError
	if err == nil || errors.As(err, &exitErr) || !strings.Contains(err.Error(), "reading the daemon patrol config back") {
		t.Fatalf("--check on a file the daemon cannot load = %v, want a refusal (not an exit code)", err)
	}
}

func TestTownStateProfileRefusesAnUnreadableDaemonFile(t *testing.T) {
	t.Parallel()
	town, path := writeProfileTown(t, profileTestSettings)
	env := newProfileEnv(town, &bytes.Buffer{})
	env.files.read = func(string) ([]byte, error) { return nil, errProfileRead }
	err := runTownStateProfileIn(env)
	if !errors.Is(err, errProfileRead) {
		t.Fatalf("profile of an unreadable file = %v, want the read error", err)
	}
	if got := readProfileFile(t, path); string(got) != profileTestSettings {
		t.Errorf("an unreadable file's settings changed:\n%s", got)
	}
}

func TestTownStateProfileWritesNothingWhenAlreadyProfiled(t *testing.T) {
	t.Parallel()
	town, path := writeProfileTown(t, profileTestSettings)
	env := newProfileEnv(town, &bytes.Buffer{})
	if err := runTownStateProfileIn(env); err != nil {
		t.Fatalf("first write: %v", err)
	}
	profiled := readProfileFile(t, path)

	if err := runTownStateProfileIn(env); err != nil {
		t.Fatalf("second write = %v, want nothing to do", err)
	}
	if got := readProfileFile(t, path); !bytes.Equal(got, profiled) {
		t.Errorf("a write with nothing to change rewrote the settings")
	}
	// The backup is still the first run's original, not the profiled copy.
	backup := path + rehearsalBackupSuffix
	if got := readProfileFile(t, backup); string(got) != profileTestSettings {
		t.Errorf("the backup was overwritten by a later run:\n%s", got)
	}
}

func TestTownStateProfileRefusesAnExistingBackup(t *testing.T) {
	t.Parallel()
	town, path := writeProfileTown(t, profileTestSettings)
	backup := path + rehearsalBackupSuffix
	if err := os.WriteFile(backup, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runTownStateProfileIn(newProfileEnv(town, &bytes.Buffer{}))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("profile over an existing backup = %v, want a refusal", err)
	}
	if got := readProfileFile(t, path); string(got) != profileTestSettings {
		t.Errorf("the settings changed under an existing backup:\n%s", got)
	}
	if got := readProfileFile(t, backup); string(got) != "original" {
		t.Errorf("the existing backup was overwritten: %q", got)
	}
}

func TestTownStateProfileRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		rehearsal bool
		setup     func(t *testing.T, town, path string)
		want      string
	}{
		{
			name: "without --rehearsal", rehearsal: false,
			setup: func(t *testing.T, town, path string) { writeProfileFile(t, path, profileTestSettings) },
			want:  "needs --rehearsal",
		},
		{
			name: "without a settings file", rehearsal: true,
			setup: func(t *testing.T, town, path string) {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "reading the town settings",
		},
		{
			name: "a settings path that is a directory", rehearsal: true,
			setup: func(t *testing.T, town, path string) {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "reading the town settings",
		},
		{
			name: "settings without a patrols object", rehearsal: true,
			setup: func(t *testing.T, town, path string) { writeProfileFile(t, path, `{"daemon":{}}`) },
			want:  "no daemon.patrols object",
		},
		{
			name: "a malformed settings file", rehearsal: true,
			setup: func(t *testing.T, town, path string) { writeProfileFile(t, path, `{"daemon":`) },
			want:  "does not parse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			path := config.TownSettingsPath(town)
			tc.setup(t, town, path)
			env := newProfileEnv(town, &bytes.Buffer{})
			env.rehearsal = tc.rehearsal
			err := runTownStateProfileIn(env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("profile = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// writeProfileFile writes content to path, creating its directory.
func writeProfileFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTownStateProfileKeepsTheSettingsWhenAWriteFails: each step of the write
// that can fail is made to fail, and the settings are left as they were, with
// no temporary file beside them.
func TestTownStateProfileKeepsTheSettingsWhenAWriteFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		files func(host profileFiles, backup, tmp string) profileFiles
		want  string
	}{
		{
			name:  "the backup cannot be created",
			files: func(h profileFiles, backup, tmp string) profileFiles { return failOpen(h, backup) },
			want:  "writing the rehearsal backup",
		},
		{
			name:  "the backup write fails",
			files: func(h profileFiles, backup, tmp string) profileFiles { return failWrite(h, backup) },
			want:  "writing the rehearsal backup",
		},
		{
			name:  "the backup close fails",
			files: func(h profileFiles, backup, tmp string) profileFiles { return failClose(h, backup) },
			want:  "writing the rehearsal backup",
		},
		{
			name:  "the temporary file cannot be created",
			files: func(h profileFiles, backup, tmp string) profileFiles { return failOpen(h, tmp) },
			want:  "writing the rehearsal settings",
		},
		{
			name:  "the temporary file write fails",
			files: func(h profileFiles, backup, tmp string) profileFiles { return failWrite(h, tmp) },
			want:  "writing the rehearsal settings",
		},
		{
			name:  "the temporary file close fails",
			files: func(h profileFiles, backup, tmp string) profileFiles { return failClose(h, tmp) },
			want:  "writing the rehearsal settings",
		},
		{
			name: "the rename fails",
			files: func(h profileFiles, backup, tmp string) profileFiles {
				h.rename = func(string, string) error { return errProfileRename }
				return h
			},
			want: "replacing the town settings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, path := writeProfileTown(t, profileTestSettings)
			backup := path + rehearsalBackupSuffix
			tmp := path + rehearsalTempSuffix
			env := newProfileEnv(town, &bytes.Buffer{})
			env.files = tc.files(hostProfileFiles(), backup, tmp)
			err := runTownStateProfileIn(env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("profile = %v, want a refusal containing %q", err, tc.want)
			}
			if got := readProfileFile(t, path); string(got) != profileTestSettings {
				t.Errorf("a failed write changed the settings:\n%s", got)
			}
			if _, err := os.Stat(tmp); !os.IsNotExist(err) {
				t.Errorf("a failed write left its temporary file: stat = %v", err)
			}
		})
	}
}
