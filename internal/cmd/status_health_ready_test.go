package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/townstatus"
)

// notLooping is a supervisor that is running the daemon, so the health file
// alone decides the view. The test never reads the host's launchctl.
func notLooping(string) (templates.SupervisorState, bool) {
	return templates.SupervisorState{}, false
}

// TestStatusLineReadyExitCodes runs the health line over a report a test wrote
// into a temp town, at a clock the test fixes, and reads the exit code, the
// printed line and the not-ready reason on stderr.
func TestStatusLineReadyExitCodes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	recorded := []townhealth.Field{
		{Name: townhealth.FieldDispatch, Tag: townhealth.Recorded, Verdict: townhealth.Red, Value: "off"},
		{Name: townhealth.FieldBackup, Tag: townhealth.Recorded, Verdict: townhealth.Red, Value: "none"},
	}
	measuredRed := []townhealth.Field{
		{Name: townhealth.FieldDolt, Tag: townhealth.Live, Verdict: townhealth.Red, Value: "p50 900ms"},
		{Name: townhealth.FieldDispatch, Tag: townhealth.Recorded, Verdict: townhealth.Red, Value: "off"},
	}
	// view reads the health file with the supervisor stubbed, so the answer
	// depends only on the report the test wrote.
	view := func(townRoot string, at time.Time) ([]string, townhealth.Verdict, *townhealth.Report) {
		return townstatus.HealthViewWith(townRoot, at, notLooping)
	}
	tests := []struct {
		name       string
		ready      bool
		report     townhealth.Report
		wantCode   int
		wantLine   string
		wantReason string // a substring of stderr; "" means stderr is empty
	}{
		{
			name:     "ready: red with only recorded fields exits 0 and prints its line",
			ready:    true,
			report:   townhealth.Report{At: now, Verdict: townhealth.Red, Fields: recorded},
			wantCode: 0,
			wantLine: "RED tick",
		},
		{
			name:       "ready: a measured red field exits 2 and names it",
			ready:      true,
			report:     townhealth.Report{At: now, Verdict: townhealth.Red, Fields: measuredRed},
			wantCode:   2,
			wantLine:   "RED tick",
			wantReason: "not ready: dolt is LIVE, not RECORDED",
		},
		{
			name:       "ready: a stale report exits 3 and says so",
			ready:      true,
			report:     townhealth.Report{At: now.Add(-time.Hour), Verdict: townhealth.Red, Fields: recorded},
			wantCode:   3,
			wantLine:   "UNKNOWN tick",
			wantReason: "not ready: report is stale",
		},
		{
			name:     "without ready: the same red report exits 2 and prints no reason",
			ready:    false,
			report:   townhealth.Report{At: now, Verdict: townhealth.Red, Fields: recorded},
			wantCode: 2,
			wantLine: "RED tick",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if err := townhealth.Write(town, tt.report); err != nil {
				t.Fatal(err)
			}
			cmd := &cobra.Command{}
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)

			code := silentExitCode(t, statusHealthLineExit(cmd, town, tt.ready, now, view))
			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.wantCode, errOut.String())
			}
			if !strings.HasPrefix(out.String(), tt.wantLine) {
				t.Errorf("line = %q, want it to start with %q", out.String(), tt.wantLine)
			}
			if tt.wantReason == "" {
				if errOut.Len() != 0 {
					t.Errorf("stderr = %q, want none", errOut.String())
				}
			} else if !strings.Contains(errOut.String(), tt.wantReason) {
				t.Errorf("stderr = %q, want it to contain %q", errOut.String(), tt.wantReason)
			}
		})
	}
}

// TestStatusLineReadyNilReport runs the health line over a town with no health
// file, so HealthViewWith reads nothing and returns a nil report. With --ready
// that is not ready and says why; without it the view's exit code stands.
func TestStatusLineReadyNilReport(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	view := func(townRoot string, at time.Time) ([]string, townhealth.Verdict, *townhealth.Report) {
		return townstatus.HealthViewWith(townRoot, at, notLooping)
	}
	tests := []struct {
		name       string
		ready      bool
		wantCode   int
		wantReason string // a substring of stderr; "" means stderr is empty
	}{
		{name: "ready: nothing readable exits non-zero and names the reason", ready: true, wantCode: 3, wantReason: "not ready: no health report"},
		{name: "without ready: the view's own exit code stands", ready: false, wantCode: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)

			code := silentExitCode(t, statusHealthLineExit(cmd, t.TempDir(), tt.ready, now, view))
			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.wantCode, errOut.String())
			}
			if tt.wantReason == "" {
				if errOut.Len() != 0 {
					t.Errorf("stderr = %q, want none", errOut.String())
				}
			} else if !strings.Contains(errOut.String(), tt.wantReason) {
				t.Errorf("stderr = %q, want it to contain %q", errOut.String(), tt.wantReason)
			}
		})
	}
}

// TestStatusFlagUsageIsOneLine holds the --ready flag usage to one short line
// that leaves the [R] explanation to the Long help.
func TestStatusFlagUsageIsOneLine(t *testing.T) {
	t.Parallel()
	usage := statusCmd.Flags().Lookup("ready").Usage
	if strings.Contains(usage, "\n") {
		t.Errorf("usage %q spans more than one line", usage)
	}
	if len(usage) > 80 {
		t.Errorf("usage %q is %d characters, want 80 or fewer", usage, len(usage))
	}
	for _, marker := range []string{"[R]", "RECORDED"} {
		if strings.Contains(usage, marker) {
			t.Errorf("usage %q restates the Long help's %q explanation", usage, marker)
		}
	}
}

// TestStatusLineReadyNeedsLine refuses --ready without --line through the
// command's dispatch, naming both flags, before any town state is read.
func TestStatusLineReadyNeedsLine(t *testing.T) {
	t.Parallel()
	err := runStatusWith(&cobra.Command{}, nil, false, true)
	if err == nil {
		t.Fatal("--ready without --line succeeded, want a refusal")
	}
	for _, flag := range []string{"--ready", "--line"} {
		if !strings.Contains(err.Error(), flag) {
			t.Errorf("refusal %q does not name %s", err, flag)
		}
	}
}

// TestStatusLineReadyFlagsAccepted lists the flag combinations --ready does not
// refuse.
func TestStatusLineReadyFlagsAccepted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		line, ready bool
	}{
		{name: "ready with line", line: true, ready: true},
		{name: "line alone", line: true},
		{name: "neither flag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := validateStatusReady(tt.line, tt.ready); err != nil {
				t.Fatalf("validateStatusReady(%v, %v) = %v, want nil", tt.line, tt.ready, err)
			}
		})
	}
}

// silentExitCode is the process exit code a command returned: 0 for nil, the
// code of a silent exit otherwise.
func silentExitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	code, ok := IsSilentExit(err)
	if !ok {
		t.Fatalf("error %v is not a silent exit", err)
	}
	return code
}
