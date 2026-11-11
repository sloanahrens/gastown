package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	gtgit "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// townStateTree is a fake town: files the backup exports, files it refuses, and
// files it must neither export nor refuse.
func townStateTree() fstest.MapFS {
	return fstest.MapFS{
		"mayor/town.json":                   {Data: []byte(`{"name":"town"}`)},
		"mayor/rigs.json":                   {Data: []byte(`{"rigs":{}}`)},
		"settings/config.json":              {Data: []byte(`{"daemon":{}}`)},
		"settings/models.json":              {Data: []byte(`{}`)},
		"settings/placement.json":           {Data: []byte(`{}`)},
		"settings/regime-events.json":       {Data: []byte(`[]`)},
		"settings/themes/dark.json":         {Data: []byte(`{"bg":"#000"}`)},
		"settings/themes/light.json":        {Data: []byte(`{"bg":"#fff"}`)},
		"settings/themes/nested/x.json":     {Data: []byte(`{}`)},
		"settings/notes.txt":                {Data: []byte("not configuration")},
		"settings/daemon.env":               {Data: []byte("TOKEN=secret")},
		"settings/themes/ci.key":            {Data: []byte("key")},
		"settings/themes/token.json":        {Data: []byte(`{}`)},
		"gastown/settings/config.json":      {Data: []byte(`{"rig":true}`)},
		"gastown/.beads/config.yaml":        {Data: []byte("prefix: gt\n")},
		"gastown/.beads/notes.md":           {Data: []byte("not exported")},
		"gastown/polecats/dag/x.json":       {Data: []byte(`{}`)},
		"unregistered/settings/config.json": {Data: []byte(`{}`)},
	}
}

// TestTownStateBackupExportSetAndRefusals asserts the exact set exported and the
// exact set refused from one fake town.
func TestTownStateBackupExportSetAndRefusals(t *testing.T) {
	t.Parallel()
	snap, err := collectTownState(townStateTree(), []string{"gastown"})
	if err != nil {
		t.Fatalf("collectTownState: %v", err)
	}

	wantExported := []string{
		"gastown/.beads/config.yaml",
		"gastown/settings/config.json",
		"mayor/town.json",
		"settings/config.json",
		"settings/models.json",
		"settings/placement.json",
		"settings/regime-events.json",
		"settings/themes/dark.json",
		"settings/themes/light.json",
	}
	if got := sortedFileKeys(snap.Files); !reflect.DeepEqual(got, wantExported) {
		t.Errorf("exported = %v\nwant      %v", got, wantExported)
	}

	wantRefused := []string{
		"settings/daemon.env",
		"settings/themes/ci.key",
		"settings/themes/token.json",
	}
	if !reflect.DeepEqual(snap.Refused, wantRefused) {
		t.Errorf("refused = %v\nwant    %v", snap.Refused, wantRefused)
	}

	if got := string(snap.Files["settings/themes/dark.json"]); got != `{"bg":"#000"}` {
		t.Errorf("dark.json content = %q, want the file as read", got)
	}
}

// TestTownStateBackupRefusesEachSecretPattern is one failing case per refused
// pattern, each placed where the walk would otherwise export a file.
func TestTownStateBackupRefusesEachSecretPattern(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		file string
	}{
		{name: "env file in settings", file: "settings/daemon.env"},
		{name: "env file in a themes directory", file: "settings/themes/brand.env"},
		{name: "key file in a themes directory", file: "settings/themes/ci.key"},
		{name: "token in a rig beads directory", file: "gastown/.beads/api_token.yaml"},
		{name: "token in a rig settings directory", file: "gastown/settings/token"},
		{name: "upper-case key file is still a key", file: "settings/themes/SIGNING.KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := townStateTree()
			tree[tc.file] = &fstest.MapFile{Data: []byte("sensitive")}
			snap, err := collectTownState(tree, []string{"gastown"})
			if err != nil {
				t.Fatalf("collectTownState: %v", err)
			}
			if _, ok := snap.Files[tc.file]; ok {
				t.Errorf("%s was exported; the %s pattern must refuse it", tc.file, tc.name)
			}
			if !containsString(snap.Refused, tc.file) {
				t.Errorf("refused = %v, want %s among them", snap.Refused, tc.file)
			}
		})
	}
}

// TestTownStateBackupReadErrorFailsSnapshot: a file or directory that cannot be
// read fails the whole snapshot. It is never read as absent, which would back
// up the file as gone.
func TestTownStateBackupReadErrorFailsSnapshot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fsys fs.FS
	}{
		{name: "unreadable export", fsys: failingFS{MapFS: townStateTree(), file: "settings/models.json"}},
		{name: "unreadable directory", fsys: failingFS{MapFS: townStateTree(), dir: "settings/themes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			snap, err := collectTownState(tc.fsys, []string{"gastown"})
			if err == nil {
				t.Fatalf("collectTownState returned %d files and no error; want the read failure", len(snap.Files))
			}
			if snap.Files != nil {
				t.Errorf("a failed snapshot still carries files: %v", sortedFileKeys(snap.Files))
			}
		})
	}
}

// TestTownStateBackupRefusesPartialTown: a town root without its identity file
// is an unmounted volume, not an empty town, so nothing is committed.
func TestTownStateBackupRefusesPartialTown(t *testing.T) {
	t.Parallel()
	tree := townStateTree()
	delete(tree, "mayor/town.json")
	if _, err := collectTownState(tree, []string{"gastown"}); err == nil {
		t.Fatal("collectTownState without mayor/town.json succeeded; want a refusal")
	}
}

// townStateDiskTown writes the export set under a temporary town root, plus a
// file outside it that a link could reach. MapFS cannot hold a symbolic link,
// so the symlink tests read a real directory.
func townStateDiskTown(t *testing.T) (town, outside string) {
	t.Helper()
	town = t.TempDir()
	outside = t.TempDir()
	for _, name := range []string{
		"mayor/town.json",
		"settings/config.json",
		"settings/models.json",
		"settings/placement.json",
		"settings/regime-events.json",
		"settings/themes/dark.json",
		"gastown/settings/config.json",
		"gastown/.beads/config.yaml",
	} {
		writeTownStateFile(t, filepath.Join(town, filepath.FromSlash(name)), "{}")
	}
	writeTownStateFile(t, filepath.Join(outside, "secret.json"), `{"token":"outside"}`)
	return town, outside
}

// townStateLink replaces the town-relative path with a symbolic link to target.
func townStateLink(t *testing.T, town, name, target string) {
	t.Helper()
	full := filepath.Join(town, filepath.FromSlash(name))
	if err := os.RemoveAll(full); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

// TestTownStateBackupRefusesSymlinksOutOfTheTown: a link on the way to an
// exported file, or the directory it is read from, fails the snapshot. The
// backup never reads the file the link points at.
func TestTownStateBackupRefusesSymlinksOutOfTheTown(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, town, outside string)
	}{
		{
			name: "exported file is a link",
			setup: func(t *testing.T, town, outside string) {
				townStateLink(t, town, "settings/models.json", filepath.Join(outside, "secret.json"))
			},
		},
		{
			name: "exported directory is a link",
			setup: func(t *testing.T, town, outside string) {
				townStateLink(t, town, "settings/themes", outside)
			},
		},
		{
			name: "directory on the way to an export is a link",
			setup: func(t *testing.T, town, outside string) {
				townStateLink(t, town, "settings", outside)
			},
		},
		{
			name: "rig directory is a link",
			setup: func(t *testing.T, town, outside string) {
				townStateLink(t, town, "gastown", outside)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, outside := townStateDiskTown(t)
			tc.setup(t, town, outside)
			snap, err := collectTownState(os.DirFS(town), []string{"gastown"})
			if err == nil {
				t.Fatalf("collectTownState returned %d files and no error; want the link refused", len(snap.Files))
			}
			if snap.Files != nil {
				t.Errorf("a refused snapshot still carries files: %v", sortedFileKeys(snap.Files))
			}
		})
	}
}

// TestTownStateBackupMissingRigDirectoryHoldsNothing: a registered rig with no
// directory on disk holds nothing to back up; it is not an error.
func TestTownStateBackupMissingRigDirectoryHoldsNothing(t *testing.T) {
	t.Parallel()
	snap, err := collectTownState(townStateTree(), []string{"gastown", "absent"})
	if err != nil {
		t.Fatalf("collectTownState: %v", err)
	}
	if _, ok := snap.Files["absent/settings/config.json"]; ok {
		t.Errorf("a missing rig directory exported a file")
	}
	if !containsString(sortedFileKeys(snap.Files), "gastown/settings/config.json") {
		t.Errorf("the present rig's configuration was not exported: %v", sortedFileKeys(snap.Files))
	}
}

// TestTownStateBackupIgnoresAnUnexportedLink: a link that no export row names
// is never read, so it does not fail the snapshot.
func TestTownStateBackupIgnoresAnUnexportedLink(t *testing.T) {
	t.Parallel()
	town, outside := townStateDiskTown(t)
	townStateLink(t, town, "settings/notes.txt", filepath.Join(outside, "secret.json"))
	snap, err := collectTownState(os.DirFS(town), []string{"gastown"})
	if err != nil {
		t.Fatalf("collectTownState: %v", err)
	}
	if _, ok := snap.Files["settings/notes.txt"]; ok {
		t.Errorf("an unexported link was exported")
	}
}

// TestTownStateBackupCorruptRegistryIsAnError: a rig registry that exists but
// does not load fails the tick. It is not zero rigs, which would back up no rig
// configuration and report the run clean.
func TestTownStateBackupCorruptRegistryIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: &Config{TownRoot: town}}
	if _, err := d.townStateRigNames(); err == nil {
		t.Fatal("townStateRigNames on a corrupt registry = nil error, want the read failure")
	}
}

// TestTownStateBackupRigNameStaysUnderTheRepository: a registry name that would
// step out of town-state/ is refused, not joined into a path.
func TestTownStateBackupRigNameStaysUnderTheRepository(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rig  string
	}{
		{name: "parent directory", rig: "../escape"},
		{name: "nested path", rig: "gastown/settings"},
		{name: "current directory", rig: "."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
				t.Fatal(err)
			}
			registry := `{"version":1,"rigs":{"` + tc.rig + `":{"git_url":"https://example.invalid/r.git"}}}`
			if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(registry), 0o644); err != nil {
				t.Fatal(err)
			}
			d := &Daemon{config: &Config{TownRoot: town}}
			if _, err := d.townStateRigNames(); err == nil {
				t.Fatalf("townStateRigNames accepted rig name %q; want a refusal", tc.rig)
			}
		})
	}
}

// TestTownStateBackupPatrolGate: the patrol is off without its config block, off
// when enabled is false, and on only with enabled true.
func TestTownStateBackupPatrolGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *DaemonPatrolConfig
		want bool
	}{
		{name: "nil config", cfg: nil, want: false},
		{name: "no patrols section", cfg: &DaemonPatrolConfig{}, want: false},
		{name: "block absent", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, want: false},
		{name: "enabled false", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{TownStateBackup: &TownStateBackupConfig{Enabled: false}}}, want: false},
		{name: "enabled true", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{TownStateBackup: &TownStateBackupConfig{Enabled: true}}}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsPatrolEnabled(tc.cfg, "town_state_backup"); got != tc.want {
				t.Errorf("IsPatrolEnabled = %v, want %v", got, tc.want)
			}
			d := &Daemon{config: &Config{TownRoot: t.TempDir()}, patrolConfig: tc.cfg}
			if got := d.isPatrolActive("town_state_backup"); got != tc.want {
				t.Errorf("isPatrolActive = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTownStateBackupInterval(t *testing.T) {
	t.Parallel()
	with := func(s string) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{Patrols: &PatrolsConfig{TownStateBackup: &TownStateBackupConfig{Enabled: true, IntervalStr: s}}}
	}
	cases := []struct {
		name string
		cfg  *DaemonPatrolConfig
		want time.Duration
	}{
		{name: "absent takes the hourly default", cfg: &DaemonPatrolConfig{}, want: time.Hour},
		{name: "empty string takes the default", cfg: with(""), want: time.Hour},
		{name: "configured", cfg: with("15m"), want: 15 * time.Minute},
		{name: "unparseable takes the default", cfg: with("soon"), want: time.Hour},
		{name: "non-positive takes the default", cfg: with("-5m"), want: time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := townStateBackupInterval(tc.cfg); got != tc.want {
				t.Errorf("interval = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTownStateBackupTwoTicks runs two ticks with no change and asserts one
// commit, then changes a theme and asserts a second commit that names it.
func TestTownStateBackupTwoTicks(t *testing.T) {
	t.Parallel()
	d, f, town := townStateDaemon(t)
	tree := townStateTree()
	d.townStateFS = func(string) fs.FS { return tree }
	gitRepo := filepath.Join(town, ".dolt-archive", "git")
	remote := seedTownStateOrigin(t, f, town, gitRepo)

	d.syncTownStateBackup()
	head1 := townStateHead(t, f, gitRepo)
	got := townStateSubjects(t, d, gitRepo)
	if len(got) != 1 || !strings.HasPrefix(got[0], "town-state: back up 9 changed file(s)") {
		t.Fatalf("after the first tick, town-state subjects = %q; want one commit naming 9 files", got)
	}
	if tip := f.Ref(remote, "refs/heads/main"); tip != head1 {
		t.Errorf("origin main = %s, want the first backup commit %s", tip, head1)
	}

	d.syncTownStateBackup()
	if head2 := townStateHead(t, f, gitRepo); head2 != head1 {
		t.Errorf("a tick with no change moved HEAD from %s to %s", head1, head2)
	}
	if got := townStateSubjects(t, d, gitRepo); len(got) != 1 {
		t.Errorf("a tick with no change added a commit: %q", got)
	}

	tree["settings/themes/dark.json"] = &fstest.MapFile{Data: []byte(`{"bg":"#111"}`)}
	d.syncTownStateBackup()
	got = townStateSubjects(t, d, gitRepo)
	if len(got) != 2 || !strings.HasPrefix(got[0], "town-state: back up 1 changed file(s): settings/themes/dark.json") {
		t.Fatalf("after the theme change, town-state subjects = %q; want a second commit naming dark.json", got)
	}
	if strings.Contains(got[0], "light.json") {
		t.Errorf("second commit %q names an unchanged file", got[0])
	}
}

// TestTownStateBackupRefusesForeignStagedFile: a file another writer staged in
// the backup repository is not committed under the town-state subject.
func TestTownStateBackupRefusesForeignStagedFile(t *testing.T) {
	t.Parallel()
	d, f, town := townStateDaemon(t)
	d.townStateFS = func(string) fs.FS { return townStateTree() }
	gitRepo := filepath.Join(town, ".dolt-archive", "git")
	seedTownStateOrigin(t, f, town, gitRepo)

	foreign := filepath.Join(gitRepo, "testdb", "issues.jsonl")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.backupGitAt(gitRepo, 0).Add("-A", "testdb"); err != nil {
		t.Fatalf("staging the foreign file: %v", err)
	}
	before := townStateHead(t, f, gitRepo)

	err := d.runTownStateBackup(gitRepo)
	if err == nil || !strings.Contains(err.Error(), "outside town-state/") {
		t.Fatalf("runTownStateBackup = %v, want a refusal naming the foreign staged file", err)
	}
	if after := townStateHead(t, f, gitRepo); after != before {
		t.Errorf("the refused tick committed: HEAD %s -> %s", before, after)
	}
}

// townStateFault wraps the daemon's git and fails one operation, so each error
// branch of a tick is taken for real rather than assumed.
type townStateFault struct {
	daemonGit
	op string
}

func (f townStateFault) Add(pathspecs ...string) error {
	if f.op == "add" {
		return errors.New("injected: add")
	}
	return f.daemonGit.Add(pathspecs...)
}

func (f townStateFault) StagedChanges() ([]gtgit.StagedChange, error) {
	if f.op == "staged" {
		return nil, errors.New("injected: staged")
	}
	return f.daemonGit.StagedChanges()
}

func (f townStateFault) CommitWithAuthor(message, author string) error {
	if f.op == "commit" {
		return errors.New("injected: commit")
	}
	return f.daemonGit.CommitWithAuthor(message, author)
}

func (f townStateFault) CurrentBranch() (string, error) {
	if f.op == "branch" {
		return "", errors.New("injected: branch")
	}
	return f.daemonGit.CurrentBranch()
}

func (f townStateFault) PushWithTimeout(remote, refspec string, force bool, timeout time.Duration) error {
	if f.op == "push" {
		return errors.New("injected: push")
	}
	return f.daemonGit.PushWithTimeout(remote, refspec, force, timeout)
}

// TestTownStateBackupFailuresEscalate takes each error branch of a tick: the
// read, the registry, the repository and the git steps. Each raises one alert
// whose text names the failure that was taken.
func TestTownStateBackupFailuresEscalate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, d *Daemon, f *gitfake.Fake, town, gitRepo string)
		fault string // the git operation townStateFault fails, if any
		want  string // text the alert must carry
	}{
		{
			name: "corrupt rig registry",
			setup: func(t *testing.T, d *Daemon, f *gitfake.Fake, town, gitRepo string) {
				d.townStateRigs = nil
				writeTownStateFile(t, filepath.Join(town, "mayor", "rigs.json"), "{not json")
			},
			want: "rig registry",
		},
		{
			name: "unreadable town file",
			setup: func(t *testing.T, d *Daemon, f *gitfake.Fake, town, gitRepo string) {
				d.townStateFS = func(string) fs.FS {
					return failingFS{MapFS: townStateTree(), file: "settings/models.json"}
				}
			},
			want: "input/output error",
		},
		{
			name: "repository directory is a file",
			setup: func(t *testing.T, d *Daemon, f *gitfake.Fake, town, gitRepo string) {
				writeTownStateFile(t, filepath.Join(town, ".dolt-archive"), "not a directory")
			},
			want: "not a directory",
		},
		{
			name: "snapshot directory is a file",
			setup: func(t *testing.T, d *Daemon, f *gitfake.Fake, town, gitRepo string) {
				initTownStateRepo(t, d, gitRepo)
				writeTownStateFile(t, filepath.Join(gitRepo, townStateBackupDir), "not a directory")
			},
			want: "not a directory",
		},
		{
			name: "snapshot file is a directory",
			setup: func(t *testing.T, d *Daemon, f *gitfake.Fake, town, gitRepo string) {
				initTownStateRepo(t, d, gitRepo)
				if err := os.MkdirAll(filepath.Join(gitRepo, townStateBackupDir, townStateIdentityFile), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "is a directory",
		},
		{
			name:  "git add fails",
			setup: seededTownStateOrigin,
			fault: "add",
			want:  "injected: add",
		},
		{
			name:  "git diff --cached fails",
			setup: seededTownStateOrigin,
			fault: "staged",
			want:  "injected: staged",
		},
		{
			name:  "git commit fails",
			setup: seededTownStateOrigin,
			fault: "commit",
			want:  "injected: commit",
		},
		{
			name:  "backup branch unreadable",
			setup: seededTownStateOrigin,
			fault: "branch",
			want:  "injected: branch",
		},
		{
			name:  "push refused",
			setup: seededTownStateOrigin,
			fault: "push",
			want:  "injected: push",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, f, town := townStateDaemon(t)
			rec := notifyfake.New()
			d.notifier = rec
			d.townStateFS = func(string) fs.FS { return townStateTree() }
			gitRepo := filepath.Join(town, ".dolt-archive", "git")
			tc.setup(t, d, f, town, gitRepo)
			if tc.fault != "" {
				orig := d.openGitFn
				d.openGitFn = func(dir string) daemonGit { return townStateFault{orig(dir), tc.fault} }
			}

			d.syncTownStateBackup()

			got := rec.Escalations()
			if len(got) != 1 || got[0].Escalation.Fingerprint != alertKeyTownStateBackup {
				t.Fatalf("escalations = %+v, want one under %s", got, alertKeyTownStateBackup)
			}
			if !strings.Contains(got[0].Escalation.Reason, tc.want) {
				t.Errorf("escalation reason = %q, want it to carry %q", got[0].Escalation.Reason, tc.want)
			}
		})
	}
}

// seededTownStateOrigin is the setup for a tick that reaches the git steps: a
// backup repository cloned from an origin that holds one commit.
func seededTownStateOrigin(t *testing.T, d *Daemon, f *gitfake.Fake, town, gitRepo string) {
	t.Helper()
	seedTownStateOrigin(t, f, town, gitRepo)
}

// initTownStateRepo makes gitRepo an empty backup repository.
func initTownStateRepo(t *testing.T, d *Daemon, gitRepo string) {
	t.Helper()
	if err := os.MkdirAll(gitRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	initEmptyGitRepo(t, d, gitRepo)
}

// writeTownFile writes content to path, creating its parent directory.
func writeTownStateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTownStateBackupWithoutOriginEscalates: a backup that cannot push raises an
// alert, so a copy that stays on the pod is not reported as a clean run.
func TestTownStateBackupWithoutOriginEscalates(t *testing.T) {
	t.Parallel()
	d, _, town := townStateDaemon(t)
	rec := notifyfake.New()
	d.notifier = rec
	d.townStateFS = func(string) fs.FS { return townStateTree() }
	gitRepo := filepath.Join(town, ".dolt-archive", "git")
	if err := os.MkdirAll(gitRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	initEmptyGitRepo(t, d, gitRepo)

	d.syncTownStateBackup()

	got := rec.Escalations()
	if len(got) != 1 || got[0].Escalation.Fingerprint != alertKeyTownStateBackup {
		t.Fatalf("escalations = %+v, want one under %s", got, alertKeyTownStateBackup)
	}
	if !strings.Contains(got[0].Escalation.Reason, "origin") {
		t.Errorf("escalation reason = %q, want it to name the missing origin", got[0].Escalation.Reason)
	}
}

// townStateDaemon is a daemon over a fresh town root, its git in a gitfake world,
// with the patrol enabled and one registered rig.
func townStateDaemon(t *testing.T) (*Daemon, *gitfake.Fake, string) {
	t.Helper()
	town := t.TempDir()
	d, f := backupTestDaemon(t)
	d.config = &Config{TownRoot: town}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		TownStateBackup: &TownStateBackupConfig{Enabled: true},
	}}
	d.townStateRigs = func(string) []string { return []string{"gastown"} }
	return d, f, town
}

// seedTownStateOrigin makes a bare origin with one commit and clones it to
// gitRepo, the backup repository. It returns the origin's path.
func seedTownStateOrigin(t *testing.T, f *gitfake.Fake, town, gitRepo string) string {
	t.Helper()
	remote := filepath.Join(town, "origin.git")
	f.InitBare(t, remote)
	f.Commit(t, remote, "main", "seed", map[string]string{"README": "init\n"})
	if err := os.MkdirAll(filepath.Dir(gitRepo), 0o755); err != nil {
		t.Fatal(err)
	}
	f.Clone(t, remote, gitRepo)
	return remote
}

// townStateHead is the backup repository's HEAD commit.
func townStateHead(t *testing.T, f *gitfake.Fake, gitRepo string) string {
	t.Helper()
	head, err := f.Open(gitRepo).Rev("HEAD")
	if err != nil {
		t.Fatalf("HEAD of %s: %v", gitRepo, err)
	}
	return head
}

// townStateSubjects lists the backup repository's town-state commit subjects,
// newest first.
func townStateSubjects(t *testing.T, d *Daemon, gitRepo string) []string {
	t.Helper()
	var out []string
	for _, s := range backupSubjects(t, d, gitRepo) {
		if strings.HasPrefix(s, "town-state:") {
			out = append(out, s)
		}
	}
	return out
}

// failingFS fails the read of one file, or the listing of one directory, and
// reads everything else from the map.
type failingFS struct {
	fstest.MapFS
	file, dir string
}

func (f failingFS) ReadFile(name string) ([]byte, error) {
	if name == f.file {
		return nil, errors.New("input/output error")
	}
	return fs.ReadFile(f.MapFS, name)
}

func (f failingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.dir {
		return nil, errors.New("input/output error")
	}
	return fs.ReadDir(f.MapFS, name)
}

func sortedFileKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
