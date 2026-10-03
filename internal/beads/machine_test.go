package beads

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const (
	envelopeArray = `{"schema_version":1,"contract_version":1,"data":[{"id":"gt-1"}],"pagination":{"returned":1,"truncated":false},"error":null}`
	envelopeError = `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"not_found","message":"no issue found matching \"gt-x\""}}`
)

func TestLegacyPayload(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		out  string
		want string
	}{
		{"array", []string{"list", "--json"}, envelopeArray, `[{"id":"gt-1"}]`},
		{"object", []string{"config", "get", "k"}, `{"schema_version":1,"contract_version":1,"data":{"key":"k","value":"v"},"error":null}`, `{"key":"k","value":"v"}`},
		{"create --silent is the bare id", []string{"create", "--silent", "--title=t"}, `{"schema_version":1,"contract_version":1,"data":{"id":"gt-9","title":"t"},"error":null}`, "gt-9\n"},
		{"create --json stays the issue", []string{"create", "--json"}, `{"schema_version":1,"contract_version":1,"data":{"id":"gt-9"},"error":null}`, `{"id":"gt-9"}`},
		{"error envelope passes through", []string{"show", "gt-x", "--json"}, envelopeError, envelopeError},
		{"legacy array passes through", []string{"list", "--json"}, `[{"id":"gt-1"}]`, `[{"id":"gt-1"}]`},
		{"legacy object passes through", []string{"show", "gt-1", "--json"}, `{"id":"gt-1","data":"x"}`, `{"id":"gt-1","data":"x"}`},
		{"prose passes through", []string{"close", "gt-1"}, "Closed gt-1\n", "Closed gt-1\n"},
		{"empty passes through", []string{"close", "gt-1"}, "", ""},
		{"null data is null", []string{"close", "gt-1", "--json"}, `{"schema_version":1,"contract_version":1,"data":null,"error":null}`, `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := string(LegacyPayload(tt.args, []byte(tt.out))); got != tt.want {
				t.Errorf("LegacyPayload = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithMachineEnvSetsItExactlyOnce(t *testing.T) {
	t.Parallel()
	got := WithMachineEnv([]string{"A=1", "BD_MACHINE=0", "B=2", "BD_MACHINE=", "BD_MACHINE=1"})
	if want := []string{"A=1", "B=2", "BD_MACHINE=1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("WithMachineEnv = %v, want %v", got, want)
	}
}

// A nil env is the parent's environment, which exec.Cmd reads with PWD moved
// to the working directory: the expanded env keeps that.
func TestWithMachineEnvInExpandsNilEnvWithPWD(t *testing.T) {
	t.Setenv("PWD", "/somewhere/else")
	dir := t.TempDir()
	got := WithMachineEnvIn(dir, nil)
	if v, _ := lastEnvValue(got, "PWD"); v != dir {
		t.Errorf("PWD = %q, want %q", v, dir)
	}
	if v, _ := lastEnvValue(got, "BD_MACHINE"); v != "1" {
		t.Errorf("BD_MACHINE = %q, want 1", v)
	}
	if _, ok := lastEnvValue(WithMachineEnvIn(dir, []string{"A=1"}), "PWD"); ok {
		t.Error("an explicit env must not gain a PWD")
	}
}

// Every policy builder, pinned or routed, read or write, inherits machine mode
// from SuppressBDSideEffects, and a hostile BD_MACHINE in the parent does not
// survive it.
func TestPolicyBuildersSetMachineMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := []string{"PATH=/usr/bin", "BD_MACHINE=0"}
	builders := map[string][]string{
		"pinned":            BuildPinnedBDEnv(base, dir),
		"routing":           BuildRoutingBDEnv(base, dir),
		"read-only pinned":  BuildReadOnlyPinnedBDEnv(base, dir),
		"read-only routing": BuildReadOnlyRoutingBDEnv(base, dir),
		"mutation pinned":   BuildMutationPinnedBDEnv(base, dir),
		"mutation routing":  BuildMutationRoutingBDEnv(base, dir),
		"mutation neutral":  BuildMutationNeutralBDEnv(base),
		"suppress":          SuppressBDSideEffects(base),
	}
	for name, env := range builders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := 0
			for _, kv := range env {
				if strings.HasPrefix(kv, "BD_MACHINE=") {
					n++
				}
			}
			if v, _ := lastEnvValue(env, "BD_MACHINE"); n != 1 || v != "1" {
				t.Errorf("env carries BD_MACHINE %d times, last %q; want once, 1", n, v)
			}
		})
	}
}

func TestMachineExemptIsBdSQLOnly(t *testing.T) {
	t.Parallel()
	for args, want := range map[string]bool{
		"sql SELECT 1":                   true,
		"sql --csv SELECT 1":             true,
		"--allow-stale sql SELECT 1":     true,
		"show gt-1 --json":               false,
		"list --json":                    false,
		"config get sql":                 false,
		"":                               false,
		"--unknown-flag sql SELECT 1":    false,
		"create --title=sql --json":      false,
		"mol wisp list --json":           false,
		"dolt sql":                       false,
		"--allow-stale --json sql pick1": true,
	} {
		if got := machineExempt(strings.Fields(args)); got != want {
			t.Errorf("machineExempt(%q) = %v, want %v", args, got, want)
		}
	}
	env := machineEnvForCall("", []string{"A=1"}, []string{"sql", "SELECT 1"})
	if _, ok := lastEnvValue(env, "BD_MACHINE"); ok {
		t.Errorf("bd sql env %v must not carry BD_MACHINE", env)
	}
	env = machineEnvForCall("", []string{"A=1"}, []string{"show", "gt-1"})
	if v, _ := lastEnvValue(env, "BD_MACHINE"); v != "1" {
		t.Errorf("bd show env %v must carry BD_MACHINE=1", env)
	}
}

func TestParseConfigOutputReadsMachineOutput(t *testing.T) {
	t.Parallel()
	for name, out := range map[string]string{
		"data object":       `{"key":"issue_prefix","value":"gt"}`,
		"envelope":          `{"schema_version":1,"contract_version":1,"data":{"key":"issue_prefix","value":"gt"},"error":null}`,
		"legacy value line": "gt\n",
		"legacy with note":  "Note: routed via shared server\ngt\n",
	} {
		if got := ParseConfigOutput([]byte(out)); got != "gt" {
			t.Errorf("%s: ParseConfigOutput = %q, want gt", name, got)
		}
	}
	for name, out := range map[string]string{
		"unset under machine mode": `{"key":"status.custom","value":""}`,
		"unset legacy":             "status.custom (not set)\n",
		"empty":                    "",
	} {
		if got := ParseConfigOutput([]byte(out)); got != "" {
			t.Errorf("%s: ParseConfigOutput = %q, want empty", name, got)
		}
	}
}

// Beads.run hands its callers the payload bd printed before machine mode, and
// runs every verb but sql in machine mode.
func TestBeadsRunUnwrapsEnvelopeAndSetsMachineMode(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func(args []string) reply {
		if args[0] == "sql" {
			return reply{stdout: "cnt\n3\n"}
		}
		return reply{stdout: envelopeArray}
	})
	b := newRecordedBeads(t.TempDir(), rec)

	out, err := b.run("show", "gt-1", "--json")
	if err != nil || string(out) != `[{"id":"gt-1"}]` {
		t.Fatalf("run show = %q, %v; want the envelope's data", out, err)
	}
	if out, err = b.run("sql", "--csv", "SELECT COUNT(*) as cnt FROM issues"); err != nil || string(out) != "cnt\n3\n" {
		t.Fatalf("run sql = %q, %v; want bd's csv untouched", out, err)
	}

	calls := rec.calls()
	if v, ok := lastEnvValue(calls[0].env, "BD_MACHINE"); !ok || v != "1" {
		t.Errorf("show ran with BD_MACHINE = %q (set %v), want 1", v, ok)
	}
	if _, ok := lastEnvValue(calls[1].env, "BD_MACHINE"); ok {
		t.Errorf("sql ran in machine mode: env %v", calls[1].env)
	}
}

// Failure keeps the envelope where the typed-error readers look for it.
func TestBeadsRunFailureKeepsErrorEnvelope(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply {
		return reply{stdout: envelopeError, stderr: "Issue gt-x not found", err: exitError{bdNotFoundExit}}
	})
	if _, err := newRecordedBeads(t.TempDir(), rec).Show("gt-x"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Show = %v, want a not-found error", err)
	}
}

// fakeBD writes a bd that prints an envelope naming the machine mode and argv
// it was started with, or fails with the given status, and returns its path.
func fakeBD(t *testing.T, failWith int) string {
	t.Helper()
	script := `#!/bin/sh
printf '{"schema_version":1,"contract_version":1,"data":{"machine":"%s","argv":"%s"},"pagination":null,"error":null}\n' "$BD_MACHINE" "$*"
`
	if failWith != 0 {
		script = `#!/bin/sh
echo "Issue gt-x not found" >&2
printf '{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"not_found","message":"no issue found"}}\n'
exit ` + strconv.Itoa(failWith) + "\n"
	}
	path := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCmdHandsBackTheUnwrappedPayload(t *testing.T) {
	t.Parallel()
	const want = `{"machine":"1","argv":"show gt-1 --json"}`
	bin := fakeBD(t, 0)
	env := []string{"PATH=/usr/bin:/bin"}
	newCmd := func() *Cmd { return CommandWithPath(bin, t.TempDir(), env, "show", "gt-1", "--json") }

	t.Run("Output", func(t *testing.T) {
		t.Parallel()
		out, err := newCmd().Output()
		if err != nil || string(out) != want {
			t.Errorf("Output = %q, %v; want %q", out, err, want)
		}
	})
	t.Run("CombinedOutput", func(t *testing.T) {
		t.Parallel()
		out, err := newCmd().CombinedOutput()
		if err != nil || string(out) != want {
			t.Errorf("CombinedOutput = %q, %v; want %q", out, err, want)
		}
	})
	t.Run("Run into a buffer", func(t *testing.T) {
		t.Parallel()
		cmd := newCmd()
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Run(); err != nil || stdout.String() != want {
			t.Errorf("Run stdout = %q, %v; want %q", stdout.String(), err, want)
		}
		if cmd.Stdout != &stdout {
			t.Error("Run left its unwrapper in cmd.Stdout")
		}
	})
	t.Run("Start and Wait into a buffer", func(t *testing.T) {
		t.Parallel()
		cmd := newCmd()
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil || stdout.String() != want {
			t.Errorf("Wait stdout = %q, %v; want %q", stdout.String(), err, want)
		}
	})
	t.Run("the embedded exec.Cmd is raw", func(t *testing.T) {
		t.Parallel()
		cmd := newCmd()
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Cmd.Run(); err != nil || !strings.Contains(stdout.String(), `"contract_version"`) {
			t.Errorf("exec.Cmd.Run stdout = %q, %v; want the raw envelope", stdout.String(), err)
		}
	})
}

// A failed call hands back the envelope untouched: the typed error kind is in
// it, and the prose bd wrote to stderr stays readable.
func TestCmdFailureKeepsTheEnvelope(t *testing.T) {
	t.Parallel()
	bin := fakeBD(t, bdNotFoundExit)
	newCmd := func() *Cmd { return CommandWithPath(bin, t.TempDir(), []string{"PATH=/usr/bin:/bin"}, "show", "gt-x") }

	out, err := newCmd().Output()
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != bdNotFoundExit {
		t.Fatalf("Output err = %v, want exit %d", err, bdNotFoundExit)
	}
	if !strings.Contains(string(out), `"not_found"`) {
		t.Errorf("Output = %q, want the error envelope", out)
	}

	combined, err := newCmd().CombinedOutput()
	if err == nil {
		t.Fatal("CombinedOutput succeeded")
	}
	if !strings.HasPrefix(string(combined), "Issue gt-x not found") || !strings.Contains(string(combined), `"not_found"`) {
		t.Errorf("CombinedOutput = %q, want bd's prose first and the envelope after", combined)
	}

	cmd := newCmd()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err == nil || !strings.Contains(stdout.String(), `"not_found"`) {
		t.Errorf("Run stdout = %q, %v; want the error envelope in the writer", stdout.String(), err)
	}
}

func TestCommandConstructorsPutBdInMachineMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := t.Context()
	cmds := map[string]*Cmd{
		"CommandWithEnv":               CommandWithEnv(dir, []string{"A=1"}, "list"),
		"CommandWithEnv nil env":       CommandWithEnv(dir, nil, "list"),
		"CommandContextWithEnv":        CommandContextWithEnv(ctx, dir, []string{"A=1"}, "list"),
		"CommandWithPath":              CommandWithPath("/opt/bin/bd", dir, []string{"A=1"}, "list"),
		"CommandContextWithPath":       CommandContextWithPath(ctx, "/opt/bin/bd", dir, []string{"A=1"}, "list"),
		"CommandWithEnv, caller's own": CommandWithEnv(dir, []string{"BD_MACHINE=0"}, "list"),
	}
	for name, cmd := range cmds {
		if v, ok := lastEnvValue(cmd.Environ(), "BD_MACHINE"); !ok || v != "1" {
			t.Errorf("%s: BD_MACHINE = %q (set %v), want 1", name, v, ok)
		}
	}
	// bd sql is the exempt verb on every constructor.
	for name, cmd := range map[string]*Cmd{
		"CommandWithEnv":         CommandWithEnv(dir, []string{"A=1"}, "sql", "SELECT 1"),
		"CommandContextWithEnv":  CommandContextWithEnv(ctx, dir, []string{"A=1"}, "sql", "SELECT 1"),
		"CommandWithPath":        CommandWithPath("/opt/bin/bd", dir, nil, "sql", "SELECT 1"),
		"CommandContextWithPath": CommandContextWithPath(ctx, "/opt/bin/bd", dir, nil, "sql", "SELECT 1"),
	} {
		if _, ok := lastEnvValue(cmd.Environ(), "BD_MACHINE"); ok {
			t.Errorf("%s: bd sql must run outside machine mode", name)
		}
	}
}
