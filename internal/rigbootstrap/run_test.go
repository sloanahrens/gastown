package rigbootstrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// call is one program the fake exec was asked to run.
type call struct {
	dir  string
	name string
	args []string
}

// fakeExec answers for gt and git without starting either. It records every call.
// A successful gt call writes gtOut and gtErrOut as gt would, creates the rig
// directory as gt rig add does, and seeds rigFiles (relative path to contents)
// under it, as a rig created from a repo that already has settings would.
// gitStartErr and gtStartErr are returned instead of an exit code when set: the
// program could not start. gitBlocks makes the git call wait for its context to
// end and return that error, as a program the context kills does.
type fakeExec struct {
	t           *testing.T
	calls       []call
	gtCode      int
	gtOut       string
	gtErrOut    string
	gitFails    bool
	gitBlocks   bool
	gitStartErr error
	gtStartErr  error
	rigFiles    map[string][]byte
	rigFileMode fs.FileMode // the mode of seeded rigFiles; zero means 0o644
}

func (f *fakeExec) exec(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) (int, error) {
	f.calls = append(f.calls, call{dir: dir, name: name, args: args})
	if name == "git" {
		if f.gitStartErr != nil {
			return 0, f.gitStartErr
		}
		if f.gitBlocks {
			<-ctx.Done()
			return 0, ctx.Err()
		}
		if f.gitFails {
			return 128, nil
		}
		return 0, nil
	}
	if f.gtStartErr != nil {
		return 0, f.gtStartErr
	}
	io.WriteString(stdout, f.gtOut)
	io.WriteString(stderr, f.gtErrOut)
	if f.gtCode != 0 {
		return f.gtCode, nil
	}
	rigDir := filepath.Join(dir, args[2])
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	mode := f.rigFileMode
	if mode == 0 {
		mode = 0o644
	}
	for rel, data := range f.rigFiles {
		path := filepath.Join(rigDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			f.t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			f.t.Fatal(err)
		}
	}
	return 0, nil
}

// fixture is a town root and a local repo under a temporary directory, with the
// run's deps wired to a fake gt and git. Files live only under that directory.
type fixture struct {
	t      *testing.T
	town   string
	repo   string
	fake   *fakeExec
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t:      t,
		town:   filepath.Join(root, "town"),
		repo:   filepath.Join(root, "repo"),
		fake:   &fakeExec{t: t},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
	}
	for _, dir := range []string{f.town, f.repo} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) deps() Deps {
	return Deps{Exec: f.fake.exec, Files: OSFiles{}, Stdout: f.stdout, Stderr: f.stderr}
}

// opts is a run that names rig and uses the fixture's town root and repo.
func (f *fixture) opts(rig string) Options {
	return Options{TownRoot: f.town, Rig: rig, LocalRepo: f.repo, GTBin: "gt"}
}

// gtCalls returns the calls that ran gt, leaving out git's.
func (f *fixture) gtCalls() []call {
	var out []call
	for _, c := range f.fake.calls {
		if c.name == "gt" {
			out = append(out, c)
		}
	}
	return out
}

func TestRunRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(f *fixture) (Options, string) // the options and the stderr message
	}{
		{
			name: "town root missing",
			setup: func(f *fixture) (Options, string) {
				o := f.opts("nightrider")
				o.TownRoot = filepath.Join(f.town, "absent")
				return o, "Town root does not exist: " + o.TownRoot
			},
		},
		{
			name: "town root is a file, not a directory",
			setup: func(f *fixture) (Options, string) {
				path := filepath.Join(f.town, "townfile")
				if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
					f.t.Fatal(err)
				}
				o := f.opts("nightrider")
				o.TownRoot = path
				return o, "Town root does not exist: " + path
			},
		},
		{
			name: "local repo missing",
			setup: func(f *fixture) (Options, string) {
				o := f.opts("nightrider")
				o.LocalRepo = filepath.Join(f.repo, "absent")
				return o, "Local repo does not exist: " + o.LocalRepo
			},
		},
		{
			name: "local repo is a file, not a directory",
			setup: func(f *fixture) (Options, string) {
				path := filepath.Join(f.repo, "repofile")
				if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
					f.t.Fatal(err)
				}
				o := f.opts("nightrider")
				o.LocalRepo = path
				return o, "Local repo does not exist: " + path
			},
		},
		{
			name: "local repo is not a git repository",
			setup: func(f *fixture) (Options, string) {
				f.fake.gitFails = true
				o := f.opts("nightrider")
				return o, "Local repo is not a git repository: " + f.repo
			},
		},
		{
			name: "target rig is a directory",
			setup: func(f *fixture) (Options, string) {
				if err := os.MkdirAll(filepath.Join(f.town, "nightrider"), 0o755); err != nil {
					f.t.Fatal(err)
				}
				return f.opts("nightrider"), "Target rig already exists: " + f.town + "/nightrider"
			},
		},
		{
			name: "target rig is a file: any existing entry counts",
			setup: func(f *fixture) (Options, string) {
				if err := os.WriteFile(filepath.Join(f.town, "nightrider"), []byte("x"), 0o644); err != nil {
					f.t.Fatal(err)
				}
				return f.opts("nightrider"), "Target rig already exists: " + f.town + "/nightrider"
			},
		},
		{
			// Both town root and local repo are missing: the town root is named,
			// as the script checked it first.
			name: "town root is checked before the local repo",
			setup: func(f *fixture) (Options, string) {
				o := f.opts("nightrider")
				o.TownRoot = filepath.Join(f.town, "absent")
				o.LocalRepo = filepath.Join(f.repo, "absent")
				return o, "Town root does not exist: " + o.TownRoot
			},
		},
		{
			// The local repo is missing and the target is taken: the repo is named,
			// as it is checked before the target.
			name: "local repo is checked before the target rig",
			setup: func(f *fixture) (Options, string) {
				if err := os.MkdirAll(filepath.Join(f.town, "nightrider"), 0o755); err != nil {
					f.t.Fatal(err)
				}
				o := f.opts("nightrider")
				o.LocalRepo = filepath.Join(f.repo, "absent")
				return o, "Local repo does not exist: " + o.LocalRepo
			},
		},
		{
			// Not a git repository and the target is taken: git is named, as it is
			// checked before the target.
			name: "git check is before the target rig",
			setup: func(f *fixture) (Options, string) {
				f.fake.gitFails = true
				if err := os.MkdirAll(filepath.Join(f.town, "nightrider"), 0o755); err != nil {
					f.t.Fatal(err)
				}
				return f.opts("nightrider"), "Local repo is not a git repository: " + f.repo
			},
		},
		{
			// A trailing slash on the town root must not double the separator: the
			// message names the rig once, as the settings write does.
			name: "target rig is a directory, town root has a trailing slash",
			setup: func(f *fixture) (Options, string) {
				if err := os.MkdirAll(filepath.Join(f.town, "nightrider"), 0o755); err != nil {
					f.t.Fatal(err)
				}
				o := f.opts("nightrider")
				o.TownRoot = f.town + "/"
				return o, "Target rig already exists: " + filepath.Join(f.town, "nightrider")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			opts, want := tt.setup(f)

			if code := Run(context.Background(), opts, f.deps()); code != 1 {
				t.Fatalf("Run() = %d, want 1", code)
			}
			if got := f.stderr.String(); got != want+"\n" {
				t.Fatalf("stderr = %q, want %q", got, want+"\n")
			}
			if f.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", f.stdout.String())
			}
			if calls := f.gtCalls(); len(calls) != 0 {
				t.Fatalf("gt ran %d time(s), want none: %+v", len(calls), calls)
			}
		})
	}
}

func TestRunGtArgvAndCwd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		remote   string   // the --remote value; empty leaves the default file://<repo>
		prefix   string   // the --prefix value; empty leaves it out
		branch   string   // the --branch value; empty leaves it out
		wantTail []string // the arguments after --local-repo <repo>
	}{
		{name: "required only", wantTail: nil},
		{name: "prefix", prefix: "nr", wantTail: []string{"--prefix", "nr"}},
		{name: "branch", branch: "trunk", wantTail: []string{"--branch", "trunk"}},
		{name: "prefix then branch", prefix: "nr", branch: "trunk", wantTail: []string{"--prefix", "nr", "--branch", "trunk"}},
		{name: "remote given", remote: "git@example.com:rigs/nr.git", wantTail: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			o := f.opts("nightrider")
			o.Remote = tt.remote
			o.Prefix = tt.prefix
			o.Branch = tt.branch

			wantRemote := tt.remote
			if wantRemote == "" {
				wantRemote = "file://" + f.repo
			}
			wantArgs := append([]string{"rig", "add", "nightrider", wantRemote, "--local-repo", f.repo}, tt.wantTail...)

			if code := Run(context.Background(), o, f.deps()); code != 0 {
				t.Fatalf("Run() = %d, want 0; stderr %q", code, f.stderr.String())
			}
			if len(f.fake.calls) != 2 {
				t.Fatalf("programs run = %+v, want git then gt", f.fake.calls)
			}
			wantGit := []string{"-C", f.repo, "rev-parse", "--git-dir"}
			if got := f.fake.calls[0].args; !reflect.DeepEqual(got, wantGit) {
				t.Fatalf("git args = %q, want %q", got, wantGit)
			}
			gt := f.gtCalls()
			if len(gt) != 1 {
				t.Fatalf("gt ran %d time(s), want 1", len(gt))
			}
			if gt[0].dir != f.town {
				t.Fatalf("gt ran in %q, want the town root %q", gt[0].dir, f.town)
			}
			if !reflect.DeepEqual(gt[0].args, wantArgs) {
				t.Fatalf("gt args = %q, want %q", gt[0].args, wantArgs)
			}
			if got, want := f.stdout.String(), "Rig bootstrapped at "+f.town+"/nightrider\n"; got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
		})
	}
}

// TestRunStartFailures: a program that cannot start is not an answer about the
// repo or the rig. The run names the program, exits 127 as the command-not-run
// code, and does not go on to refuse or write anything.
func TestRunStartFailures(t *testing.T) {
	t.Parallel()
	startErr := errors.New("fork/exec: no such file or directory")
	tests := []struct {
		name       string
		setup      func(f *fixture)
		wantGT     int    // the number of gt calls: a start failure still counts as an attempt
		wantStderr string // the stderr line
	}{
		{
			name:       "git cannot start",
			setup:      func(f *fixture) { f.fake.gitStartErr = startErr },
			wantGT:     0,
			wantStderr: "Could not run git: " + startErr.Error() + "\n",
		},
		{
			name:       "git not found on the path",
			setup:      func(f *fixture) { f.fake.gitStartErr = exec.ErrNotFound },
			wantGT:     0,
			wantStderr: "Could not run git: " + exec.ErrNotFound.Error() + "\n",
		},
		{
			name:       "gt cannot start",
			setup:      func(f *fixture) { f.fake.gtStartErr = startErr },
			wantGT:     1,
			wantStderr: "Could not run gt: " + startErr.Error() + "\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tt.setup(f)
			o := f.opts("nightrider")
			o.PolecatAgent = "claude"

			if code := Run(context.Background(), o, f.deps()); code != 127 {
				t.Fatalf("Run() = %d, want 127; stderr %q", code, f.stderr.String())
			}
			if got := f.stderr.String(); got != tt.wantStderr {
				t.Fatalf("stderr = %q, want %q", got, tt.wantStderr)
			}
			if f.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", f.stdout.String())
			}
			if got := len(f.gtCalls()); got != tt.wantGT {
				t.Fatalf("gt ran %d time(s), want %d", got, tt.wantGT)
			}
			if _, err := os.Stat(filepath.Join(f.town, "nightrider")); !os.IsNotExist(err) {
				t.Fatalf("rig directory exists after a start failure: err = %v", err)
			}
		})
	}
}

// TestRunGitInterrupted: a context that ends while git is checked is an
// interrupted run, not a start failure. It exits 1 with the interrupt named, and
// it does not exit 127, run gt, or write anything.
func TestRunGitInterrupted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ctx     func() (context.Context, context.CancelFunc) // the context the run gets
		wantErr string                                       // the stderr line
	}{
		{
			name: "cancelled before git runs",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			wantErr: "interrupted: context canceled\n",
		},
		{
			name: "deadline passes while git runs",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 10*time.Millisecond)
			},
			wantErr: "interrupted: context deadline exceeded\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.fake.gitBlocks = true
			o := f.opts("nightrider")
			o.PolecatAgent = "claude"
			ctx, cancel := tt.ctx()
			defer cancel()

			if code := Run(ctx, o, f.deps()); code != 1 {
				t.Fatalf("Run() = %d, want 1; stderr %q", code, f.stderr.String())
			}
			if got := f.stderr.String(); got != tt.wantErr {
				t.Fatalf("stderr = %q, want %q", got, tt.wantErr)
			}
			if f.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", f.stdout.String())
			}
			if got := len(f.gtCalls()); got != 0 {
				t.Fatalf("gt ran %d time(s), want none", got)
			}
			if _, err := os.Stat(filepath.Join(f.town, "nightrider")); !os.IsNotExist(err) {
				t.Fatalf("rig directory exists after an interrupted run: err = %v", err)
			}
		})
	}
}

func TestRunGtFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fake.gtCode = 3
	f.fake.gtOut = "gt: rig add refused\n"
	f.fake.gtErrOut = "gt: detail\n"
	o := f.opts("nightrider")
	o.PolecatAgent = "claude"

	if code := Run(context.Background(), o, f.deps()); code != 3 {
		t.Fatalf("Run() = %d, want gt's exit code 3", code)
	}
	if got := f.stdout.String(); got != f.fake.gtOut {
		t.Fatalf("stdout = %q, want only gt's output %q", got, f.fake.gtOut)
	}
	if got := f.stderr.String(); got != f.fake.gtErrOut {
		t.Fatalf("stderr = %q, want only gt's output %q", got, f.fake.gtErrOut)
	}
	if _, err := os.Stat(filepath.Join(f.town, "nightrider", "settings")); !os.IsNotExist(err) {
		t.Fatalf("settings directory exists after gt failed: err = %v", err)
	}
}

func TestRunSettings(t *testing.T) {
	t.Parallel()
	before, err := os.ReadFile("testdata/settings_before.json")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile("testdata/settings_after.json")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		polecat  string
		existing []byte // seeded at settings/config.json by the gt that creates the rig
		wantFile []byte // nil means no settings file exists after the run
		wantCode int
		wantPerm fs.FileMode
	}{
		{
			name:     "no polecat agent and no settings file writes nothing",
			polecat:  "",
			wantCode: 0,
		},
		{
			name:     "no polecat agent leaves an existing settings file alone",
			polecat:  "",
			existing: before,
			wantFile: before,
			wantCode: 0,
		},
		{
			name:     "polecat agent with no settings file writes a new one",
			polecat:  "claude",
			wantFile: []byte("{\n  \"type\": \"rig-settings\",\n  \"version\": 1,\n  \"role_agents\": {\n    \"polecat\": \"claude\"\n  }\n}\n"),
			wantCode: 0,
			wantPerm: 0o644,
		},
		{
			name:     "polecat agent merges into an existing settings file",
			polecat:  "claude",
			existing: before,
			wantFile: after,
			wantCode: 0,
			wantPerm: 0o644,
		},
		{
			name:     "corrupt settings file fails after gt ran, and is left as it was",
			polecat:  "claude",
			existing: []byte("{not json"),
			wantFile: []byte("{not json"),
			wantCode: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if tt.existing != nil {
				f.fake.rigFiles = map[string][]byte{"settings/config.json": tt.existing}
			}
			o := f.opts("nightrider")
			o.PolecatAgent = tt.polecat
			rigPath := f.town + "/nightrider"
			settingsPath := filepath.Join(rigPath, "settings", "config.json")

			if code := Run(context.Background(), o, f.deps()); code != tt.wantCode {
				t.Fatalf("Run() = %d, want %d; stderr %q", code, tt.wantCode, f.stderr.String())
			}
			if len(f.gtCalls()) != 1 {
				t.Fatalf("gt ran %d time(s), want 1: gt runs before the settings are written", len(f.gtCalls()))
			}

			got, err := os.ReadFile(settingsPath)
			switch {
			case tt.wantFile == nil:
				if !os.IsNotExist(err) {
					t.Fatalf("settings file exists or cannot be read (err %v), want none", err)
				}
			case err != nil:
				t.Fatalf("read settings: %v", err)
			case !bytes.Equal(got, tt.wantFile):
				t.Fatalf("settings file = %q, want %q", got, tt.wantFile)
			}
			if tt.wantPerm != 0 {
				info, err := os.Stat(settingsPath)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != tt.wantPerm {
					t.Fatalf("settings mode = %v, want %v", info.Mode().Perm(), tt.wantPerm)
				}
			}

			if tt.wantCode == 0 {
				if want := "Rig bootstrapped at " + rigPath + "\n"; f.stdout.String() != want {
					t.Fatalf("stdout = %q, want %q", f.stdout.String(), want)
				}
				return
			}
			if f.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing: the final line is not printed", f.stdout.String())
			}
			// The message says the rig exists and the settings were not written,
			// so the operator knows the rig is there.
			if !strings.Contains(f.stderr.String(), "Rig exists at "+rigPath+", but its settings were not written:") {
				t.Fatalf("stderr = %q, want the rig-exists message for %s", f.stderr.String(), rigPath)
			}
		})
	}
}

// TestRunSettingsKeepsExistingMode: the 0644 mode is for a settings file the run
// creates. A file that already exists keeps the mode the operator gave it.
func TestRunSettingsKeepsExistingMode(t *testing.T) {
	t.Parallel()
	before, err := os.ReadFile("testdata/settings_before.json")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile("testdata/settings_after.json")
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.fake.rigFiles = map[string][]byte{"settings/config.json": before}
	f.fake.rigFileMode = 0o600
	o := f.opts("nightrider")
	o.PolecatAgent = "claude"
	settingsPath := filepath.Join(f.town, "nightrider", "settings", "config.json")

	if code := Run(context.Background(), o, f.deps()); code != 0 {
		t.Fatalf("Run() = %d, want 0; stderr %q", code, f.stderr.String())
	}
	info, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %v, want 0600 kept from the existing file", info.Mode().Perm())
	}
	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, after) {
		t.Fatalf("settings file = %q, want the merged file %q", got, after)
	}
}

// TestRunSettingsUnwritable: when the settings directory cannot be made or the
// settings file cannot be read, gt has already created the rig, so the run says
// so and exits 1 without writing settings.
func TestRunSettingsUnwritable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seed map[string][]byte // seeded under the rig by the gt that creates it
	}{
		{
			// settings is a file, so settings/ cannot be made.
			name: "settings directory cannot be made",
			seed: map[string][]byte{"settings": []byte("x")},
		},
		{
			// settings/config.json is a directory, so it cannot be read as a file.
			name: "settings file cannot be read",
			seed: map[string][]byte{"settings/config.json/inner": []byte("x")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.fake.rigFiles = tt.seed
			o := f.opts("nightrider")
			o.PolecatAgent = "claude"
			rigPath := f.town + "/nightrider"

			if code := Run(context.Background(), o, f.deps()); code != 1 {
				t.Fatalf("Run() = %d, want 1; stderr %q", code, f.stderr.String())
			}
			if len(f.gtCalls()) != 1 {
				t.Fatalf("gt ran %d time(s), want 1", len(f.gtCalls()))
			}
			if f.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing: the final line is not printed", f.stdout.String())
			}
			if !strings.Contains(f.stderr.String(), "Rig exists at "+rigPath+", but its settings were not written:") {
				t.Fatalf("stderr = %q, want the rig-exists message for %s", f.stderr.String(), rigPath)
			}
		})
	}
}
