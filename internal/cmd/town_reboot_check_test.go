package cmd

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townreboot"
)

// rebootFailingWriter is an output that cannot be written.
type rebootFailingWriter struct{}

func (rebootFailingWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

func rebootCheckNames(checks []townreboot.Check) []string {
	var names []string
	for _, c := range checks {
		names = append(names, c.Name)
	}
	return names
}

// fakeRebootEnv is the reboot-check's environment with a town directory, an empty settings
// lookup and a runner that is never called (the checks are replaced).
func fakeRebootEnv(t *testing.T, out *bytes.Buffer, checks []townreboot.Check) townRebootCheckEnv {
	t.Helper()
	town := t.TempDir()
	return townRebootCheckEnv{
		town:   func() (string, error) { return town, nil },
		getenv: func(string) string { return "" },
		checks: func(townreboot.Runner, string) []townreboot.Check { return checks },
		out:    out,
	}
}

// TestTownRebootCheckOK: an all-OK list prints one line per check and the boot-noise note, and
// the command returns nil (gt-9lciim).
func TestTownRebootCheckOK(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	ok := func(name string) townreboot.Check {
		return townreboot.Check{Name: name, Run: func(context.Context) townreboot.Result { return townreboot.Result{OK: true, Detail: "fine"} }}
	}
	env := fakeRebootEnv(t, &out, []townreboot.Check{ok("1 docker"), ok("2 cluster")})

	err := runTownRebootCheck(context.Background(), env)

	if err != nil {
		t.Fatalf("runTownRebootCheck = %v, want nil", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ok   1 docker") || !strings.HasPrefix(lines[1], "ok   2 cluster") || !strings.HasPrefix(lines[2], "note:") {
		t.Errorf("output = %q, want one line per check and the note", out.String())
	}
}

// TestTownRebootCheckFailingRowStillExitsZero: a failing row is printed with its repair and the
// command still returns nil; the non-zero exit is bead gt-nrxgxr's (gt-9lciim).
func TestTownRebootCheckFailingRowStillExitsZero(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	bad := townreboot.Check{Name: "3 vms", Run: func(context.Context) townreboot.Result {
		return townreboot.Result{Detail: "colima is stopped", Fix: "gt town vms start"}
	}}
	env := fakeRebootEnv(t, &out, []townreboot.Check{bad})

	if err := runTownRebootCheck(context.Background(), env); err != nil {
		t.Fatalf("runTownRebootCheck = %v, want nil whatever the rows say", err)
	}
	if !strings.Contains(out.String(), "FAIL 3 vms: colima is stopped (fix: gt town vms start)") {
		t.Errorf("output = %q, want the failing row with its repair", out.String())
	}
}

// TestTownRebootCheckErrors: a town that cannot be found, a settings error and an output that
// cannot be written are each an error naming their cause, and no check runs before the first two
// (gt-9lciim).
func TestTownRebootCheckErrors(t *testing.T) {
	t.Parallel()
	ran := false
	spy := townreboot.Check{Name: "x", Run: func(context.Context) townreboot.Result { ran = true; return townreboot.Result{OK: true} }}
	for _, tc := range []struct {
		name    string
		edit    func(env *townRebootCheckEnv)
		wantErr string
	}{
		{"no town", func(env *townRebootCheckEnv) {
			env.town = func() (string, error) { return "", errors.New("not in a town") }
		}, "not in a town"},
		{"a settings error", func(env *townRebootCheckEnv) {
			env.getenv = func(k string) string {
				if k == "POLECAT_IMAGE_STALE_MINUTES" {
					return "soon"
				}
				return ""
			}
		}, "POLECAT_IMAGE_STALE_MINUTES"},
		{"an output that cannot be written", func(env *townRebootCheckEnv) { env.out = rebootFailingWriter{} }, "pipe closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran = false
			env := fakeRebootEnv(t, &bytes.Buffer{}, []townreboot.Check{spy})
			tc.edit(&env)

			err := runTownRebootCheck(context.Background(), env)

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("runTownRebootCheck = %v, want an error containing %q", err, tc.wantErr)
			}
			if ran && tc.name != "an output that cannot be written" {
				t.Errorf("a check ran before the failure")
			}
		})
	}
}

// TestTownRebootCheckListOrder: the host's list is the doc's four lists in order: the host's VM
// checks, the network's, the cluster's and the image gateway's, built over one runner and the
// registry base (gt-9lciim).
func TestTownRebootCheckListOrder(t *testing.T) {
	t.Parallel()
	var run townreboot.Runner = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	base := config.ImageRegistryBase("")

	got := rebootCheckNames(hostRebootChecks(run, base))

	want := slices.Concat(
		rebootCheckNames(townreboot.HostChecks(run)),
		rebootCheckNames(townreboot.NetworkChecks(run)),
		rebootCheckNames(townreboot.ClusterChecks(run)),
		rebootCheckNames(townreboot.ImageGatewayChecks(run, base)),
	)
	if !slices.Equal(got, want) || len(got) == 0 {
		t.Errorf("checks = %v, want %v", got, want)
	}
}

// TestTownRebootCheckIsRegistered: the command is `gt town reboot-check` and takes no arguments
// (gt-9lciim).
func TestTownRebootCheckIsRegistered(t *testing.T) {
	t.Parallel()
	cmd, _, err := townCmd.Find([]string{"reboot-check"})
	if err != nil || cmd != townRebootCheckCmd {
		t.Fatalf("townCmd.Find(reboot-check) = %v, %v; want the reboot-check command", cmd, err)
	}
	if err := cmd.Args(cmd, []string{"extra"}); err == nil {
		t.Error("reboot-check accepted an argument, want none")
	}
}

// TestTownRebootCheckPassesTheRegistryBase: the registry the image-gateway checks read is the
// town's POLECAT_IMAGE_REGISTRY setting through config.ImageRegistryBase, handed to the check
// lists with the runner (gt-9lciim).
func TestTownRebootCheckPassesTheRegistryBase(t *testing.T) {
	t.Parallel()
	var got string
	var sawRunner bool
	env := fakeRebootEnv(t, &bytes.Buffer{}, nil)
	env.getenv = func(k string) string {
		if k == "POLECAT_IMAGE_REGISTRY" {
			return "registry.example:5000"
		}
		return ""
	}
	env.checks = func(run townreboot.Runner, base string) []townreboot.Check {
		got, sawRunner = base, run != nil
		return nil
	}

	if err := runTownRebootCheck(context.Background(), env); err != nil {
		t.Fatalf("runTownRebootCheck = %v", err)
	}

	if want := config.ImageRegistryBase("registry.example:5000"); got != want || want == "" {
		t.Errorf("registry base = %q, want %q", got, want)
	}
	if !sawRunner {
		t.Error("the check lists were built with no runner")
	}
}
