package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/townstate"
)

// migrateFlagCmd is a cobra command named use that defines the strs flags as
// strings and the bools flags as bools, so reading one of them as the wrong type
// is the read error a migrate command must refuse.
func migrateFlagCmd(use string, strs, bools []string) *cobra.Command {
	c := &cobra.Command{Use: use}
	for _, n := range strs {
		c.Flags().String(n, "", "")
	}
	for _, n := range bools {
		c.Flags().Bool(n, false, "")
	}
	return c
}

// TestTownStateMigrateFlagReads: each flag a migrate command reads is read as its
// type, and each read that fails, or a value that is not what the flag means, is a
// refusal that names the flag, never a default.
func TestTownStateMigrateFlagReads(t *testing.T) {
	t.Parallel()
	findTown := func() (string, error) { return migrateTown(t), nil }
	noTown := func() (string, error) { return "", errors.New("no town here") }
	badSecrets := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(badSecrets, []byte("files: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(t.TempDir(), "absent")
	cases := []struct {
		name        string
		cmd         *cobra.Command
		needSecrets bool
		find        func() (string, error)
		set         map[string]string
		want        string
	}{
		{name: "town as a bool", cmd: migrateFlagCmd("plan", nil, []string{"town"}), find: findTown, want: "reading --town"},
		{name: "no --town and no town found", cmd: migrateFlagCmd("plan", nil, nil), find: noTown, want: "finding the town root"},
		{name: "images as a bool", cmd: migrateFlagCmd("plan", []string{"town"}, []string{"images"}), find: findTown, want: "reading --images"},
		{name: "compose-file as a bool", cmd: migrateFlagCmd("plan", []string{"town", "images"}, []string{"compose-file"}), find: findTown, want: "reading --compose-file"},
		{name: "volume as a bool", cmd: migrateFlagCmd("plan", []string{"town", "images", "compose-file"}, []string{"volume"}), find: findTown, want: "reading --volume"},
		{name: "saved-compose as a bool", cmd: migrateFlagCmd("rollback-plan", []string{"town", "images", "compose-file", "volume"}, []string{"saved-compose"}), find: findTown, want: "reading --saved-compose"},
		{name: "since as a bool", cmd: migrateFlagCmd("copy", []string{"town", "images", "compose-file", "volume"}, []string{"since"}), find: findTown, want: "reading --since"},
		{name: "since that is not RFC 3339", cmd: migrateFlagCmd("copy", []string{"town", "images", "compose-file", "volume", "since"}, nil), find: findTown, set: map[string]string{"since": "yesterday"}, want: "not an RFC 3339 time"},
		{name: "cutover as a string", cmd: migrateFlagCmd("copy", []string{"town", "images", "compose-file", "volume", "since", "cutover"}, nil), find: findTown, want: "reading --cutover"},
		{name: "daemon-removed as a string", cmd: migrateFlagCmd("copy", []string{"town", "images", "compose-file", "volume", "since", "daemon-removed"}, []string{"cutover"}), find: findTown, want: "reading --daemon-removed"},
		{name: "secrets-manifest as a bool", cmd: migrateFlagCmd("copy", []string{"town", "images", "compose-file", "volume", "since"}, []string{"secrets-manifest"}), needSecrets: true, find: findTown, want: "reading --secrets-manifest"},
		{name: "a secrets manifest that is missing", cmd: migrateFlagCmd("copy", []string{"town", "images", "compose-file", "volume", "since", "secrets-manifest"}, nil), needSecrets: true, find: findTown, set: map[string]string{"secrets-manifest": absent}, want: "reading the secrets manifest"},
		{name: "a secrets manifest that does not parse", cmd: migrateFlagCmd("copy", []string{"town", "images", "compose-file", "volume", "since", "secrets-manifest"}, nil), needSecrets: true, find: findTown, set: map[string]string{"secrets-manifest": badSecrets}, want: "secrets manifest"},
		{name: "a volume with no image to run", cmd: migrateFlagCmd("copy", []string{"town", "images", "compose-file", "volume", "since"}, nil), find: findTown, set: map[string]string{"volume": "gt-state", "images": absent, "compose-file": absent}, want: "reading the compose file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for name, value := range tc.set {
				if err := tc.cmd.Flags().Set(name, value); err != nil {
					t.Fatalf("setting --%s: %v", name, err)
				}
			}
			_, err := migrateEnvFromFlags(tc.cmd, tc.needSecrets, tc.find, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("migrateEnvFromFlags = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}

	t.Run("a volume with its image resolved from the compose file", func(t *testing.T) {
		t.Parallel()
		compose := filepath.Join(t.TempDir(), "compose.yml")
		if err := os.WriteFile(compose, []byte("image: gt-daemon:4f2a1c9\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		c := migrateFlagCmd("copy", []string{"town", "images", "compose-file", "volume"}, nil)
		for name, value := range map[string]string{"volume": "gt-state", "images": absent, "compose-file": compose} {
			if err := c.Flags().Set(name, value); err != nil {
				t.Fatal(err)
			}
		}
		env, err := migrateEnvFromFlags(c, false, findTown, "")
		if err != nil {
			t.Fatalf("migrateEnvFromFlags: %v", err)
		}
		if env.image != "gt-daemon:4f2a1c9" || env.engine == nil {
			t.Fatalf("env.image = %q, engine set = %v; want the compose tag and an engine", env.image, env.engine != nil)
		}
		if env.savedCompose != compose+".pre-cutover" {
			t.Errorf("savedCompose = %q, want the default beside the compose file", env.savedCompose)
		}
	})

	t.Run("the default clock reads the wall clock", func(t *testing.T) {
		t.Parallel()
		c := migrateFlagCmd("plan", []string{"town"}, nil)
		if err := c.Flags().Set("town", migrateTown(t)); err != nil {
			t.Fatal(err)
		}
		before := time.Now()
		env, err := migrateEnvFromFlags(c, false, findTown, "")
		if err != nil {
			t.Fatalf("migrateEnvFromFlags: %v", err)
		}
		if env.now().Before(before) {
			t.Fatal("the default clock reads a time before the test started")
		}
	})
}

// fakeDocker is a dockerRun that records each argv and its environment, and
// answers from its fields.
type fakeDocker struct {
	calls    [][]string
	envs     [][]string
	deadline bool
	stdout   string
	stderr   string
	err      error
}

func (f *fakeDocker) run(ctx context.Context, args, env []string, _ io.Reader, stdout, stderr io.Writer) error {
	f.calls = append(f.calls, args)
	f.envs = append(f.envs, env)
	_, f.deadline = ctx.Deadline()
	if _, err := io.WriteString(stdout, f.stdout); err != nil {
		return err
	}
	if _, err := io.WriteString(stderr, f.stderr); err != nil {
		return err
	}
	return f.err
}

// TestTownStateMigrateCreateVolume: the create runs docker volume create with the
// migrate label and a bounded run, refuses a name that is not a volume before
// docker sees it, and reports a failure without docker's stderr.
func TestTownStateMigrateCreateVolume(t *testing.T) {
	t.Parallel()
	t.Run("the create carries the migrate label", func(t *testing.T) {
		t.Parallel()
		fd := &fakeDocker{}
		if err := (dockerEngine{run: fd.run}).CreateVolume("gt-state"); err != nil {
			t.Fatalf("CreateVolume = %v", err)
		}
		want := "volume create --label gt/managed-by=town-state-migrate gt-state"
		if len(fd.calls) != 1 || !strings.Contains(strings.Join(fd.calls[0], " "), want) {
			t.Fatalf("docker argv = %v, want %q", fd.calls, want)
		}
		if !fd.deadline {
			t.Error("the create ran without a deadline")
		}
	})
	t.Run("a failure does not carry docker's stderr", func(t *testing.T) {
		t.Parallel()
		fd := &fakeDocker{stderr: "Cannot connect to the Docker daemon\n", err: errors.New("exit status 1")}
		err := (dockerEngine{run: fd.run}).CreateVolume("gt-state")
		if err == nil || strings.Contains(err.Error(), "Cannot connect") {
			t.Fatalf("CreateVolume = %v, want an error without docker's stderr", err)
		}
	})
	t.Run("a path is refused before docker runs", func(t *testing.T) {
		t.Parallel()
		fd := &fakeDocker{}
		if err := (dockerEngine{run: fd.run}).CreateVolume("/host/dir"); err == nil {
			t.Fatal("CreateVolume with a host path = nil, want a refusal")
		}
		if len(fd.calls) != 0 {
			t.Fatalf("docker ran for a refused name: %v", fd.calls)
		}
	})
}

// TestTownStateMigrateDockerEngine: the engine runs one helper per command with
// the volume mounted, refuses a name that is not a volume before docker sees it,
// reads only the docker marker for an absent volume, and reports any other failure.
func TestTownStateMigrateDockerEngine(t *testing.T) {
	t.Parallel()
	t.Run("a run mounts the volume and reports a failed helper", func(t *testing.T) {
		t.Parallel()
		fd := &fakeDocker{err: errors.New("exit status 1")}
		eng := dockerEngine{image: "gt-daemon:test", run: fd.run}
		err := eng.Run("gt-state", townstate.ImportArgv, strings.NewReader(""), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "docker run of the helper for volume gt-state exited") {
			t.Fatalf("Run = %v, want the helper's exit named", err)
		}
		if len(fd.calls) != 1 || !strings.Contains(strings.Join(fd.calls[0], " "), "-v gt-state:/state gt-daemon:test gt town state import --town /state") {
			t.Fatalf("docker argv = %v, want the volume mounted and the import run", fd.calls)
		}
	})
	t.Run("a path is refused before docker runs", func(t *testing.T) {
		t.Parallel()
		fd := &fakeDocker{}
		eng := dockerEngine{image: "gt-daemon:test", run: fd.run}
		if err := eng.Run("/host/dir", townstate.ImportArgv, nil, io.Discard); err == nil {
			t.Fatal("Run with a host path = nil, want a refusal")
		}
		if _, err := eng.HasVolume("/host/dir"); err == nil {
			t.Fatal("HasVolume with a host path = nil, want a refusal")
		}
		if len(fd.calls) != 0 {
			t.Fatalf("docker ran for a refused name: %v", fd.calls)
		}
	})
	t.Run("the existence check", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			fd     *fakeDocker
			want   bool
			errOut bool
		}{
			{name: "present", fd: &fakeDocker{}, want: true},
			{name: "absent", fd: &fakeDocker{stderr: "Error: No such volume: gt-state\n", err: errors.New("exit status 1")}, want: false},
			{name: "docker fails", fd: &fakeDocker{stderr: "Cannot connect to the Docker daemon\n", err: errors.New("exit status 1")}, errOut: true},
		}
		for _, tc := range cases {
			got, err := dockerEngine{run: tc.fd.run}.HasVolume("gt-state")
			if tc.errOut {
				if err == nil || strings.Contains(err.Error(), "Cannot connect") {
					t.Fatalf("%s: HasVolume = %v, %v; want an error that does not carry docker's stderr", tc.name, got, err)
				}
				continue
			}
			if err != nil || got != tc.want {
				t.Fatalf("%s: HasVolume = %v, %v; want %v", tc.name, got, err, tc.want)
			}
			if len(tc.fd.calls) != 1 || !strings.Contains(strings.Join(tc.fd.calls[0], " "), "volume inspect gt-state") {
				t.Errorf("%s: docker argv = %v, want volume inspect", tc.name, tc.fd.calls)
			}
		}
	})
}

// TestTownStateMigrateContainerRunning: a container is running unless docker says it
// is stopped or absent; a container docker cannot inspect is unknown, never stopped.
func TestTownStateMigrateContainerRunning(t *testing.T) {
	t.Parallel()
	absent := &templates.DockerInspectError{StderrText: "Error: No such object: gt-town-daemon-1\n", Err: errors.New("exit status 1")}
	cases := []struct {
		name    string
		out     string
		err     error
		want    bool
		wantErr bool
	}{
		{name: "running", out: "running gt-daemon:abc1234\n", want: true},
		{name: "restarting counts as running", out: "restarting gt-daemon:abc1234\n", want: true},
		{name: "exited", out: "exited gt-daemon:abc1234\n", want: false},
		{name: "absent is neither running nor stopped", err: absent, wantErr: true},
		{name: "inspect fails", err: errors.New("cannot connect"), wantErr: true},
		{name: "no status printed", out: "running\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := func(_ context.Context, _ []string) ([]byte, error) { return []byte(tc.out), tc.err }
			got, err := containerRunningWith(run, "gt-town-daemon-1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("containerRunningWith = %v, nil; want an error", got)
				}
				if tc.err == absent && !errors.Is(err, errDaemonContainerAbsent) {
					t.Fatalf("containerRunningWith error = %v, want errDaemonContainerAbsent", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("containerRunningWith = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// TestTownStateMigrateHoldFile: the dispatch hold is present when its file is, and
// a hold that cannot be checked is an error, never absent.
func TestTownStateMigrateHoldFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if got, err := holdFilePresent(town); err != nil || got {
		t.Fatalf("holdFilePresent with no hold = %v, %v; want absent", got, err)
	}
	if err := os.WriteFile(filepath.Join(town, "seat-refill.hold"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := holdFilePresent(town); err != nil || !got {
		t.Fatalf("holdFilePresent with the hold = %v, %v; want present", got, err)
	}
	notADir := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := holdFilePresent(notADir); err == nil {
		t.Fatal("holdFilePresent under a file = nil, want an error")
	}
}

// TestTownStateMigrateDaemonContainerName: the container is named by the town's
// own settings; a town that names none, or whose settings do not read, has none to
// name.
func TestTownStateMigrateDaemonContainerName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		settings string
		patrol   string
		want     string
		wantErr  string
	}{
		{name: "a town with no patrol config names none", wantErr: "names no daemon.container_name"},
		{name: "settings that are not a JSON object", settings: "[]", patrol: `{"container_name":"gt-town-daemon-1","compose_project":"gt"}`, wantErr: "reading the daemon settings"},
		{name: "a container name that is not a name", patrol: `{"container_name":"!!","compose_project":"gt"}`, wantErr: "daemon.container_name"},
		{name: "the named container", patrol: `{"container_name":"gt-town-daemon-1","compose_project":"gt"}`, want: "gt-town-daemon-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.settings != "" {
				writeTownStateFileContent(t, town, "settings/config.json", tc.settings)
			}
			if tc.patrol != "" {
				writeTownStateFileContent(t, town, "mayor/daemon.json", tc.patrol)
			}
			got, err := daemonContainerName(town)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("daemonContainerName = %q, %v; want an error naming %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("daemonContainerName = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// writeTownStateFileContent writes content at rel under town, creating directories.
func writeTownStateFileContent(t *testing.T, town, rel, content string) {
	t.Helper()
	p := filepath.Join(town, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTownStateMigrateCommandWrappersRefuseAFlagTheyCannotRead: each cobra entry
// point returns its flag read's refusal, and reaches no town lookup first, so the
// refusal does not depend on the directory the test runs in.
func TestTownStateMigrateCommandWrappersRefuseAFlagTheyCannotRead(t *testing.T) {
	t.Parallel()
	wrappers := map[string]func(*cobra.Command, []string) error{
		"plan":          runTownStateMigratePlan,
		"copy":          runTownStateMigrateCopy,
		"verify":        runTownStateMigrateVerify,
		"rollback-plan": runTownStateMigrateRollbackPlan,
	}
	for name, run := range wrappers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := migrateFlagCmd(name, nil, []string{"town"})
			if err := run(c, nil); err == nil || !strings.Contains(err.Error(), "reading --town") {
				t.Fatalf("%s with a --town that is not a string = %v, want the flag read refused", name, err)
			}
		})
	}
}

// TestTownStateMigrateSecretsAndImages: a secrets manifest that does not parse is
// refused, and a compose file that cannot be read gives no image.
func TestTownStateMigrateSecretsAndImages(t *testing.T) {
	t.Parallel()
	if _, err := secretExclusion([]byte("files: [\n")); err == nil {
		t.Fatal("secretExclusion of a manifest that does not parse = nil, want an error")
	}
	_, err := helperImage(filepath.Join(t.TempDir(), "absent.yaml"), filepath.Join(t.TempDir(), "absent-compose.yml"))
	if err == nil || !strings.Contains(err.Error(), "reading the compose file") {
		t.Fatalf("helperImage with no values and no compose = %v, want a refusal naming the compose file", err)
	}
}

// TestTownStateMigrateCommandsRefuseAFailedVolumeRead: each command refuses when
// the volume cannot be checked, listed, measured or surveyed, and names the step.
func TestTownStateMigrateCommandsRefuseAFailedVolumeRead(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent")
	cases := []struct {
		name  string
		town  string
		fault func(fv *fakeVolume)
		run   func(env townStateMigrateEnv) error
		want  string
	}{
		{name: "plan cannot read the free space", fault: func(fv *fakeVolume) { fv.freeErr = errors.New("df exited 1") }, run: runTownStateMigratePlanIn, want: "reading the free space"},
		{name: "plan cannot survey a missing town", town: missing, run: runTownStateMigratePlanIn, want: "surveying the town state"},
		{name: "copy cannot check the volume", fault: func(fv *fakeVolume) { fv.hasErr = errors.New("docker unreachable") }, run: runTownStateMigrateCopyIn, want: "checking volume gt-state"},
		{name: "copy cannot list the volume", fault: func(fv *fakeVolume) { fv.listErr = errors.New("tar exited 2") }, run: runTownStateMigrateCopyIn, want: "listing volume"},
		{name: "copy cannot read the free space", fault: func(fv *fakeVolume) { fv.freeErr = errors.New("df exited 1") }, run: runTownStateMigrateCopyIn, want: "reading the free space"},
		{name: "copy cannot survey a missing town", town: missing, run: runTownStateMigrateCopyIn, want: "surveying the town state"},
		{name: "verify cannot check the volume", fault: func(fv *fakeVolume) { fv.hasErr = errors.New("docker unreachable") }, run: runTownStateMigrateVerifyIn, want: "checking volume gt-state"},
		{name: "verify cannot list the volume", fault: func(fv *fakeVolume) { fv.listErr = errors.New("tar exited 2") }, run: runTownStateMigrateVerifyIn, want: "listing volume"},
		{name: "verify cannot list a missing town", town: missing, run: runTownStateMigrateVerifyIn, want: "listing the host's copy set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, fv, _ := newMigrateEnv(t, migrateTown(t))
			if tc.town != "" {
				env.town = tc.town
			}
			if tc.fault != nil {
				tc.fault(fv)
			}
			err := tc.run(env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("command = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}
