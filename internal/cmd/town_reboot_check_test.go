package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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

// TestTownRebootCheckInPod: in a pod the command refuses before it builds the runner, names the
// reason and starts no check (gt-9v9y1s).
func TestTownRebootCheckInPod(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	ran := false
	env := fakeRebootEnv(t, &out, []townreboot.Check{{Name: "x", Run: func(context.Context) townreboot.Result {
		ran = true
		return townreboot.Result{OK: true}
	}}})
	built := false
	env.checks = func(townreboot.Runner, string) []townreboot.Check {
		built = true
		return nil
	}
	env.getenv = func(key string) string {
		if key == podEnv {
			return "10.0.0.1"
		}
		return ""
	}

	err := runTownRebootCheck(context.Background(), env)

	if err == nil || !strings.Contains(err.Error(), podEnv) {
		t.Fatalf("reboot-check in a pod = %v, want a refusal naming %s", err, podEnv)
	}
	if built {
		t.Error("the check lists were built in a pod, want no runner")
	}
	if ran {
		t.Error("a check ran in a pod, want none")
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want none from a run that never started", out.String())
	}
}

// rebootOKCheck is a check whose row is OK.
func rebootOKCheck(name string) townreboot.Check {
	return townreboot.Check{Name: name, Run: func(context.Context) townreboot.Result { return townreboot.Result{OK: true, Detail: "fine"} }}
}

// rebootFailingCheck is a check whose row is not OK, with the repair the row carries (gt-ukmp5e).
func rebootFailingCheck(name string) townreboot.Check {
	return townreboot.Check{Name: name, Run: func(context.Context) townreboot.Result {
		return townreboot.Result{Detail: "colima is stopped", Fix: "gt town vms start"}
	}}
}

// rebootJSONRows decodes the --json output as the array of rows a script reads.
func rebootJSONRows(t *testing.T, out string) []townreboot.Row {
	t.Helper()
	var rows []townreboot.Row
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output %q is not a JSON array of rows: %v", out, err)
	}
	return rows
}

// rebootTextOK reads the status column of the text output: true for "ok  ", false for "FAIL".
func rebootTextOK(t *testing.T, text string) []bool {
	t.Helper()
	var oks []bool
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		switch {
		case strings.HasPrefix(line, "note:"):
		case strings.HasPrefix(line, "ok   "):
			oks = append(oks, true)
		case strings.HasPrefix(line, "FAIL "):
			oks = append(oks, false)
		default:
			t.Fatalf("text line %q has no status column", line)
		}
	}
	return oks
}

// TestTownRebootCheckJSONRows: --json prints one array with a row per check, named as the text
// run names them, and the same ok values as the text run (gt-ukmp5e).
func TestTownRebootCheckJSONRows(t *testing.T) {
	t.Parallel()
	checks := func() []townreboot.Check {
		return []townreboot.Check{rebootOKCheck("1 docker"), rebootFailingCheck("2 cluster")}
	}
	var textOut, jsonOut bytes.Buffer
	if err := runTownRebootCheck(context.Background(), fakeRebootEnv(t, &textOut, checks())); err != nil {
		t.Fatalf("text run = %v, want nil", err)
	}
	jsonEnv := fakeRebootEnv(t, &jsonOut, checks())
	jsonEnv.asJSON = true

	err := runTownRebootCheck(context.Background(), jsonEnv)

	var exit *ExitCodeError
	if !errors.As(err, &exit) {
		t.Fatalf("json run = %v, want the exit-1 error for the failing row", err)
	}
	rows := rebootJSONRows(t, jsonOut.String())
	var gotNames []string
	var gotOK []bool
	for _, r := range rows {
		gotNames = append(gotNames, r.Name)
		gotOK = append(gotOK, r.OK)
	}
	if want := []string{"1 docker", "2 cluster"}; !slices.Equal(gotNames, want) {
		t.Errorf("row names = %v, want %v", gotNames, want)
	}
	if want := rebootTextOK(t, textOut.String()); !slices.Equal(gotOK, want) {
		t.Errorf("row ok values = %v, want the text run's %v", gotOK, want)
	}
}

// TestTownRebootCheckJSONFails: one failing row makes --json exit 1, the array is still printed,
// and the row carries its fix (gt-ukmp5e).
func TestTownRebootCheckJSONFails(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	env := fakeRebootEnv(t, &out, []townreboot.Check{rebootFailingCheck("3 vms")})
	env.asJSON = true

	err := runTownRebootCheck(context.Background(), env)

	var exit *ExitCodeError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("runTownRebootCheck = %v, want an exit-1 error", err)
	}
	rows := rebootJSONRows(t, out.String())
	if len(rows) != 1 || rows[0].OK || rows[0].Fix != "gt town vms start" {
		t.Errorf("rows = %+v, want one not-OK row carrying its fix", rows)
	}
}

// TestTownRebootCheckJSONEmptyIsArray: with no checks --json prints an empty array and exits 0,
// so a reader can range the output without a null check (gt-ukmp5e).
func TestTownRebootCheckJSONEmptyIsArray(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	env := fakeRebootEnv(t, &out, nil)
	env.asJSON = true

	if err := runTownRebootCheck(context.Background(), env); err != nil {
		t.Fatalf("runTownRebootCheck = %v, want nil", err)
	}
	if out.String() != "[]\n" {
		t.Errorf("output = %q, want an empty array", out.String())
	}
}

// TestTownRebootCheckJSONWriteError: a JSON output that cannot be written is an error naming its
// cause (gt-ukmp5e).
func TestTownRebootCheckJSONWriteError(t *testing.T) {
	t.Parallel()
	env := fakeRebootEnv(t, &bytes.Buffer{}, []townreboot.Check{rebootOKCheck("1 docker")})
	env.out = rebootFailingWriter{}
	env.asJSON = true

	err := runTownRebootCheck(context.Background(), env)

	if err == nil || !strings.Contains(err.Error(), "pipe closed") {
		t.Errorf("runTownRebootCheck = %v, want an error containing %q", err, "pipe closed")
	}
}

// TestTownRebootCheckJSONFlagReadsTheFlag: the registered --json flag reads as false by default
// and as true once set (gt-ukmp5e).
func TestTownRebootCheckJSONFlagReadsTheFlag(t *testing.T) {
	t.Parallel()
	set := &cobra.Command{}
	set.Flags().Bool("json", true, "")

	if got, err := townRebootCheckJSONFlag(townRebootCheckCmd); err != nil || got {
		t.Errorf("default --json = %v, %v; want false, nil", got, err)
	}
	if got, err := townRebootCheckJSONFlag(set); err != nil || !got {
		t.Errorf("set --json = %v, %v; want true, nil", got, err)
	}
}

// TestTownRebootCheckJSONFlagMissingIsError: a command without the flag is an error naming the
// flag, not a silent text run (gt-ukmp5e).
func TestTownRebootCheckJSONFlagMissingIsError(t *testing.T) {
	t.Parallel()
	_, err := townRebootCheckJSONFlag(&cobra.Command{})

	if err == nil || !strings.Contains(err.Error(), "--json") {
		t.Errorf("townRebootCheckJSONFlag = %v, want an error naming --json", err)
	}
}

// TestTownRebootCheckCmdNeedsTheFlag: the command run without its --json flag is an error that
// names the flag, before any host check runs (gt-ukmp5e).
func TestTownRebootCheckCmdNeedsTheFlag(t *testing.T) {
	t.Parallel()
	err := runTownRebootCheckCmd(&cobra.Command{})

	if err == nil || !strings.Contains(err.Error(), "--json") {
		t.Errorf("runTownRebootCheckCmd = %v, want an error naming --json", err)
	}
}
