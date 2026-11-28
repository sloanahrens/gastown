package polecat

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat/cachevol"
	"github.com/steveyegge/gastown/internal/session"
)

// envRecordingBackend keeps the environment its Prepare receives.
type envRecordingBackend struct {
	*pathBackend
	prepareEnv map[string]string
}

func (b *envRecordingBackend) Prepare(ctx context.Context, spec session.SpawnSpec) (*session.Prepared, error) {
	b.prepareEnv = spec.Env
	return b.pathBackend.Prepare(ctx, spec)
}

// launchSeatGoFlags launches seat-a through a fake sandbox backend (sandbox) or
// with none, on a seat with pool's entry and an agent env of agentGOFLAGS. It
// returns the env Prepare received, the startup command, and the GOFLAGS the
// spawn logged for the seat.
func launchSeatGoFlags(t *testing.T, sandbox bool, pool *config.PolecatPool, agentGOFLAGS string) (prepareEnv map[string]string, command, logged string, err error) {
	t.Helper()
	r := newLaunchRigAt(t, "gastown/polecats/p/gastown")
	agent := &config.RuntimeConfig{Command: "claude", Args: []string{"--dangerously-skip-permissions"}}
	if agentGOFLAGS != "" {
		agent.Env = map[string]string{"GOFLAGS": agentGOFLAGS}
	}
	ts := config.NewTownSettings()
	ts.Agents = map[string]*config.RuntimeConfig{"seat-a": agent}
	if err = config.SaveTownSettings(config.TownSettingsPath(r.town), ts); err != nil {
		t.Fatal(err)
	}
	r.m.seatPool = func(string) (*config.PolecatPool, error) { return pool, nil }
	r.m.logf = func(format string, args ...any) {
		if flags, ok := strings.CutPrefix(fmt.Sprintf(format, args...), "seat seat-a: GOFLAGS="); ok {
			logged = flags
		}
	}
	rec := &envRecordingBackend{pathBackend: &pathBackend{
		preparingBackend: preparingBackend{town: r.town},
		workDir:          "/sandbox/gt/gastown/polecats/p/gastown",
	}}
	var backend session.Backend
	if sandbox {
		backend = rec
	}
	// The capturing tmux refuses the session, ending the launch after the
	// command is recorded.
	err = r.m.launch("box", r.m.SessionName("box"), SessionStartOptions{
		WorkDir:          r.workDir,
		Agent:            "seat-a",
		Backend:          backend,
		RuntimeConfigDir: filepath.Join(r.town, "runtime-config"),
	})
	return rec.prepareEnv, r.tm.command, logged, err
}

// gcflagWords returns the -gcflags words of a GOFLAGS value, in order.
func gcflagWords(goflags string) (words []string) {
	for _, f := range strings.Fields(goflags) {
		if strings.HasPrefix(f, "-gcflags") {
			words = append(words, f)
		}
	}
	return words
}

// A sandbox seat keeps the image's compile flag whatever its seat type says: a
// GOFLAGS that replaces the image's loses the cache key the seed shares
// (gt-efdlfv). A tmux launch gains nothing.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestSandboxSeatGoFlagsCarryTheImageCompileFlag(t *testing.T) {
	//testpolicy:allow no-env — the spawning GOFLAGS is part of the value under test, so the test clears it
	t.Setenv("GOFLAGS", "")
	compile := cachevol.GoCompileFlags
	withFlags := func(flags string) *config.PolecatPool {
		return &config.PolecatPool{Seats: map[string]config.SeatConfig{"seat-a": {Cap: 1, GoFlags: flags}}}
	}
	tests := []struct {
		name    string
		sandbox bool
		pool    *config.PolecatPool
		agent   string
		want    []string // the -gcflags words the GOFLAGS carries
	}{
		{"sandbox seat with no go_flags", true, withFlags(""), "", []string{compile}},
		{"sandbox seat with no pool entry", true, &config.PolecatPool{}, "", []string{compile}},
		{"sandbox seat whose go_flags name it", true, withFlags(compile), "", []string{compile}},
		{"sandbox agent GOFLAGS name another -gcflags", true, withFlags(""), "-gcflags=all=-N", []string{"-gcflags=all=-N"}},
		{"sandbox agent GOFLAGS name it", true, withFlags(""), compile, []string{compile}},
		{"tmux seat with no go_flags", false, withFlags(""), "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepareEnv, command, logged, err := launchSeatGoFlags(t, tt.sandbox, tt.pool, tt.agent)
			if logged == "" {
				t.Fatalf("the spawn logged no seat GOFLAGS (launch error: %v)", err)
			}
			if got := gcflagWords(logged); !slices.Equal(got, tt.want) {
				t.Errorf("logged GOFLAGS %q carries -gcflags %q, want %q", logged, got, tt.want)
			}
			if tt.sandbox && !slices.Equal(gcflagWords(prepareEnv["GOFLAGS"]), tt.want) {
				t.Errorf("Prepare's GOFLAGS %q carries the wrong -gcflags, want %q", prepareEnv["GOFLAGS"], tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(command, w) {
					t.Errorf("the startup command lacks %q:\n%s", w, command)
				}
			}
		})
	}
}

// A seat whose go_flags the loader refuses still fails a sandbox launch before
// any session starts.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestSandboxSeatGoFlagsRefusesAnUnacceptedSeatFlag(t *testing.T) {
	//testpolicy:allow no-env — the spawning GOFLAGS is part of the value under test, so the test clears it
	t.Setenv("GOFLAGS", "")
	pool := &config.PolecatPool{Seats: map[string]config.SeatConfig{"seat-a": {Cap: 1, GoFlags: "-toolexec=sneaky"}}}

	_, command, _, err := launchSeatGoFlags(t, true, pool, "")

	if err == nil || !strings.Contains(err.Error(), "-toolexec=sneaky") {
		t.Fatalf("launch error = %v, want the refused flag named", err)
	}
	if command != "" {
		t.Errorf("the session was started despite the refused go flag:\n%s", command)
	}
}
