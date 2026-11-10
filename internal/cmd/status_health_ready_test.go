package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// TestStatusLineReadyExitCodes runs the health line over a report a test wrote
// into a temp town, at a clock the test fixes, and reads the exit code and the
// printed line.
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
	tests := []struct {
		name     string
		ready    bool
		report   townhealth.Report
		wantCode int
		wantLine string
	}{
		{
			name:     "ready: red with only recorded fields exits 0 and prints its line",
			ready:    true,
			report:   townhealth.Report{At: now, Verdict: townhealth.Red, Fields: recorded},
			wantCode: 0,
			wantLine: "RED tick",
		},
		{
			name:     "ready: a measured red field exits 2",
			ready:    true,
			report:   townhealth.Report{At: now, Verdict: townhealth.Red, Fields: measuredRed},
			wantCode: 2,
			wantLine: "RED tick",
		},
		{
			name:     "ready: a stale report exits 3",
			ready:    true,
			report:   townhealth.Report{At: now.Add(-time.Hour), Verdict: townhealth.Red, Fields: recorded},
			wantCode: 3,
			wantLine: "UNKNOWN tick",
		},
		{
			name:     "without ready: the same red report exits 2",
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

			code := silentExitCode(t, statusHealthLineExit(cmd, town, tt.ready, now))
			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", code, tt.wantCode, errOut.String())
			}
			if !strings.HasPrefix(out.String(), tt.wantLine) {
				t.Errorf("line = %q, want it to start with %q", out.String(), tt.wantLine)
			}
		})
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
