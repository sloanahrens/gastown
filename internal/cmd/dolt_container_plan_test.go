package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

const (
	planTestConfig = "/town dir/.dolt-data/config.yaml"
	planTestData   = "/town dir/.dolt-data"
	planTestBackup = "/backup dir/dolt"
)

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
// a missing flag.
func planTestArgs() []string {
	return []string{
		"--name", "gt-dolt",
		"--image", "dolt:test",
		"--config", planTestConfig,
		"--data-dir", planTestData,
		"--backup-dir", planTestBackup,
	}
}

func TestDoltContainerPlanCommand(t *testing.T) {
	t.Parallel()

	t.Run("one shell-quoted line", func(t *testing.T) {
		t.Parallel()
		out, err := executeDoltContainerPlan(t, planTestDeps(t), planTestArgs()...)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		want := "docker create --name gt-dolt --memory 12g --memory-swap 12g --restart unless-stopped " +
			"-p 127.0.0.1:3307:3307 " +
			"-v '" + planTestData + ":" + planTestData + "' " +
			"-v '" + planTestBackup + ":" + planTestBackup + "' " +
			"-v '" + planTestConfig + ":" + planTestConfig + ":ro' " +
			"dolt:test dolt sql-server --config '" + planTestConfig + "'\n"
		if out != want {
			t.Fatalf("output =\n%s\nwant\n%s", out, want)
		}
	})

	t.Run("json array", func(t *testing.T) {
		t.Parallel()
		out, err := executeDoltContainerPlan(t, planTestDeps(t), append(planTestArgs(), "--json")...)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		var got []string
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("output is not a JSON array: %v\n%s", err, out)
		}
		want, err := doltserver.ContainerPlan(doltserver.ContainerSpec{
			Name:          "gt-dolt",
			Image:         "dolt:test",
			DataDir:       planTestData,
			BackupDir:     planTestBackup,
			ConfigPath:    planTestConfig,
			MemoryGiB:     12,
			HostPort:      3307,
			ContainerPort: 3307,
			Restart:       "unless-stopped",
		})
		if err != nil {
			t.Fatalf("ContainerPlan: %v", err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("argv = %#v\nwant %#v", got, want)
		}
	})

	t.Run("flags are required", func(t *testing.T) {
		t.Parallel()
		all := planTestArgs()
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
		args := planTestArgs()
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
		_, err := executeDoltContainerPlan(t, planTestDeps(t), append(planTestArgs(), "extra")...)
		if err == nil {
			t.Fatal("the command accepted a positional argument")
		}
	})

	t.Run("a failed write is an error", func(t *testing.T) {
		t.Parallel()
		deps := planTestDeps(t)
		if err := runDoltContainerPlan(deps, failingPlanWriter{}, planTestArgs()...); err == nil {
			t.Fatal("the shell-quoted line ignored a failed write")
		}
		if err := runDoltContainerPlan(deps, failingPlanWriter{}, append(planTestArgs(), "--json")...); err == nil {
			t.Fatal("the JSON form ignored a failed write")
		}
	})
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
		"--json", "--name", "gt-dolt", "--image", "dolt:test", "--config", "/config/config.yaml")
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
				"--name", "gt-dolt", "--image", "dolt:test", "--config", "/config/config.yaml")
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
