package installer

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// slotLine is the line `gt slot run` prints once it holds the slot.
const slotLine = "Container-gate slot acquired (role=gastown/rebuild-gt, waited 0s, slot 0/2).\n"

// slotArgv is the build a slot role asks gt to run, GT standing for the
// installed binary.
var slotArgv = []string{"GT", "slot", "run", "--role", "gastown/rebuild-gt", "--timeout", "600s", "--", "make", "SKIP_UPDATE_CHECK=1", "build"}

// makeArgv is the build without a slot role.
var makeArgv = []string{"make", "SKIP_UPDATE_CHECK=1", "build"}

// Build runs make in the checkout, through the container-gate slot when a role
// is named. A failed build ends the run failed/build-failed; a slot that was
// never taken is a refusal the caller retries, unless the gt that asked for it
// could not run at all (gt-tpg0p.11).
func TestBuild(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		slotRole   string
		executable bool
		respond    func(dir, name string, args []string) ([]byte, []byte, int)
		ended      bool
		exit       Exit
		cmds       [][]string
		lines      []string
		receipt    string
		// escalation is the fingerprint the run raised, with the message it
		// escalated and its severity.
		escalation string
		message    string
		severity   string
	}{
		{
			name:       "a plain build runs make in the checkout",
			executable: true,
			respond:    func(string, string, []string) ([]byte, []byte, int) { return []byte("built\n"), []byte("linking\n"), 0 },
			cmds:       [][]string{makeArgv},
			lines: []string{
				"[install-gt] Building c2222222 in RIG (source: manual)",
				"built",
				"linking",
			},
		},
		{
			name:       "a build that fails ends the run failed",
			executable: true,
			respond:    func(string, string, []string) ([]byte, []byte, int) { return []byte("build error\n"), nil, 2 },
			ended:      true,
			exit:       ExitFailed,
			cmds:       [][]string{makeArgv},
			lines: []string{
				"[install-gt] Building c2222222 in RIG (source: manual)",
				"build error",
				"[install-gt] FAILED: make build failed for c2222222",
				"install-gt: RESULT failed c2222222 c1111111 build-failed",
			},
			receipt:    "build-failed",
			escalation: "install-gt:build-failed",
			message:    "make build failed for c2222222",
			severity:   "medium",
		},
		{
			name:       "a slot build waits for the line that says the slot is held",
			slotRole:   "gastown/rebuild-gt",
			executable: true,
			respond:    func(string, string, []string) ([]byte, []byte, int) { return []byte(slotLine + "built\n"), nil, 0 },
			cmds:       [][]string{slotArgv},
			lines: []string{
				"[install-gt] Building c2222222 in RIG (source: manual)",
				"Container-gate slot acquired (role=gastown/rebuild-gt, waited 0s, slot 0/2).",
				"built",
			},
		},
		{
			name:       "a slot build that never held the slot is a refusal, exit 3",
			slotRole:   "gastown/rebuild-gt",
			executable: true,
			respond:    func(string, string, []string) ([]byte, []byte, int) { return []byte("waiting for a slot\n"), nil, 0 },
			ended:      true,
			exit:       ExitBusy,
			cmds:       [][]string{slotArgv},
			lines: []string{
				"[install-gt] Building c2222222 in RIG (source: manual)",
				"waiting for a slot",
				"[install-gt] Did not get the container-gate slot within 600s.",
				"install-gt: RESULT refused c2222222 c1111111 slot-busy",
			},
			receipt: "slot-busy",
		},
		{
			// The phrase inside a longer line is something else saying it
			// waited, not the slot being held.
			name:       "the phrase inside another line is not the slot",
			slotRole:   "gastown/rebuild-gt",
			executable: true,
			respond: func(string, string, []string) ([]byte, []byte, int) {
				return []byte("gt: still waiting for " + slotLine), nil, 0
			},
			ended: true,
			exit:  ExitBusy,
			cmds:  [][]string{slotArgv},
			lines: []string{
				"[install-gt] Building c2222222 in RIG (source: manual)",
				"gt: still waiting for Container-gate slot acquired (role=gastown/rebuild-gt, waited 0s, slot 0/2).",
				"[install-gt] Did not get the container-gate slot within 600s.",
				"install-gt: RESULT refused c2222222 c1111111 slot-busy",
			},
			receipt: "slot-busy",
		},
		{
			// The line must end where slot's own line ends: a build that keeps
			// writing on the same line has not handed over a whole one.
			name:       "a line that runs on past the slot line is not the slot",
			slotRole:   "gastown/rebuild-gt",
			executable: true,
			respond: func(string, string, []string) ([]byte, []byte, int) {
				return []byte(strings.TrimSuffix(slotLine, "\n") + " and then compile output\n"), nil, 0
			},
			ended: true,
			exit:  ExitBusy,
			cmds:  [][]string{slotArgv},
			lines: []string{
				"[install-gt] Building c2222222 in RIG (source: manual)",
				"Container-gate slot acquired (role=gastown/rebuild-gt, waited 0s, slot 0/2). and then compile output",
				"[install-gt] Did not get the container-gate slot within 600s.",
				"install-gt: RESULT refused c2222222 c1111111 slot-busy",
			},
			receipt: "slot-busy",
		},
		{
			name:       "a gt that could not run is a failure, not a busy slot",
			slotRole:   "gastown/rebuild-gt",
			executable: true,
			respond:    func(string, string, []string) ([]byte, []byte, int) { return nil, []byte("exec format error\n"), 127 },
			ended:      true,
			exit:       ExitFailed,
			cmds:       [][]string{slotArgv},
			lines: []string{
				"[install-gt] Building c2222222 in RIG (source: manual)",
				"exec format error",
				"[install-gt] FAILED: cannot run GT slot run (exit 127); the container-gate slot was never requested",
				"install-gt: RESULT failed c2222222 c1111111 slot-unavailable",
			},
			receipt:    "slot-unavailable",
			escalation: "install-gt:slot-unavailable",
			message:    "cannot run GT slot run (exit 127); the container-gate slot was never requested",
			severity:   "medium",
		},
		{
			name:       "a gt that is not executable is a failure before any command runs",
			slotRole:   "gastown/rebuild-gt",
			executable: false,
			ended:      true,
			exit:       ExitFailed,
			lines: []string{
				"[install-gt] Building c2222222 in RIG (source: manual)",
				"[install-gt] FAILED: GT is missing or not executable; cannot take the container-gate slot",
				"install-gt: RESULT failed c2222222 c1111111 slot-unavailable",
			},
			receipt:    "slot-unavailable",
			escalation: "install-gt:slot-unavailable",
			message:    "GT is missing or not executable; cannot take the container-gate slot",
			severity:   "medium",
		},
		{
			name:       "the acquired line does not hide a build that failed",
			slotRole:   "gastown/rebuild-gt",
			executable: true,
			respond: func(string, string, []string) ([]byte, []byte, int) {
				return []byte(slotLine), []byte("compile error\n"), 1
			},
			ended: true,
			exit:  ExitFailed,
			cmds:  [][]string{slotArgv},
			lines: []string{
				"[install-gt] Building c2222222 in RIG (source: manual)",
				"Container-gate slot acquired (role=gastown/rebuild-gt, waited 0s, slot 0/2).",
				"compile error",
				"[install-gt] FAILED: make build failed for c2222222",
				"install-gt: RESULT failed c2222222 c1111111 build-failed",
			},
			receipt:    "build-failed",
			escalation: "install-gt:build-failed",
			message:    "make build failed for c2222222",
			severity:   "medium",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.resolved("c1111111")
			tr.C.Receipts = filepath.Join(t.TempDir(), "install-receipts.jsonl")
			tr.Cfg.SlotRole = tc.slotRole
			tr.Files.IsExecutable = func(string) bool { return tc.executable }
			tr.respond = tc.respond

			exit, ended := tr.Build()

			if exit != tc.exit || ended != tc.ended {
				t.Errorf("Build() = (%d, %v), want (%d, %v)", exit, ended, tc.exit, tc.ended)
			}
			if got, want := substituteAll(tc.cmds, tr), tr.cmds; !reflect.DeepEqual(want, got) {
				t.Errorf("commands = %q, want %q", want, got)
			}
			wantLines := substitute(tc.lines, tr)
			if got := tr.lines(); !reflect.DeepEqual(got, wantLines) {
				t.Errorf("stdout = %q, want %q", got, wantLines)
			}
			if tc.receipt != "" {
				if got := read(t, tr.C.Receipts); !strings.Contains(got, `"reason": "`+tc.receipt+`"`) {
					t.Errorf("receipts = %q, want reason %q", got, tc.receipt)
				}
			}
			if tc.escalation == "" {
				if got := tr.escalation(); got != nil {
					t.Errorf("escalated with %q, want nothing", got)
				}
				return
			}
			want := []string{tr.C.GT, "escalate", "install-gt: " + substitute([]string{tc.message}, tr)[0],
				"-s", tc.severity, "--source", "script:install-gt", "--fingerprint", tc.escalation}
			if got := tr.escalation(); !reflect.DeepEqual(got, want) {
				t.Errorf("escalation = %q, want %q", got, want)
			}
		})
	}
}

// Install is the swap: make install-local runs in the checkout with the install
// directory the Makefile takes, and the output is echoed as the script's
// command substitution leaves it.
func TestInstall(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		stdout string
		stderr string
		status int
		ended  bool
		exit   Exit
		// want is stdout exactly: the echoed command output, then the run's own
		// lines. The command substitution drops every trailing newline and echo
		// puts one back, so "a\nb\n\n" comes out as "a\nb\n".
		want    string
		receipt string
	}{
		{
			name:   "a good install echoes what it said",
			stdout: "installed gt\n",
			want:   "installed gt\n",
		},
		{
			name:   "the output is echoed with one trailing newline",
			stdout: "a\nb\n\n",
			want:   "a\nb\n",
		},
		{
			name:    "a failed install ends the run failed",
			stdout:  "check-forward-only: not forward\n",
			status:  1,
			ended:   true,
			exit:    ExitFailed,
			receipt: "build-failed",
			want: "check-forward-only: not forward\n" +
				"[install-gt] FAILED: make install-local failed for c2222222\n" +
				"install-gt: RESULT failed c2222222 c1111111 build-failed\n",
		},
		{
			name:    "a binary already at HEAD is a no-op, not a failure",
			stdout:  "Binary is already at HEAD, nothing to do\n",
			status:  1,
			ended:   true,
			exit:    ExitOK,
			receipt: "already-installed",
			want: "Binary is already at HEAD, nothing to do\n" +
				"install-gt: RESULT noop c2222222 c1111111 already-installed\n",
		},
		{
			// Only check-forward-only's own sentence is a no-op: a failure that
			// mentions the phrase in passing still ends the run failed.
			name:    "a failure that mentions HEAD is not a no-op",
			stdout:  "check-forward-only: the binary is not already at HEAD\n",
			status:  1,
			ended:   true,
			exit:    ExitFailed,
			receipt: "build-failed",
			want: "check-forward-only: the binary is not already at HEAD\n" +
				"[install-gt] FAILED: make install-local failed for c2222222\n" +
				"install-gt: RESULT failed c2222222 c1111111 build-failed\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.resolved("c1111111")
			tr.C.Receipts = filepath.Join(t.TempDir(), "install-receipts.jsonl")
			tr.respond = func(string, string, []string) ([]byte, []byte, int) {
				return []byte(tc.stdout), []byte(tc.stderr), tc.status
			}

			exit, ended := tr.Install()

			if exit != tc.exit || ended != tc.ended {
				t.Errorf("Install() = (%d, %v), want (%d, %v)", exit, ended, tc.exit, tc.ended)
			}
			want := []string{"make", "SKIP_UPDATE_CHECK=1", "INSTALL_DIR=" + tr.Paths.BinDir, "install-local"}
			if got := tr.command(0); !reflect.DeepEqual(got, want) {
				t.Errorf("command = %q, want %q", got, want)
			}
			if got := tr.out.String(); got != tc.want {
				t.Errorf("stdout = %q, want %q", got, tc.want)
			}
			if tc.receipt != "" {
				if got := read(t, tr.C.Receipts); !strings.Contains(got, `"reason": "`+tc.receipt+`"`) {
					t.Errorf("receipts = %q, want reason %q", got, tc.receipt)
				}
			}
		})
	}
}

// substitute replaces the GT and RIG stand-ins in argv or lines with the run's
// own paths, so an expectation stays readable.
func substitute(values []string, tr *testRun) []string {
	out := make([]string, len(values))
	for i, value := range values {
		value = strings.ReplaceAll(value, "GT", tr.Paths.GT())
		out[i] = strings.ReplaceAll(value, "RIG", tr.Paths.RigDir)
	}
	return out
}

// substituteAll applies substitute to every argv of a list.
func substituteAll(cmds [][]string, tr *testRun) [][]string {
	if cmds == nil {
		return nil
	}
	out := make([][]string, len(cmds))
	for i, cmd := range cmds {
		out[i] = substitute(cmd, tr)
	}
	return out
}
