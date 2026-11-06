package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

const (
	planTestData   = "/town dir/.dolt-data"
	planTestBackup = "/backup dir/dolt"

	// planTestListenerPort is the listener.port in the config file
	// writePlanTestConfig writes.
	planTestListenerPort = 3306
)

// writePlanTestConfig writes a Dolt config.yaml whose listener names
// planTestListenerPort in a temporary directory and returns its path.
func writePlanTestConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "listener:\n  port: " + strconv.Itoa(planTestListenerPort) + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write the test config: %v", err)
	}
	return path
}

// planTestDeps fails the test when the command asks for a default path: every
// test that does not exercise the defaults passes both --data-dir and
// --backup-dir.
func planTestDeps(t *testing.T) doltContainerPlanDeps {
	t.Helper()
	refuse := func() (string, error) {
		t.Error("the command resolved a default path although both directories were given")
		return "", errors.New("unexpected default path lookup")
	}
	return doltContainerPlanDeps{townRoot: refuse, backupRoot: refuse}
}

// runDoltContainerPlan runs `gt dolt container plan` the way the shell does,
// through its flag parsing, writing what it prints to out.
func runDoltContainerPlan(deps doltContainerPlanDeps, out io.Writer, args ...string) error {
	cmd := newDoltContainerPlanCmd(deps)
	cmd.SetArgs(args)
	cmd.SetOut(out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	return cmd.Execute()
}

// executeDoltContainerPlan runs the command and returns what it printed.
func executeDoltContainerPlan(t *testing.T, deps doltContainerPlanDeps, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runDoltContainerPlan(deps, &out, args...)
	return out.String(), err
}

// failingPlanWriter is an output whose writes fail, as a closed pipe's do.
type failingPlanWriter struct{}

func (failingPlanWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

// planTestArgs are the flags every command test passes when it is not testing
// a missing flag; config is the path of the config file the plan reads.
func planTestArgs(config string) []string {
	return []string{
		"--name", "gt-dolt",
		"--image", "dolt:test",
		"--config", config,
		"--data-dir", planTestData,
		"--backup-dir", planTestBackup,
	}
}

// planTestSpec is the ContainerSpec the command builds from planTestArgs for
// the config file at config, whose listener names planTestListenerPort.
func planTestSpec(config string) doltserver.ContainerSpec {
	return doltserver.ContainerSpec{
		Name:          "gt-dolt",
		Image:         "dolt:test",
		DataDir:       planTestData,
		BackupDir:     planTestBackup,
		ConfigPath:    config,
		MemoryGiB:     8,
		HostPort:      3307,
		ContainerPort: planTestListenerPort,
		Restart:       "unless-stopped",
	}
}

// planTestJSON runs the command with --json and returns the argv it printed.
func planTestJSON(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := executeDoltContainerPlan(t, planTestDeps(t), append(args, "--json")...)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var argv []string
	if err := json.Unmarshal([]byte(out), &argv); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out)
	}
	return argv
}

func TestDoltContainerPlanCommand(t *testing.T) {
	t.Parallel()

	t.Run("one shell-quoted line", func(t *testing.T) {
		t.Parallel()
		config := writePlanTestConfig(t)
		out, err := executeDoltContainerPlan(t, planTestDeps(t), planTestArgs(config)...)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		// The data and backup paths hold spaces, so they are quoted; a
		// t.TempDir path holds no character that needs quoting.
		want := "docker create --name gt-dolt --memory 8g --memory-swap 8g --restart unless-stopped " +
			"-p 127.0.0.1:3307:" + strconv.Itoa(planTestListenerPort) + " " +
			"-v '" + planTestData + ":" + planTestData + "' " +
			"-v '" + planTestBackup + ":" + planTestBackup + "' " +
			"-v " + config + ":" + config + ":ro " +
			"--label org.gastown.keep=true " +
			"dolt:test --config " + config + "\n"
		if out != want {
			t.Fatalf("output =\n%s\nwant\n%s", out, want)
		}
	})

	t.Run("json array", func(t *testing.T) {
		t.Parallel()
		config := writePlanTestConfig(t)
		out, err := executeDoltContainerPlan(t, planTestDeps(t), append(planTestArgs(config), "--json")...)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		var got []string
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("output is not a JSON array: %v\n%s", err, out)
		}
		want, err := doltserver.ContainerPlan(planTestSpec(config))
		if err != nil {
			t.Fatalf("ContainerPlan: %v", err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("argv = %#v\nwant %#v", got, want)
		}
	})

	t.Run("flags are required", func(t *testing.T) {
		t.Parallel()
		all := planTestArgs("/config/config.yaml")
		for _, required := range []string{"name", "image", "config"} {
			required := required
			t.Run(required, func(t *testing.T) {
				t.Parallel()
				args := make([]string, 0, len(all))
				for i := 0; i < len(all); i += 2 {
					if all[i] != "--"+required {
						args = append(args, all[i], all[i+1])
					}
				}
				_, err := executeDoltContainerPlan(t, planTestDeps(t), args...)
				if err == nil {
					t.Fatalf("the command ran without --%s", required)
				}
				if !strings.Contains(err.Error(), required) {
					t.Fatalf("error %q does not name --%s", err, required)
				}
			})
		}
	})

	t.Run("a refused plan is an error", func(t *testing.T) {
		t.Parallel()
		args := planTestArgs(writePlanTestConfig(t))
		args[1] = "-bad"
		_, err := executeDoltContainerPlan(t, planTestDeps(t), args...)
		if err == nil {
			t.Fatal("the command printed a plan for a name docker reads as a flag")
		}
		if !strings.Contains(err.Error(), "container name") {
			t.Fatalf("error %q does not name the container name", err)
		}
	})

	t.Run("no positional arguments", func(t *testing.T) {
		t.Parallel()
		args := append(planTestArgs(writePlanTestConfig(t)), "extra")
		_, err := executeDoltContainerPlan(t, planTestDeps(t), args...)
		if err == nil {
			t.Fatal("the command accepted a positional argument")
		}
	})

	t.Run("a failed write is an error", func(t *testing.T) {
		t.Parallel()
		config := writePlanTestConfig(t)
		deps := planTestDeps(t)
		if err := runDoltContainerPlan(deps, failingPlanWriter{}, planTestArgs(config)...); err == nil {
			t.Fatal("the shell-quoted line ignored a failed write")
		}
		if err := runDoltContainerPlan(deps, failingPlanWriter{}, append(planTestArgs(config), "--json")...); err == nil {
			t.Fatal("the JSON form ignored a failed write")
		}
	})
}

func TestDoltContainerPlanHasKeepLabel(t *testing.T) {
	t.Parallel()

	argv := planTestJSON(t, planTestArgs(writePlanTestConfig(t))...)
	labelAt := slices.Index(argv, "--label")
	if labelAt < 0 || labelAt+1 >= len(argv) {
		t.Fatalf("argv %#v holds no --label pair", argv)
	}
	if got := argv[labelAt+1]; got != "org.gastown.keep=true" {
		t.Fatalf("label = %q, want org.gastown.keep=true", got)
	}
	imageAt := slices.Index(argv, "dolt:test")
	if imageAt < 0 {
		t.Fatalf("argv %#v holds no image reference", argv)
	}
	if labelAt > imageAt {
		t.Fatalf("the label at %d follows the image reference at %d", labelAt, imageAt)
	}
}

func TestDoltContainerPlanKeepsExistingFlags(t *testing.T) {
	t.Parallel()

	config := writePlanTestConfig(t)
	got := planTestJSON(t, planTestArgs(config)...)
	want, err := doltserver.ContainerPlan(planTestSpec(config))
	if err != nil {
		t.Fatalf("ContainerPlan: %v", err)
	}
	// Dropping the one keep-label pair from each side must leave every other
	// element, so the memory, restart, publish and mount flags keep their
	// values and their order.
	stripped := dropKeepLabelPair(got)
	if !slices.Equal(stripped, dropKeepLabelPair(want)) {
		t.Fatalf("plan without the label = %#v\nwant %#v", stripped, dropKeepLabelPair(want))
	}
}

// dropKeepLabelPair returns argv without its --label pair.
func dropKeepLabelPair(argv []string) []string {
	stripped := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--label" && i+1 < len(argv) {
			i++
			continue
		}
		stripped = append(stripped, argv[i])
	}
	return stripped
}

func TestDoltContainerPlanDefaults(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	backupRoot := t.TempDir()
	deps := doltContainerPlanDeps{
		townRoot:   func() (string, error) { return townRoot, nil },
		backupRoot: func() (string, error) { return backupRoot, nil },
	}
	out, err := executeDoltContainerPlan(t, deps,
		"--json", "--name", "gt-dolt", "--image", "dolt:test", "--config", writePlanTestConfig(t))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var argv []string
	if err := json.Unmarshal([]byte(out), &argv); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out)
	}
	dataDir := filepath.Join(townRoot, ".dolt-data")
	for _, want := range []string{dataDir + ":" + dataDir, backupRoot + ":" + backupRoot} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv %#v does not hold the default mount %q", argv, want)
		}
	}
}

// publishFlag returns the value of the -p pair in argv.
func publishFlag(t *testing.T, argv []string) string {
	t.Helper()
	at := slices.Index(argv, "-p")
	if at < 0 || at+1 >= len(argv) {
		t.Fatalf("argv %#v holds no -p pair", argv)
	}
	return argv[at+1]
}

func TestDoltContainerPlanReadsTheListenerPort(t *testing.T) {
	t.Parallel()

	argv := planTestJSON(t, planTestArgs(writePlanTestConfig(t))...)
	want := "127.0.0.1:3307:" + strconv.Itoa(planTestListenerPort)
	if got := publishFlag(t, argv); got != want {
		t.Fatalf("published port = %q, want the config's listener port %q", got, want)
	}
}

func TestDoltContainerPlanTrailingArguments(t *testing.T) {
	t.Parallel()

	config := writePlanTestConfig(t)
	argv := planTestJSON(t, planTestArgs(config)...)
	imageAt := slices.Index(argv, "dolt:test")
	if imageAt < 0 {
		t.Fatalf("argv %#v holds no image reference", argv)
	}
	want := []string{"dolt:test", "--config", config}
	if got := argv[imageAt:]; !slices.Equal(got, want) {
		t.Fatalf("argv after the flags = %#v, want %#v", got, want)
	}
}

func TestDoltContainerPlanContainerPortOverride(t *testing.T) {
	t.Parallel()

	config := writePlanTestConfig(t)
	argv := planTestJSON(t, append(planTestArgs(config), "--container-port", "3317")...)
	if got, want := publishFlag(t, argv), "127.0.0.1:3307:3317"; got != want {
		t.Fatalf("published port = %q, want the override %q", got, want)
	}
}

func TestDoltContainerPlanContainerPortOverrideReadsNoConfig(t *testing.T) {
	t.Parallel()

	// An explicit --container-port is the operator's port, so the plan takes
	// it without reading the config for one.
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	argv := planTestJSON(t, append(planTestArgs(missing), "--container-port", "3317")...)
	if got, want := publishFlag(t, argv), "127.0.0.1:3307:3317"; got != want {
		t.Fatalf("published port = %q, want the override %q", got, want)
	}
}

func TestDoltContainerPlanExplicitZeroContainerPortIsRefused(t *testing.T) {
	t.Parallel()

	config := writePlanTestConfig(t)
	_, err := executeDoltContainerPlan(t, planTestDeps(t),
		append(planTestArgs(config), "--container-port", "0")...)
	if err == nil {
		t.Fatal("the plan accepted --container-port 0")
	}
	if !strings.Contains(err.Error(), "container port") {
		t.Fatalf("error %q does not name the container port", err)
	}
}

func TestDoltContainerPlanDefaultMemory(t *testing.T) {
	t.Parallel()

	argv := planTestJSON(t, planTestArgs(writePlanTestConfig(t))...)
	for _, flag := range []string{"--memory", "--memory-swap"} {
		at := slices.Index(argv, flag)
		if at < 0 || at+1 >= len(argv) {
			t.Fatalf("argv %#v holds no %s pair", argv, flag)
		}
		if got := argv[at+1]; got != "8g" {
			t.Fatalf("%s = %q, want 8g", flag, got)
		}
	}
}

func TestDoltContainerPlanConfigWithoutAListenerPort(t *testing.T) {
	t.Parallel()

	writeConfig := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatalf("write the config: %v", err)
		}
		return path
	}

	t.Run("the plan is refused", func(t *testing.T) {
		t.Parallel()
		config := writeConfig(t, "log_level: warning\n")
		_, err := executeDoltContainerPlan(t, planTestDeps(t), planTestArgs(config)...)
		if err == nil {
			t.Fatal("the plan guessed a container port for a config that names none")
		}
	})

	t.Run("the error names the config file", func(t *testing.T) {
		t.Parallel()
		config := writeConfig(t, "log_level: warning\n")
		_, err := executeDoltContainerPlan(t, planTestDeps(t), planTestArgs(config)...)
		if err == nil {
			t.Fatal("the plan guessed a container port for a config that names none")
		}
		if !strings.Contains(err.Error(), config) {
			t.Fatalf("error %q does not name the config file %s", err, config)
		}
	})

	t.Run("the error points at the override", func(t *testing.T) {
		t.Parallel()
		config := writeConfig(t, "listener:\n  max_connections: 100\n")
		_, err := executeDoltContainerPlan(t, planTestDeps(t), planTestArgs(config)...)
		if err == nil {
			t.Fatal("the plan guessed a container port for a config that names none")
		}
		if !strings.Contains(err.Error(), "--container-port") {
			t.Fatalf("error %q does not point at --container-port", err)
		}
	})
}

func TestDoltContainerPlanDefaultLookupErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		deps doltContainerPlanDeps
		want string
	}{
		{
			name: "data directory",
			deps: doltContainerPlanDeps{
				townRoot:   func() (string, error) { return "", errors.New("not in a workspace") },
				backupRoot: func() (string, error) { return t.TempDir(), nil },
			},
			want: "--data-dir",
		},
		{
			name: "backup directory",
			deps: doltContainerPlanDeps{
				townRoot:   func() (string, error) { return t.TempDir(), nil },
				backupRoot: func() (string, error) { return "", errors.New("no home") },
			},
			want: "--backup-dir",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := executeDoltContainerPlan(t, tt.deps,
				"--name", "gt-dolt", "--image", "dolt:test", "--config", writePlanTestConfig(t))
			if err == nil {
				t.Fatal("the command ignored a failed default lookup")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not point at %s", err, tt.want)
			}
		})
	}
}

func TestDoltContainerPlanRequiresASubcommand(t *testing.T) {
	t.Parallel()

	cmd := newDoltContainerCmd(planTestDeps(t))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("gt dolt container ran without a subcommand")
	}
}

func TestDoltContainerPlanIsAttachedToDolt(t *testing.T) {
	t.Parallel()

	for _, c := range doltCmd.Commands() {
		if c.Name() != "container" {
			continue
		}
		for _, sub := range c.Commands() {
			if sub.Name() == "plan" {
				return
			}
		}
		t.Fatalf("gt dolt container has no plan subcommand: %v", c.Commands())
	}
	t.Fatalf("gt dolt has no container subcommand: %v", doltCmd.Commands())
}

// TestDoltContainerPlanRunsNoProcess reads the two non-test source files and
// fails on an os/exec or syscall import: printing a plan must not gain the
// ability to run a process. An import reached through another repo package is
// out of scope (internal/doltserver also holds the server start code).
func TestDoltContainerPlanRunsNoProcess(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"dolt_container_plan.go", filepath.Join("..", "doltserver", "containerplan.go")} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			pkg, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("parse %s: import path %s: %v", path, imp.Path.Value, err)
			}
			if pkg == "os/exec" || pkg == "syscall" {
				t.Errorf("%s imports %q: the plan must run no process", path, pkg)
			}
		}
	}
}
