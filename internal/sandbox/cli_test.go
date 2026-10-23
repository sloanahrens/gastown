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
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %v, want exactly one", f.Calls)
	}
	if !reflect.DeepEqual(f.Calls[0], want) {
		t.Errorf("argv =\n  %q\nwant\n  %q", f.Calls[0], want)
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
		"--env", "A=1", "--env", "Z=2",
		"--detach", "--output", "json",
		"--", "tmux", "-L", "gt", "new-session", "-d", "-s", "gt-r-n")
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
			Status{Name: "x", Phase: "Ready", Ready: true}},
		{"provisioning", `{"name":"x","phase":"Provisioning","conditions":[{"type":"Ready","status":"False"}]}`,
			Status{Name: "x", Phase: "Provisioning"}},
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

func TestExecArgv(t *testing.T) {
	t.Parallel()
	cli, f := newFakeCLI()
	f.Reply = map[string]string{"exec": "out\n"}
	stdout, _, err := cli.Exec(context.Background(), "gt-r-n",
		map[string]string{"K": "V", "A": "B"}, []string{"tmux", "-L", "gt", "has-session"}, []byte("in"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stdout) != "out\n" {
		t.Errorf("stdout = %q", stdout)
	}
	wantCall(t, f,
		"openshell", "-g", "openshell", "sandbox", "exec", "-n", "gt-r-n",
		"--no-tty", "--no-login-shell", "--env", "A=B", "--env", "K=V",
		"--", "tmux", "-L", "gt", "has-session")
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
			cli := &CLI{Run: func(ctx context.Context, _ []string, _ []byte) ([]byte, []byte, error) {
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
	cli := &CLI{Run: func(ctx context.Context, _ []string, _ []byte) ([]byte, []byte, error) {
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
