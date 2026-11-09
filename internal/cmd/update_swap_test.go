package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/update"
)

// swapComposeFixture is the daemon section of a rendered compose file, with a
// sibling service whose image must survive the rewrite.
const swapComposeFixture = `services:
  forgejo:
    image: "forgejo:1"
    restart: always
  daemon:
    image: "gt-daemon:0123456789ab"
    restart: unless-stopped
    environment:
      GT_TOWN: town
  dolt:
    image: "dolt:2"
`

// swapDaemonRef is a digest reference for the daemon image.
var swapDaemonRef = "registry.internal:5000/town/gastown-daemon@sha256:" + strings.Repeat("c", 64)

// swapPolecatRef is a digest reference for the polecat image.
var swapPolecatRef = "registry.internal:5000/town/gastown-polecat@sha256:" + strings.Repeat("d", 64)

func TestSetDaemonServiceImageRewritesOnlyTheDaemonImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		compose string
		ref     string
		want    string
		wantErr string
	}{
		{
			name:    "the daemon image line is replaced and the sibling images are kept",
			compose: swapComposeFixture,
			ref:     swapDaemonRef,
			want: strings.Replace(swapComposeFixture,
				`    image: "gt-daemon:0123456789ab"`,
				`    image: "`+swapDaemonRef+`"`, 1),
		},
		{
			name:    "a compose file with no daemon service is refused",
			compose: "services:\n  forgejo:\n    image: \"forgejo:1\"\n",
			ref:     swapDaemonRef,
			wantErr: "names no image line",
		},
		{
			name:    "a daemon service with no image line is refused",
			compose: "services:\n  daemon:\n    restart: always\n  dolt:\n    image: \"dolt:2\"\n",
			ref:     swapDaemonRef,
			wantErr: "names no image line",
		},
		{
			name:    "a ref that is not a digest reference is refused",
			compose: swapComposeFixture,
			ref:     "gt-daemon:0123456789ab",
			wantErr: "is not a digest reference",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := setDaemonServiceImage([]byte(tt.compose), tt.ref)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("setDaemonServiceImage error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("setDaemonServiceImage: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("rewritten compose =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// swapCall is one command the fake compose runner received.
type swapCall struct {
	name string
	args []string
}

// swapFixture is a town's container files: the compose file, the daemon
// settings that pin the polecat image, and a fake compose command.
type swapFixture struct {
	compose    string
	config     string
	calls      []swapCall
	composeErr error
	exit       int
	output     string
}

// newSwapFixture writes the compose file and, when withRunner is set, the
// daemon settings with a tier sweep runner.
func newSwapFixture(t *testing.T, withRunner bool) *swapFixture {
	t.Helper()
	dir := t.TempDir()
	f := &swapFixture{
		compose: filepath.Join(dir, "compose.yml"),
		config:  filepath.Join(dir, "daemon.json"),
	}
	if err := os.WriteFile(f.compose, []byte(swapComposeFixture), 0o644); err != nil {
		t.Fatalf("write the compose file: %v", err)
	}
	cfg := &config.DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: config.CurrentDaemonPatrolConfigVersion,
	}
	if withRunner {
		cfg.Patrols = &config.PatrolsConfig{TierSweep: &config.TierSweepConfig{
			Runner: &config.TierSweepRunnerConfig{Backend: "docker", Image: "installed", CPUs: 2, Memory: 4, Jobs: 2, OutputRoot: filepath.Join(dir, "sweep"), Context: defaultDockerContext},
		}}
	}
	if err := config.SaveDaemonPatrolConfig(f.config, cfg); err != nil {
		t.Fatalf("write the daemon settings: %v", err)
	}
	return f
}

// run is the fake compose command: it records each call and answers with the
// scripted exit, output and error.
func (f *swapFixture) run(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
	f.calls = append(f.calls, swapCall{name: name, args: args})
	return f.output, f.exit, f.composeErr
}

// swap is the production swap over the fixture's files and command.
func (f *swapFixture) swap(project string) composeDaemonSwap {
	return composeDaemonSwap{
		ComposeFile: f.compose,
		Project:     project,
		Context:     defaultDockerContext,
		ConfigPath:  f.config,
		Run:         f.run,
	}
}

func TestComposeDaemonSwapAppliesThePairAndRecreates(t *testing.T) {
	t.Parallel()
	pair := update.ImagePair{Daemon: swapDaemonRef, Polecat: swapPolecatRef, Commit: rollbackNewCommit}

	t.Run("the daemon and the polecat pin move together and the daemon is recreated", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		if err := f.swap("town").Apply(context.Background(), pair); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		compose, err := os.ReadFile(f.compose)
		if err != nil {
			t.Fatalf("read the compose file: %v", err)
		}
		if !strings.Contains(string(compose), `image: "`+swapDaemonRef+`"`) {
			t.Errorf("compose file does not name the daemon digest:\n%s", compose)
		}
		if !strings.Contains(string(compose), `image: "forgejo:1"`) || !strings.Contains(string(compose), `image: "dolt:2"`) {
			t.Errorf("compose file lost a sibling service's image:\n%s", compose)
		}
		cfg, err := config.LoadDaemonPatrolConfig(f.config)
		if err != nil {
			t.Fatalf("read the daemon settings: %v", err)
		}
		if got := cfg.Patrols.TierSweep.Runner.Image; got != swapPolecatRef {
			t.Errorf("runner image = %q, want the pair's polecat digest %q", got, swapPolecatRef)
		}
		want := []string{"--context", defaultDockerContext, "compose", "-p", "town", "-f", f.compose, "up", "-d", "--force-recreate", "daemon"}
		if len(f.calls) != 1 || f.calls[0].name != "docker" || strings.Join(f.calls[0].args, " ") != strings.Join(want, " ") {
			t.Errorf("commands = %+v, want one docker compose recreate of the daemon", f.calls)
		}
	})

	t.Run("a town with no tier sweep runner is refused before anything changes", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, false)
		before, _ := os.ReadFile(f.compose)
		beforeCfg, _ := os.ReadFile(f.config)
		err := f.swap("town").Apply(context.Background(), pair)
		if err == nil || !strings.Contains(err.Error(), "no tier_sweep runner") {
			t.Fatalf("Apply error = %v, want the missing runner named", err)
		}
		if after, _ := os.ReadFile(f.compose); string(after) != string(before) {
			t.Error("the compose file changed on a refused swap")
		}
		if after, _ := os.ReadFile(f.config); string(after) != string(beforeCfg) {
			t.Error("the daemon settings changed on a refused swap")
		}
		if len(f.calls) != 0 {
			t.Errorf("commands = %+v, want none", f.calls)
		}
	})

	t.Run("a compose command that exits nonzero is named without its output", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		f.exit = 1
		f.output = "secret-token-in-output"
		err := f.swap("town").Apply(context.Background(), pair)
		if err == nil || !strings.Contains(err.Error(), "exited 1") {
			t.Fatalf("Apply error = %v, want the exit status", err)
		}
		if strings.Contains(err.Error(), "secret-token-in-output") {
			t.Errorf("Apply error carries the command's output: %v", err)
		}
	})

	t.Run("a compose command that cannot start is an error", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		f.composeErr = errors.New("docker not found")
		if err := f.swap("town").Apply(context.Background(), pair); err == nil {
			t.Fatal("Apply with a command that cannot start returned no error")
		}
	})
}

// TestUpdateImagesFlagRefusesAMissingFileBeforeAnyStep holds the early refusal:
// a values file that does not read stops the run before a polecat is drained.
func TestUpdateImagesFlagRefusesAMissingFileBeforeAnyStep(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ran := false
	swapAsked := 0
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return townRoot, nil },
		DepsFactory: func(string) (update.Deps, error) {
			return update.Deps{Store: &update.Store{Dir: update.Dir(townRoot)}, Clock: updateClock{}}, nil
		},
		Run: func(context.Context, update.Deps, update.Options) (update.State, error) {
			ran = true
			return update.State{}, nil
		},
		ImageSwap: func(string) (update.ImageSwap, error) {
			swapAsked++
			return nil, nil
		},
	}
	c := newUpdateCmdWith(seams)
	c.SetArgs([]string{"--images", filepath.Join(townRoot, "missing.yaml")})
	c.SetOut(new(strings.Builder))
	c.SetErr(new(strings.Builder))
	err := c.Execute()
	if err == nil || !strings.Contains(err.Error(), "the --images values file") {
		t.Fatalf("gt update --images error = %v, want the values file refusal", err)
	}
	if ran || swapAsked != 0 {
		t.Errorf("the run started (%v) or the swap was wired (%d), want neither", ran, swapAsked)
	}
}

// TestUpdateWithoutImagesDoesNotWireTheSwap holds the flag's absence: a run
// without --images never asks for the daemon swap.
func TestUpdateWithoutImagesDoesNotWireTheSwap(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	clock := &rollbackTestClock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	ran := false
	swapAsked := 0
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return townRoot, nil },
		DepsFactory: func(string) (update.Deps, error) {
			return update.Deps{Store: &update.Store{Dir: update.Dir(townRoot)}, Clock: clock, Sleep: clock.sleep}, nil
		},
		Run: func(context.Context, update.Deps, update.Options) (update.State, error) {
			ran = true
			return update.State{Outcome: update.OutcomeOK}, nil
		},
		ImageSwap: func(string) (update.ImageSwap, error) {
			swapAsked++
			return nil, nil
		},
	}
	c := newUpdateCmdWith(seams)
	c.SetArgs([]string{})
	c.SetOut(new(strings.Builder))
	c.SetErr(new(strings.Builder))
	if err := c.Execute(); err != nil {
		t.Fatalf("gt update without --images: %v", err)
	}
	if !ran {
		t.Fatal("the run did not start")
	}
	if swapAsked != 0 {
		t.Errorf("the daemon swap was wired %d times for a run without --images, want none", swapAsked)
	}
}

// TestComposeDaemonSwapPutsBothHalvesBackOnAFailedStep holds the pair's order:
// the compose file is written first and the settings last, and a step that fails
// after a half has moved puts that half back. A failed put-back is named.
func TestComposeDaemonSwapPutsBothHalvesBackOnAFailedStep(t *testing.T) {
	t.Parallel()
	pair := update.ImagePair{Daemon: swapDaemonRef, Polecat: swapPolecatRef, Commit: rollbackNewCommit}

	t.Run("a settings write that fails after the compose file moved puts the compose file back", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		s := f.swap("town")
		s.writeSettings = func(string, func(*config.DaemonPatrolConfig, bool) error) error {
			return errors.New("settings disk full")
		}
		before := readSwapFile(t, f.compose)
		beforeCfg := readSwapFile(t, f.config)
		err := s.Apply(context.Background(), pair)
		if err == nil || !strings.Contains(err.Error(), "pinning the polecat image") || !strings.Contains(err.Error(), "both halves") {
			t.Fatalf("Apply error = %v, want the pin failure and the put-back", err)
		}
		if got := readSwapFile(t, f.compose); got != before {
			t.Errorf("compose file after the failed pin =\n%s\nwant the file as it was:\n%s", got, before)
		}
		if got := readSwapFile(t, f.config); got != beforeCfg {
			t.Error("the daemon settings changed on a failed pin")
		}
		if len(f.calls) != 0 {
			t.Errorf("commands = %+v, want none before the pin is set", f.calls)
		}
	})

	t.Run("a recreate that fails puts both halves back", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		f.exit = 1
		before := readSwapFile(t, f.compose)
		err := f.swap("town").Apply(context.Background(), pair)
		if err == nil || !strings.Contains(err.Error(), "exited 1") || !strings.Contains(err.Error(), "both halves") {
			t.Fatalf("Apply error = %v, want the recreate failure and the put-back", err)
		}
		if got := readSwapFile(t, f.compose); got != before {
			t.Errorf("compose file after the failed recreate was not put back:\n%s", got)
		}
		if got := runnerImage(t, f.config); got != "installed" {
			t.Errorf("runner image after the failed recreate = %q, want the pin put back to %q", got, "installed")
		}
		if len(f.calls) != 1 {
			t.Errorf("commands = %d, want the one recreate attempt", len(f.calls))
		}
	})

	t.Run("a failing put-back of the settings pin is named as a split pair", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		f.exit = 1
		s := f.swap("town")
		settingsWrites := 0
		s.writeSettings = func(path string, mutate func(*config.DaemonPatrolConfig, bool) error) error {
			settingsWrites++
			if settingsWrites == 2 {
				return errors.New("settings went read-only")
			}
			return config.UpdateConfigJSON(path, 0o644, mutate)
		}
		err := s.Apply(context.Background(), pair)
		if err == nil || !strings.Contains(err.Error(), "the pair is split") || !strings.Contains(err.Error(), "restoring the polecat pin") {
			t.Fatalf("Apply error = %v, want the split pair named", err)
		}
		if got := runnerImage(t, f.config); got != swapPolecatRef {
			t.Errorf("runner image = %q, want the new polecat digest left in place when its put-back failed", got)
		}
		if got := readSwapFile(t, f.compose); strings.Contains(got, swapDaemonRef) || !strings.Contains(got, "gt-daemon:0123456789ab") {
			t.Errorf("compose file = \n%s\nwant the original daemon image put back, since the compose put-back is still attempted", got)
		}
	})

	t.Run("a failing put-back of the compose file is named as a split pair", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		f.exit = 1
		s := f.swap("town")
		composeWrites := 0
		s.writeCompose = func(path string, content []byte) error {
			composeWrites++
			if composeWrites == 2 {
				return errors.New("compose went read-only")
			}
			return daemonimage.WriteComposeFile(path, content)
		}
		err := s.Apply(context.Background(), pair)
		if err == nil || !strings.Contains(err.Error(), "the pair is split") || !strings.Contains(err.Error(), "restoring the compose file") {
			t.Fatalf("Apply error = %v, want the split pair named", err)
		}
		if got := runnerImage(t, f.config); got != "installed" {
			t.Errorf("runner image = %q, want the settings pin put back", got)
		}
	})
}

// TestDigestInstallEscalatesWhenTheSwapCannotBePutBack runs a digest install over
// the real compose swap. The new pair's recreate fails, the compose file cannot
// be put back, and the restore to the previous pair fails too: the daemon is
// left on a split pair, so the restore escalates critically and the install
// reports the failure.
func TestDigestInstallEscalatesWhenTheSwapCannotBePutBack(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t, true)
	f.exit = 1
	townRoot := t.TempDir()
	oldPair := update.ImagePair{
		Daemon:  "registry.internal:5000/town/gastown-daemon@sha256:" + strings.Repeat("e", 64),
		Polecat: "registry.internal:5000/town/gastown-polecat@sha256:" + strings.Repeat("f", 64),
		Commit:  rollbackOldCommit,
	}
	newPair := update.ImagePair{Daemon: swapDaemonRef, Polecat: swapPolecatRef, Commit: rollbackNewCommit}
	ledger := update.FileImageLedger{Dir: update.Dir(townRoot)}
	if err := ledger.Write(update.ImageRecord{Current: oldPair}); err != nil {
		t.Fatalf("record the running pair: %v", err)
	}
	values := filepath.Join(townRoot, "images.yaml")
	if err := imagevalues.Write(values, imagevalues.Values{Daemon: newPair.Daemon, Polecat: newPair.Polecat, Commit: newPair.Commit}); err != nil {
		t.Fatalf("write the values file: %v", err)
	}

	swap := f.swap("town")
	composeWrites := 0
	swap.writeCompose = func(path string, content []byte) error {
		composeWrites++
		if composeWrites == 2 {
			return errors.New("compose went read-only")
		}
		return daemonimage.WriteComposeFile(path, content)
	}
	clock := &rollbackTestClock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	notes := notifyfake.New()
	inst := &update.DigestInstaller{
		Inner: cmdHostInstaller{},
		Path:  values,
		Images: update.ImageDeps{
			Ledger: ledger,
			Swap:   swap,
			Verify: &rollbackTestSwap{},
			Clock:  clock,
			Sleep:  clock.sleep,
			Wait:   updateRestartWait,
			Poll:   updateRestartPoll,
			Notify: notes,
		},
	}
	res, err := inst.Install(context.Background(), newPair.Commit)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if update.ClassifyInstall(res) != update.ResultFailed || strings.Contains(res.Reason, "restored") {
		t.Fatalf("result = %+v, want a failed install that claims no restore", res)
	}
	escalations := notes.Escalations()
	if len(escalations) != 1 || escalations[0].Escalation.Severity != "critical" {
		t.Fatalf("escalations = %+v, want one critical escalation for the failed restore", escalations)
	}
	if got, err := ledger.Read(); err != nil || got.Current != newPair || got.Previous != oldPair {
		t.Errorf("record = %+v, %v; want the new pair running and the old one recorded as previous", got, err)
	}
}

// cmdHostInstaller stands in for the host install every cycle runs.
type cmdHostInstaller struct{}

func (cmdHostInstaller) Install(_ context.Context, sha string) (update.InstallResult, error) {
	return update.InstallResult{Event: "installed", Commit: sha, Prev: "-"}, nil
}

// lateDaemon is the daemon an end-to-end swap moves. It reports the commit of
// the last pair it was put on only once delay has passed on the fake clock, so
// the wait for the restart must poll before it sees the build.
type lateDaemon struct {
	clock  *rollbackTestClock
	delay  time.Duration
	commit string
	putAt  time.Time
}

func (d *lateDaemon) DaemonVersion(context.Context) (string, error) {
	if d.clock.now.Sub(d.putAt) < d.delay {
		return "", nil
	}
	return d.commit, nil
}

func (d *lateDaemon) GTVersion(context.Context) (string, error)  { return "", nil }
func (d *lateDaemon) ImageLabel(context.Context) (string, error) { return "", nil }
func (d *lateDaemon) Health(context.Context) (string, error)     { return "", nil }

// lateSwap puts a pair in force through the real compose swap and starts the
// daemon's build clock from the moment the pair is put in force.
type lateSwap struct {
	swap   update.ImageSwap
	daemon *lateDaemon
}

func (s lateSwap) Apply(ctx context.Context, pair update.ImagePair) error {
	if err := s.swap.Apply(ctx, pair); err != nil {
		return err
	}
	s.daemon.commit = pair.Commit
	s.daemon.putAt = s.daemon.clock.now
	return nil
}

// TestUpdateImagesFlagInstallsThePairThroughRunUpdate drives gt update --images
// through runUpdate to a successful swap. The daemon comes up ten minutes after
// the swap, inside the fifteen-minute restart bound, so the wait must poll until
// it sees the new build and then succeed.
func TestUpdateImagesFlagInstallsThePairThroughRunUpdate(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t, true)
	townRoot := t.TempDir()
	oldPair := update.ImagePair{
		Daemon:  "registry.internal:5000/town/gastown-daemon@sha256:" + strings.Repeat("e", 64),
		Polecat: "registry.internal:5000/town/gastown-polecat@sha256:" + strings.Repeat("f", 64),
		Commit:  rollbackOldCommit,
	}
	newPair := update.ImagePair{Daemon: swapDaemonRef, Polecat: swapPolecatRef, Commit: rollbackNewCommit}
	ledger := update.FileImageLedger{Dir: update.Dir(townRoot)}
	if err := ledger.Write(update.ImageRecord{Current: oldPair}); err != nil {
		t.Fatalf("record the running pair: %v", err)
	}
	values := filepath.Join(townRoot, "images.yaml")
	if err := imagevalues.Write(values, imagevalues.Values{Daemon: newPair.Daemon, Polecat: newPair.Polecat, Commit: newPair.Commit}); err != nil {
		t.Fatalf("write the values file: %v", err)
	}

	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	clock := &rollbackTestClock{now: start}
	daemon := &lateDaemon{clock: clock, delay: 10 * time.Minute}
	ran := false
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return townRoot, nil },
		DepsFactory: func(string) (update.Deps, error) {
			return update.Deps{
				Store:     &update.Store{Dir: update.Dir(townRoot)},
				Clock:     clock,
				Sleep:     clock.sleep,
				Verify:    daemon,
				Installer: cmdHostInstaller{},
			}, nil
		},
		Run: func(ctx context.Context, deps update.Deps, _ update.Options) (update.State, error) {
			res, err := deps.Installer.Install(ctx, newPair.Commit)
			if err != nil {
				return update.State{}, err
			}
			if update.ClassifyInstall(res) != update.ResultOK {
				return update.State{Outcome: update.OutcomeFailed}, nil
			}
			ran = true
			return update.State{Outcome: update.OutcomeOK}, nil
		},
		ImageSwap: func(string) (update.ImageSwap, error) {
			return lateSwap{swap: f.swap("town"), daemon: daemon}, nil
		},
		Notifier: func(string) notify.Notifier { return notifyfake.New() },
	}
	c := newUpdateCmdWith(seams)
	c.SetArgs([]string{"--images", values})
	c.SetOut(new(strings.Builder))
	c.SetErr(new(strings.Builder))
	if err := c.Execute(); err != nil {
		t.Fatalf("gt update --images: %v", err)
	}
	if !ran {
		t.Fatal("the run did not reach a successful install")
	}
	if waited := clock.now.Sub(start); waited < daemon.delay || waited >= updateRestartWait {
		t.Errorf("the wait for the daemon took %v, want at least %v and within the %v restart bound", waited, daemon.delay, updateRestartWait)
	}
	if got, err := ledger.Read(); err != nil || got.Current != newPair || got.Previous != oldPair {
		t.Errorf("record = %+v, %v; want the new pair running and the old one recorded as previous", got, err)
	}
	if got := runnerImage(t, f.config); got != swapPolecatRef {
		t.Errorf("runner image = %q, want the pair's polecat digest", got)
	}
	if got := readSwapFile(t, f.compose); !strings.Contains(got, swapDaemonRef) {
		t.Error("compose file does not name the pair's daemon digest")
	}
}

// readSwapFile reads a fixture file, failing the test when it cannot.
func readSwapFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// runnerImage reads the tier sweep runner's image from the settings file.
func runnerImage(t *testing.T, path string) string {
	t.Helper()
	cfg, err := config.LoadDaemonPatrolConfig(path)
	if err != nil {
		t.Fatalf("read the daemon settings: %v", err)
	}
	return cfg.Patrols.TierSweep.Runner.Image
}
