package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/townstatus"
)

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
	// needsOverseer is the incident the --ready flag is for (gt-pittz6): every
	// serving row is healthy, and a bead waiting for the overseer is the only
	// non-green row.
	needsOverseer := []townhealth.Field{
		{Name: townhealth.FieldDolt, Tag: townhealth.Live, Verdict: townhealth.Green, Value: "3ms"},
		{Name: townhealth.FieldDaemon, Tag: townhealth.Live, Verdict: townhealth.Green, Value: "heartbeat 1s ago"},
		{Name: townhealth.FieldConfig, Tag: townhealth.Live, Verdict: townhealth.Green, Value: "valid"},
		{Name: townhealth.FieldNeedsOverseer, Tag: townhealth.Live, Verdict: townhealth.Degraded, Value: "1", Detail: "1 waiting for overseer"},
	}
	// view reads the health file, so the answer depends only on the report the
	// test wrote.
	view := func(townRoot string, at time.Time) ([]string, townhealth.Verdict, *townhealth.Report) {
		return townstatus.HealthView(townRoot, at)
	}
	tests := []struct {
		name         string
		ready        bool
		report       townhealth.Report
		wantCode     int
		wantLine     string
		wantLinePart string // a substring of the printed line; "" means unchecked
		wantReason   string // a substring of stderr; "" means stderr is empty
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
			name:         "ready: a live needs-overseer row exits 0 and still shows in the line",
			ready:        true,
			report:       townhealth.Report{At: now, Verdict: townhealth.Degraded, Fields: needsOverseer},
			wantCode:     0,
			wantLine:     "DEGRADED tick",
			wantLinePart: "needs-overseer=1",
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
			if tt.wantLinePart != "" && !strings.Contains(out.String(), tt.wantLinePart) {
				t.Errorf("line = %q, want it to contain %q", out.String(), tt.wantLinePart)
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
// file, so HealthView reads nothing and returns a nil report. With --ready
// that is not ready and says why; without it the view's exit code stands.
func TestStatusLineReadyNilReport(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	view := func(townRoot string, at time.Time) ([]string, townhealth.Verdict, *townhealth.Report) {
		return townstatus.HealthView(townRoot, at)
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

// TestStatusHelpNamesNewRig holds the Long help to the NEW tag: a rig with no
// landing record is green and reads NEW in gt status, one that landed with no
// verdict stays UNKNOWN, and the help must say both rather than leave the tag
// unexplained (gt-30ehe8). The help is compared as one sentence, so rewrapping
// the prose does not fail the test.
func TestStatusHelpNamesNewRig(t *testing.T) {
	t.Parallel()
	want := "A rig with no landing record yet is green and reads NEW in gt status, not UNKNOWN, so a rig just added does not hold the town back; a rig that has landed with no post-land verdict reads UNKNOWN, since no run of its check recorded one."
	if got := strings.Join(strings.Fields(statusCmd.Long), " "); !strings.Contains(got, want) {
		t.Errorf("status help does not explain the NEW tag; want %q in Long, got:\n%s", want, statusCmd.Long)
	}
}

// TestStatusHelpNamesTheReadyRule holds the Long help to the readiness rule
// gt-pittz6 set: a non-green attention row is ready too, because it asks a
// person to look rather than reporting that the daemon cannot serve
// (gt-oxicgh). The help is compared as one sentence, so rewrapping the prose
// does not fail the test.
func TestStatusHelpNamesTheReadyRule(t *testing.T) {
	t.Parallel()
	want := "exit 0 when the report is fresh and every non-green field is [R] (RECORDED, a state the town accepts, such as dispatch off or a backup not yet taken) or an attention row (needs-overseer, needs-operator, needs-human, escalation), which says a person should look, not that the daemon cannot serve."
	if got := strings.Join(strings.Fields(statusCmd.Long), " "); !strings.Contains(got, want) {
		t.Errorf("status help does not state the current --ready rule; want %q in Long, got:\n%s", want, statusCmd.Long)
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
