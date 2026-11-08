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
			return update.State{}, nil
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
	_ = c.Execute()
	if !ran {
		t.Fatal("the run did not start")
	}
	if swapAsked != 0 {
		t.Errorf("the daemon swap was wired %d times for a run without --images, want none", swapAsked)
	}
}
