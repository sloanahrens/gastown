package doctor

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// The command lines the check runs for the default town below, keyed as the
// runner sees them.
const (
	osStatus    = "openshell -g openshell status --output json"
	osProviders = "openshell -g openshell provider list --output json"
	dockerImage = "docker image inspect gastown-polecat:dev"
	launchd     = "launchctl print gui/501/com.gastown.openshell-gateway"
)

// openShellFake answers commands by their joined argv. A command with no reply
// and no error is an unexpected call, and fails the test through its error.
type openShellFake struct {
	replies map[string]string
	errs    map[string]error
	calls   []string
}

// healthyOpenShell answers every command the default town runs as healthy.
func healthyOpenShell() *openShellFake {
	return &openShellFake{
		replies: map[string]string{
			osStatus:    `{"status":"connected","gateway":"openshell"}`,
			osProviders: `{"providers":[{"name":"deepseek"}]}`,
			dockerImage: `[]`,
			launchd:     `state = running`,
		},
		errs: map[string]error{},
	}
}

func (f *openShellFake) run(argv []string) ([]byte, error) {
	key := strings.Join(argv, " ")
	f.calls = append(f.calls, key)
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	out, ok := f.replies[key]
	if !ok {
		return nil, fmt.Errorf("unexpected command %q", key)
	}
	return []byte(out), nil
}

// openShellTown is a town with one openshell seat on the sandbox profile p.
func openShellTown(p config.SandboxProfile) *config.TownSettings {
	return &config.TownSettings{
		PolecatPool: &config.PolecatPool{Seats: map[string]config.SeatConfig{
			"deepseek-flash": {Cap: 1, Backend: config.BackendOpenShell, Sandbox: "polecat-default"},
		}},
		Sandboxes: map[string]config.SandboxProfile{"polecat-default": p},
	}
}

func defaultProfile() config.SandboxProfile {
	return config.SandboxProfile{Image: "gastown-polecat:dev", Policy: "deploy/polecat/policy.yaml", Providers: []string{"deepseek"}}
}

func openShellCheck(f *openShellFake, ts *config.TownSettings, goos string) *OpenShellGatewayCheck {
	c := NewOpenShellGatewayCheck()
	c.run = f.run
	c.settings = func(string) (*config.TownSettings, error) { return ts, nil }
	c.goos = goos
	c.uid = func() int { return 501 }
	return c
}

func TestOpenShellGatewayCheck_AllGood_OK(t *testing.T) {
	t.Parallel()
	f := healthyOpenShell()

	result := openShellCheck(f, openShellTown(defaultProfile()), "darwin").Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; message: %s; details: %v", result.Status, result.Message, result.Details)
	}
	if len(f.calls) != 4 {
		t.Errorf("ran %d commands, want 4: %v", len(f.calls), f.calls)
	}
}

func TestOpenShellGatewayCheck_NoOpenShellSeats_OKWithoutRunning(t *testing.T) {
	t.Parallel()
	f := &openShellFake{}
	ts := &config.TownSettings{PolecatPool: &config.PolecatPool{Seats: map[string]config.SeatConfig{
		"deepseek-flash": {Cap: 1},
	}}}

	result := openShellCheck(f, ts, "darwin").Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusOK || result.Message != "no openshell seats" {
		t.Fatalf("Status = %v, Message = %q; want StatusOK no-op", result.Status, result.Message)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v, want no commands", f.calls)
	}
}

func TestOpenShellGatewayCheck_Warnings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		profile config.SandboxProfile
		setup   func(f *openShellFake)
		goos    string
		// want is a substring each of these detail lines must contain.
		want []string
	}{
		{
			name: "gateway unreachable names openshell status",
			setup: func(f *openShellFake) {
				f.replies[osStatus] = `{"status":"unreachable"}`
			},
			goos: "darwin",
			want: []string{"openshell status: gateway openshell reports \"unreachable\", not connected"},
		},
		{
			name: "gateway command fails names openshell status",
			setup: func(f *openShellFake) {
				f.errs[osStatus] = errors.New("exit status 1: connection refused")
			},
			goos: "darwin",
			want: []string{"openshell status: gateway openshell did not answer"},
		},
		{
			name: "status reply that is not json names the decode",
			setup: func(f *openShellFake) {
				f.replies[osStatus] = `connected`
			},
			goos: "darwin",
			want: []string{"openshell status: gateway openshell: decode"},
		},
		{
			name: "missing provider is named",
			setup: func(f *openShellFake) {
				f.replies[osProviders] = `{"providers":[{"name":"forgejo-polecat"}]}`
			},
			goos: "darwin",
			want: []string{`provider "deepseek"`},
		},
		{
			name: "provider list fails names openshell provider list",
			setup: func(f *openShellFake) {
				f.errs[osProviders] = errors.New("exit status 1")
			},
			goos: "darwin",
			want: []string{"openshell provider list: gateway openshell"},
		},
		{
			name: "provider reply that is not json names the decode",
			setup: func(f *openShellFake) {
				f.replies[osProviders] = `deepseek`
			},
			goos: "darwin",
			want: []string{"openshell provider list: gateway openshell: decode"},
		},
		{
			name: "missing image is named",
			setup: func(f *openShellFake) {
				f.errs[dockerImage] = errors.New("exit status 1: No such image")
			},
			goos: "darwin",
			want: []string{"docker image inspect: image gastown-polecat:dev"},
		},
		{
			name: "launchd agent not loaded is named",
			setup: func(f *openShellFake) {
				f.errs[launchd] = errors.New("exit status 113")
			},
			goos: "darwin",
			want: []string{"com.gastown.openshell-gateway: agent is not loaded"},
		},
		{
			name: "unreachable gateway skips its provider list and reports the rest",
			setup: func(f *openShellFake) {
				f.replies[osStatus] = `{"status":"unreachable"}`
				f.errs[dockerImage] = errors.New("exit status 1")
			},
			goos: "darwin",
			want: []string{"openshell status: gateway openshell", "docker image inspect: image gastown-polecat:dev"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := healthyOpenShell()
			tt.setup(f)

			result := openShellCheck(f, openShellTown(defaultProfile()), tt.goos).Run(&CheckContext{TownRoot: t.TempDir()})

			if result.Status != StatusWarning {
				t.Fatalf("Status = %v, want StatusWarning; message: %s", result.Status, result.Message)
			}
			if len(result.Details) != len(tt.want) {
				t.Fatalf("got %d detail lines, want %d (one per missing piece): %v", len(result.Details), len(tt.want), result.Details)
			}
			for i, sub := range tt.want {
				if !strings.Contains(result.Details[i], sub) {
					t.Errorf("detail %d = %q, want it to contain %q", i, result.Details[i], sub)
				}
			}
		})
	}
}

func TestOpenShellGatewayCheck_LaunchdNotCheckedOffDarwin(t *testing.T) {
	t.Parallel()
	f := healthyOpenShell()
	f.errs[launchd] = errors.New("no such command")

	result := openShellCheck(f, openShellTown(defaultProfile()), "linux").Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details: %v", result.Status, result.Details)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "launchctl") {
			t.Errorf("ran %q off darwin", call)
		}
	}
}

func TestOpenShellGatewayCheck_UnreadableSettings_Warns(t *testing.T) {
	t.Parallel()
	c := NewOpenShellGatewayCheck()
	c.settings = func(string) (*config.TownSettings, error) { return nil, errors.New("bad json") }

	result := c.Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message: %s", result.Status, result.Message)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "bad json") {
		t.Errorf("Details = %v, want the settings read error", result.Details)
	}
}

func TestOpenShellGatewayCheck_UnreadableSeats_Warns(t *testing.T) {
	t.Parallel()
	f := healthyOpenShell()
	ts := openShellTown(defaultProfile())
	ts.PolecatPool.Timezone = "Not/AZone"

	result := openShellCheck(f, ts, "darwin").Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; message: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "polecat_pool.seats") {
		t.Errorf("Message = %q, want it to name polecat_pool.seats", result.Message)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v, want no commands once the seats are unreadable", f.calls)
	}
}

func TestOpenShellGatewayCheck_NoDocker_WarnsOnceNotOK(t *testing.T) {
	t.Parallel()
	f := healthyOpenShell()
	f.errs[dockerImage] = fmt.Errorf("docker: %w", exec.ErrNotFound)

	result := openShellCheck(f, openShellTown(defaultProfile()), "darwin").Run(&CheckContext{TownRoot: t.TempDir()})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning so the skipped image check is visible; message: %s", result.Status, result.Message)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "docker CLI not on PATH") {
		t.Errorf("Details = %v, want one line saying the docker CLI is not on PATH", result.Details)
	}
}

func TestRunOpenShellCommand_MissingProgramIsErrNotFound(t *testing.T) {
	t.Parallel()
	_, err := runOpenShellCommand([]string{"gt-no-such-openshell-program"})

	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("err = %v, want it to wrap exec.ErrNotFound so the image check can tell a missing docker from a failed inspect", err)
	}
}

func TestOpenShellGatewayCheck_NameAndNoFix(t *testing.T) {
	t.Parallel()
	c := NewOpenShellGatewayCheck()
	if c.Name() != "openshell-gateway" {
		t.Errorf("Name() = %q, want openshell-gateway", c.Name())
	}
	if c.CanFix() {
		t.Error("CanFix() = true, want false: the check reports and never repairs")
	}
}
