//go:build !integration

package sandbox

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

func newFakeCLI() (*CLI, *Fake) {
	f := &Fake{}
	return &CLI{Gateway: "openshell", Run: f.Runner()}, f
}

func wantCall(t *testing.T, f *Fake, want ...string) {
	t.Helper()
	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want exactly one", calls)
	}
	if !reflect.DeepEqual(calls[0], want) {
		t.Errorf("argv =\n  %q\nwant\n  %q", calls[0], want)
	}
}

func TestCreateArgv(t *testing.T) {
	t.Parallel()
	cli, f := newFakeCLI()
	err := cli.Create(context.Background(), CreateSpec{
		Name:       "gt-r-n",
		Image:      "img",
		PolicyPath: "p.yaml",
		Providers:  []string{"a", "b"},
		CPU:        "4",
		Memory:     "8Gi",
		Labels:     map[string]string{"rig": "r", "polecat": "n"},
		Env:        map[string]string{"Z": "2", "A": "1"},
		Command:    []string{"tmux", "-L", "gt", "new-session", "-d", "-s", "gt-r-n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCall(t, f,
		"openshell", "-g", "openshell", "sandbox", "create", "--name", "gt-r-n",
		"--from", "img", "--policy", "p.yaml", "--provider", "a", "--provider", "b",
		"--cpu", "4", "--memory", "8Gi",
		"--label", "polecat=n", "--label", "rig=r",
		"--env-from", "A=GT_SANDBOX_ENV_A", "--env-from", "Z=GT_SANDBOX_ENV_Z",
		"--detach", "--output", "json",
		"--", "tmux", "-L", "gt", "new-session", "-d", "-s", "gt-r-n")

	wantEnv := []string{"GT_SANDBOX_ENV_A=1", "GT_SANDBOX_ENV_Z=2"}
	if got := f.Envs(); len(got) != 1 || !reflect.DeepEqual(got[0], wantEnv) {
		t.Errorf("env = %v, want %v; the values must reach openshell's process environment", got, wantEnv)
	}
	for _, arg := range f.Calls()[0] {
		if strings.Contains(arg, "=1") || strings.Contains(arg, "=2") {
			t.Errorf("argv carries %q; an env value must stay off the command line", arg)
		}
	}
}

func TestCreateDriverConfigAndOmittedFields(t *testing.T) {
	t.Parallel()
	cli, f := newFakeCLI()
	if err := cli.Create(context.Background(), CreateSpec{Name: "x", DriverConfigJSON: `{"v":1}`}); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f,
		"openshell", "-g", "openshell", "sandbox", "create", "--name", "x",
		"--driver-config-json", `{"v":1}`, "--detach", "--output", "json")
}

func TestBinAndGatewayDefaults(t *testing.T) {
	t.Parallel()
	f := &Fake{}
	cli := &CLI{Run: f.Runner()}
	if err := cli.Stop(context.Background(), "n"); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f, "openshell", "sandbox", "stop", "n")

	f2 := &Fake{}
	cli = &CLI{Bin: "/opt/os", Gateway: "g", Run: f2.Runner()}
	if err := cli.Stop(context.Background(), "n"); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f2, "/opt/os", "-g", "g", "sandbox", "stop", "n")
}

func TestGetArgvAndDecode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		reply string
		want  Status
	}{
		{"ready", `{"name":"x","phase":"Ready","conditions":[{"type":"Ready","status":"True"}]}`,
			Status{Name: "x", Phase: "Ready", Ready: true, ReadyKnown: true}},
		{"provisioning", `{"name":"x","phase":"Provisioning","conditions":[{"type":"Ready","status":"False"}]}`,
			Status{Name: "x", Phase: "Provisioning", ReadyKnown: true}},
		{"other condition true", `{"name":"x","phase":"Provisioning","conditions":[{"type":"Scheduled","status":"True"}]}`,
			Status{Name: "x", Phase: "Provisioning"}},
		{"no conditions", `{"name":"x","phase":"Stopped"}`,
			Status{Name: "x", Phase: "Stopped"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cli, f := newFakeCLI()
			f.Reply = map[string]string{"get": tt.reply}
			got, err := cli.Get(context.Background(), "x")
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("status = %+v, want %+v", got, tt.want)
			}
			wantCall(t, f, "openshell", "-g", "openshell", "sandbox", "get", "x", "--output", "json")
		})
	}
}

func TestGetRejectsNonJSON(t *testing.T) {
	t.Parallel()
	cli, f := newFakeCLI()
	f.Reply = map[string]string{"get": "not json"}
	if _, err := cli.Get(context.Background(), "x"); err == nil {
		t.Fatal("want a decode error")
	}
}

func TestExecRefusesEnvValues(t *testing.T) {
	t.Parallel()
	cli, f := newFakeCLI()
	_, _, err := cli.Exec(context.Background(), "gt-r-n",
		map[string]string{"K": "V"}, []string{"tmux", "-L", "gt", "has-session"}, nil)
	if err == nil {
		t.Fatal("Exec with env values: want an error, since they would go on the argv")
	}
	if n := len(f.Calls()); n != 0 {
		t.Errorf("calls = %d; a refused exec must not start openshell", n)
	}
}

func TestExecReturnsStdout(t *testing.T) {
	t.Parallel()
	cli, f := newFakeCLI()
	f.Reply = map[string]string{"exec": "out\n"}
	stdout, _, err := cli.Exec(context.Background(), "gt-r-n", nil, []string{"tmux", "-L", "gt", "has-session"}, []byte("in"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stdout) != "out\n" {
		t.Errorf("stdout = %q", stdout)
	}
	wantCall(t, f,
		"openshell", "-g", "openshell", "sandbox", "exec", "-n", "gt-r-n",
		"--no-tty", "--no-login-shell", "--", "tmux", "-L", "gt", "has-session")
}

func TestCreateRedactsEnvValuesFromStderr(t *testing.T) {
	t.Parallel()
	cli, f := newFakeCLI()
	f.Err = map[string]error{"create": errors.New("exit status 1")}
	// The value straddles the point where the stderr cut keeps only its tail,
	// so redacting after the cut would leave "e-xyz" in the error.
	f.Stderr = map[string]string{"create": "secret-value-xyz" + strings.Repeat("y", stderrLimit-5)}
	err := cli.Create(context.Background(), CreateSpec{Name: "n", Env: map[string]string{"TOKEN": "secret-value-xyz"}})
	if err == nil {
		t.Fatal("want the create error")
	}
	for _, leak := range []string{"secret", "e-xyz"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error carries %q from the env value: %.60q...", leak, err.Error())
		}
	}
}

func TestExecRunnerDropsInheritedEnvValues(t *testing.T) {
	t.Setenv("GT_SANDBOX_ENV_STALE", "old")
	stdout, _, err := execRunner(context.Background(), []string{"env"}, nil, []string{"GT_SANDBOX_ENV_NEW=new"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stdout), "GT_SANDBOX_ENV_STALE") {
		t.Error("child env carries an inherited GT_SANDBOX_ENV_ entry; only this call's values may reach openshell")
	}
	if !strings.Contains(string(stdout), "GT_SANDBOX_ENV_NEW=new") {
		t.Error("child env lacks the value this call set")
	}
}

func TestExecWithoutEnv(t *testing.T) {
	t.Parallel()
	cli, f := newFakeCLI()
	if _, _, err := cli.Exec(context.Background(), "n", nil, []string{"ls"}, nil); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f,
		"openshell", "-g", "openshell", "sandbox", "exec", "-n", "n",
		"--no-tty", "--no-login-shell", "--", "ls")
}

func TestUploadArgv(t *testing.T) {
	t.Parallel()
	cli, f := newFakeCLI()
	if err := cli.Upload(context.Background(), "n", "/tmp/skel.tgz", "/sandbox/"); err != nil {
		t.Fatal(err)
	}
	wantCall(t, f,
		"openshell", "-g", "openshell", "sandbox", "upload", "--no-git-ignore", "n", "/tmp/skel.tgz", "/sandbox/")
}

func TestStopStartDeleteArgv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		verb string
		call func(*CLI) error
	}{
		{"stop", func(c *CLI) error { return c.Stop(context.Background(), "n") }},
		{"start", func(c *CLI) error { return c.Start(context.Background(), "n") }},
		{"delete", func(c *CLI) error { return c.Delete(context.Background(), "n") }},
	}
	for _, tt := range tests {
		t.Run(tt.verb, func(t *testing.T) {
			t.Parallel()
			cli, f := newFakeCLI()
			if err := tt.call(cli); err != nil {
				t.Fatal(err)
			}
			wantCall(t, f, "openshell", "-g", "openshell", "sandbox", tt.verb, "n")
		})
	}
}

func TestLogsArgv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		since time.Duration
		want  string
	}{
		{5 * time.Minute, "5m"},
		{2 * time.Hour, "2h"},
		{90 * time.Second, "90s"},
		{1500 * time.Millisecond, "2s"},
		{0, "1s"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			cli, f := newFakeCLI()
			f.Reply = map[string]string{"logs": "line1\nline2\n"}
			got, err := cli.Logs(context.Background(), "n", tt.since)
			if err != nil {
				t.Fatal(err)
			}
			if got != "line1\nline2\n" {
				t.Errorf("logs = %q", got)
			}
			wantCall(t, f, "openshell", "-g", "openshell", "logs", "n", "--since", tt.want, "--source", "sandbox")
		})
	}
}

func TestRunnerErrorWrapsStderr(t *testing.T) {
	t.Parallel()
	boom := errors.New("exit status 1")
	cli, f := newFakeCLI()
	f.Err = map[string]error{"delete": boom}
	f.Stderr = map[string]string{"delete": "sandbox \"n\" not found\n"}
	err := cli.Delete(context.Background(), "n")
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err %v does not wrap the runner error", err)
	}
	if !strings.Contains(err.Error(), `sandbox "n" not found`) {
		t.Errorf("err %q does not carry stderr", err)
	}
}

func TestDeadlines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call func(*CLI) error
		want time.Duration // 0: no deadline added
	}{
		{"create", func(c *CLI) error { return c.Create(context.Background(), CreateSpec{Name: "n"}) }, 10 * time.Minute},
		{"get", func(c *CLI) error { _, err := c.Get(context.Background(), "n"); return err }, 2 * time.Minute},
		{"stop", func(c *CLI) error { return c.Stop(context.Background(), "n") }, 2 * time.Minute},
		{"exec", func(c *CLI) error {
			_, _, err := c.Exec(context.Background(), "n", nil, []string{"x"}, nil)
			return err
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got time.Duration
			var has bool
			cli := &CLI{Run: func(ctx context.Context, _ []string, _ []byte, _ []string) ([]byte, []byte, error) {
				if d, ok := ctx.Deadline(); ok {
					has, got = true, time.Until(d)
				}
				return []byte(`{}`), nil, nil
			}}
			if err := tt.call(cli); err != nil {
				t.Fatal(err)
			}
			if tt.want == 0 {
				if has {
					t.Errorf("deadline = %v, want none", got)
				}
				return
			}
			// time.Until ran a moment after the deadline was set.
			if !has || got > tt.want || got < tt.want-time.Minute {
				t.Errorf("deadline = %v (set %v), want about %v", got, has, tt.want)
			}
		})
	}
}

func TestCallerDeadlineWinsOverDefault(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var got time.Duration
	cli := &CLI{Run: func(ctx context.Context, _ []string, _ []byte, _ []string) ([]byte, []byte, error) {
		d, _ := ctx.Deadline()
		got = time.Until(d)
		return nil, nil, nil
	}}
	if err := cli.Stop(ctx, "n"); err != nil {
		t.Fatal(err)
	}
	if got > time.Second {
		t.Errorf("deadline = %v, want the caller's 1s", got)
	}
}

func TestErrorWithoutStderrHasNoDanglingSeparator(t *testing.T) {
	t.Parallel()
	f := &Fake{Err: map[string]error{"delete": errors.New("exit status 1")}}
	err := (&CLI{Run: f.Runner()}).Delete(context.Background(), "n")
	if err == nil {
		t.Fatal("want an error")
	}
	if got, want := err.Error(), "openshell sandbox delete: exit status 1"; got != want {
		t.Errorf("err = %q, want %q", got, want)
	}
}

func TestErrorBoundsStderr(t *testing.T) {
	t.Parallel()
	f := &Fake{Err: map[string]error{"delete": errors.New("exit status 1")}}
	f.Stderr = map[string]string{"delete": strings.Repeat("x", stderrLimit+500) + " tail reason\n"}
	err := (&CLI{Run: f.Runner()}).Delete(context.Background(), "n")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if len(msg) > stderrLimit+200 {
		t.Errorf("error is %d bytes; stderr must be bounded", len(msg))
	}
	if !strings.Contains(msg, "(truncated)") {
		t.Errorf("error %q does not mark where it was cut", msg)
	}
	if !strings.Contains(msg, "tail reason") {
		t.Errorf("error %q drops the end of stderr, where the reason is", msg)
	}
}

func TestVerbOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{"short gateway", []string{"openshell", "-g", "gw", "sandbox", "create"}, "create"},
		{"joined gateway", []string{"openshell", "-g=gw", "sandbox", "get"}, "get"},
		{"long gateway", []string{"openshell", "--gateway", "gw", "logs"}, "logs"},
		{"flag before noun", []string{"openshell", "-v", "sandbox", "stop"}, "stop"},
		{"bare noun", []string{"openshell", "sandbox", "delete"}, "delete"},
		{"no noun", []string{"openshell", "logs"}, "logs"},
		{"sandbox name is a verb", []string{"openshell", "sandbox", "get", "create"}, "get"},
		{"name without noun", []string{"openshell", "logs", "delete"}, "logs"},
		{"no verb", []string{"openshell", "-g", "gw", "sandbox"}, ""},
		{"dangling flag", []string{"openshell", "-g"}, ""},
		{"nothing", nil, ""},
		{"unknown positional", []string{"openshell", "never"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := verbOf(tt.argv); got != tt.want {
				t.Errorf("verbOf(%q) = %q, want %q", tt.argv, got, tt.want)
			}
		})
	}
}

func TestFakeCallsIsACopy(t *testing.T) {
	t.Parallel()
	cli, f := newFakeCLI()
	if err := cli.Stop(context.Background(), "n"); err != nil {
		t.Fatal(err)
	}
	f.Calls()[0][0] = "mutated"
	if got := f.Calls()[0][0]; got != "openshell" {
		t.Errorf("Calls() aliases the recording: later read %q", got)
	}
}
