package isolationprobe

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// writeRun writes a sample run directory under t.TempDir and returns it.
func writeRun(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// cleanFiles is the page's clean sample: the sandbox was admitted, Ready on
// node-a, and the log holds no failure or fallback line.
func cleanFiles() map[string]string {
	return map[string]string{
		"create.status": "0\n",
		"list.txt":      "NAME              STATUS\nisolation-probe   Ready\n",
		"placement.txt": "isolation-probe   node-a\n",
		"log.txt":       "sandbox admission started\nlandlock: ABI 4\n",
	}
}

// wantReadings asserts the three probes read want, in print order.
func wantReadings(t *testing.T, v Verdict, want ...Result) {
	t.Helper()
	got := make([]Result, 0, len(v.Readings))
	for _, r := range v.Readings {
		got = append(got, r.Result)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("readings = %v, want %v (problems: %v)", got, want, v.Problems)
	}
}

func TestCleanRunIsNoKnownFailure(t *testing.T) {
	t.Parallel()
	v := Evaluate(writeRun(t, cleanFiles()), "node-a")

	wantReadings(t, v, NoKnownFailure, NoKnownFailure, NoKnownFailure)
	if v.Outcome != OutcomeNoKnownFailure {
		t.Fatalf("outcome = %q, want %q (problems: %v)", v.Outcome, OutcomeNoKnownFailure, v.Problems)
	}
	if v.ExitCode() != 0 {
		t.Errorf("exit code = %d, want 0", v.ExitCode())
	}
	if len(v.Problems) != 0 {
		t.Errorf("problems = %v, want none", v.Problems)
	}
	wantLines := []string{
		"probe=landlock result=no-known-failure",
		"probe=seccomp-user-notify result=no-known-failure",
		"probe=task-memory result=no-known-failure",
		"VERDICT NO-KNOWN-FAILURE",
	}
	if got := v.Lines(); !slices.Equal(got, wantLines) {
		t.Errorf("lines = %q, want %q", got, wantLines)
	}
	for _, line := range v.Lines() {
		if strings.Contains(strings.ToUpper(line), "PASS") {
			t.Errorf("line %q calls the result a pass", line)
		}
	}
}

func TestEachFailurePatternFailsItsProbe(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, log, probe string }{
		{"landlock child status", "Landlock probe child exited with status 1", "landlock"},
		{"landlock access rights", "incompatible access-rights for ABI 4", "landlock"},
		{"landlock ABI 2", "landlock: ABI 2", "landlock"},
		{"landlock ABI 0 equals", "landlock ABI=0", "landlock"},
		{"landlock ABI 1 colon", "landlock ABI: 1 available", "landlock"},
		{"seccomp launcher", "notification launcher disappeared", "seccomp-user-notify"},
		{"seccomp control start", "ControlSupervisorStartFailed: start", "seccomp-user-notify"},
		{"seccomp control start mixed case", "CONTROLSUPERVISORSTARTFAILED", "seccomp-user-notify"},
		{"task-memory fails", "task-memory probe reported fail", "task-memory"},
		{"process_vm_readv denied", "process_vm_readv denied by seccomp", "task-memory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := cleanFiles()
			files["log.txt"] += tc.log + "\n"
			v := Evaluate(writeRun(t, files), "node-a")

			for _, r := range v.Readings {
				want := NoKnownFailure
				if r.Probe == tc.probe {
					want = Fail
				}
				if r.Result != want {
					t.Errorf("probe %s = %q, want %q", r.Probe, r.Result, want)
				}
			}
			if v.Outcome != OutcomeFail {
				t.Fatalf("outcome = %q, want %q", v.Outcome, OutcomeFail)
			}
			if got, want := lastLine(v.Lines()), "VERDICT FAIL probe="+tc.probe; got != want {
				t.Errorf("verdict line = %q, want %q", got, want)
			}
			if v.ExitCode() != 1 {
				t.Errorf("exit code = %d, want 1", v.ExitCode())
			}
		})
	}
}

func TestFallbackIsInconclusiveNeverPass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, log, probe string
		want             Result
	}{
		{"seccomp legacy listener", "seccomp: using the legacy listener", "seccomp-user-notify", Inconclusive},
		{"seccomp read-only listener", "seccomp: read-only listener selected", "seccomp-user-notify", Inconclusive},
		{"seccomp listener mixed case", "seccomp: READ-ONLY LISTENER selected", "seccomp-user-notify", Inconclusive},
		{"task-memory proc mem", "task-memory probe fell back to /proc/42/mem", "task-memory", Inconclusive},
		{"task-memory proc mem mixed case", "task-memory probe fell back to /PROC/42/MEM", "task-memory", Inconclusive},
		{"failure outranks seccomp fallback", "seccomp: using the legacy listener\nnotification launcher disappeared", "seccomp-user-notify", Fail},
		{"failure outranks task-memory fallback", "task-memory falling back to /proc/42/mem\nprocess_vm_readv denied", "task-memory", Fail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := cleanFiles()
			files["log.txt"] += tc.log + "\n"
			v := Evaluate(writeRun(t, files), "node-a")

			for _, r := range v.Readings {
				want := NoKnownFailure
				if r.Probe == tc.probe {
					want = tc.want
				}
				if r.Result != want {
					t.Errorf("probe %s = %q, want %q", r.Probe, r.Result, want)
				}
			}
			wantLine := "VERDICT INCONCLUSIVE probe=" + tc.probe
			wantOutcome := OutcomeInconclusive
			if tc.want == Fail {
				wantLine = "VERDICT FAIL probe=" + tc.probe
				wantOutcome = OutcomeFail
			}
			if v.Outcome != wantOutcome {
				t.Fatalf("outcome = %q, want %q", v.Outcome, wantOutcome)
			}
			if got := lastLine(v.Lines()); got != wantLine {
				t.Errorf("verdict line = %q, want %q", got, wantLine)
			}
			if v.ExitCode() != 1 {
				t.Errorf("exit code = %d, want 1", v.ExitCode())
			}
		})
	}
}

func TestMissingOrFullInputsFailClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		edit      func(map[string]string)
		emptyNode bool
		problem   string
	}{
		{"missing create.status", func(f map[string]string) { delete(f, "create.status") }, false, "create.status could not be read"},
		{"empty create.status", func(f map[string]string) { f["create.status"] = "" }, false, "create.status does not read 0"},
		{"create.status is 1", func(f map[string]string) { f["create.status"] = "1\n" }, false, "create.status does not read 0"},
		{"list says Not Ready", func(f map[string]string) {
			f["list.txt"] = "NAME              STATUS\nisolation-probe   Not Ready\n"
		}, false, "list.txt does not show isolation-probe exactly Ready"},
		{"list says NotReady", func(f map[string]string) {
			f["list.txt"] = "NAME              STATUS\nisolation-probe   NotReady\n"
		}, false, "list.txt does not show isolation-probe exactly Ready"},
		{"missing list", func(f map[string]string) { delete(f, "list.txt") }, false, "list.txt could not be read"},
		{"other node's placement", func(f map[string]string) {
			f["placement.txt"] = "isolation-probe   node-b\n"
		}, false, "placement.txt does not name the expected node"},
		{"placement names no node", func(f map[string]string) { f["placement.txt"] = "isolation-probe\n" }, false, "placement.txt does not name the expected node"},
		{"missing placement", func(f map[string]string) { delete(f, "placement.txt") }, false, "placement.txt could not be read"},
		{"empty log", func(f map[string]string) { f["log.txt"] = "" }, false, "log.txt is empty"},
		{"missing log", func(f map[string]string) { delete(f, "log.txt") }, false, "log.txt could not be read"},
		{"5000-line log", func(f map[string]string) {
			f["log.txt"] = strings.Repeat("line\n", 5000)
		}, false, "log.txt fills the 5000-line window"},
		{"5000 lines without a final newline", func(f map[string]string) {
			f["log.txt"] = strings.Repeat("line\n", 4999) + "line"
		}, false, "log.txt fills the 5000-line window"},
		{"no expected node", nil, true, "no expected node was given"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := cleanFiles()
			if tc.edit != nil {
				tc.edit(files)
			}
			node := "node-a"
			if tc.emptyNode {
				node = ""
			}
			v := Evaluate(writeRun(t, files), node)

			wantReadings(t, v, Fail, Fail, Fail)
			if v.Outcome != OutcomeFail {
				t.Fatalf("outcome = %q, want %q", v.Outcome, OutcomeFail)
			}
			if v.ExitCode() == 0 {
				t.Error("exit code = 0, want non-zero")
			}
			if len(v.Problems) == 0 {
				t.Error("no problem recorded for an input the verdict could not use")
			}
			if joined := strings.Join(v.Problems, "\n"); !strings.Contains(joined, tc.problem) {
				t.Errorf("problems = %q, want %q named", joined, tc.problem)
			}
			if got, want := lastLine(v.Lines()), "VERDICT FAIL probe=landlock,seccomp-user-notify,task-memory"; got != want {
				t.Errorf("verdict line = %q, want %q", got, want)
			}
		})
	}
}

func TestLogShorterThanTheWindowIsRead(t *testing.T) {
	t.Parallel()
	files := cleanFiles()
	files["log.txt"] = strings.Repeat("line\n", 4999)
	v := Evaluate(writeRun(t, files), "node-a")

	wantReadings(t, v, NoKnownFailure, NoKnownFailure, NoKnownFailure)
	if v.Outcome != OutcomeNoKnownFailure {
		t.Fatalf("outcome = %q, want %q (problems: %v)", v.Outcome, OutcomeNoKnownFailure, v.Problems)
	}
}

func TestLandlockABIAtOrAboveThreeIsNoFailure(t *testing.T) {
	t.Parallel()
	for _, log := range []string{"landlock: ABI 3", "landlock ABI 12", "landlock ABI: 10 enabled"} {
		t.Run(log, func(t *testing.T) {
			t.Parallel()
			files := cleanFiles()
			files["log.txt"] = log + "\n"
			v := Evaluate(writeRun(t, files), "node-a")

			wantReadings(t, v, NoKnownFailure, NoKnownFailure, NoKnownFailure)
			if v.Outcome != OutcomeNoKnownFailure {
				t.Fatalf("outcome = %q, want %q (problems: %v)", v.Outcome, OutcomeNoKnownFailure, v.Problems)
			}
		})
	}
}

func TestUnsetOutcomeRendersFailClosed(t *testing.T) {
	t.Parallel()
	var v Verdict
	if got, want := lastLine(v.Lines()), "VERDICT FAIL probe="; got != want {
		t.Errorf("verdict line = %q, want %q", got, want)
	}
	if v.ExitCode() == 0 {
		t.Error("exit code = 0, want non-zero")
	}
}

// lastLine returns the verdict line, the last line Lines prints.
func lastLine(lines []string) string {
	return lines[len(lines)-1]
}
