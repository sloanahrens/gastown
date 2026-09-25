package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

func TestReportSurvivingWorkExitContract(t *testing.T) {
	prev := survivingWorkForBeadFn
	t.Cleanup(func() { survivingWorkForBeadFn = prev })

	for _, tc := range []struct {
		name     string
		branch   string
		err      error
		wantCode int // 0 = nil error
		wantOut  string
	}{
		{name: "work survives", branch: "polecat/basalt/gt-elvf4+mu5wzd6q", wantOut: "polecat/basalt/gt-elvf4+mu5wzd6q\n"},
		{name: "no surviving work", wantCode: 1},
		{name: "rig has no git repo", err: polecat.ErrNoRigRepo, wantCode: 1},
		{name: "cannot tell", err: errors.New("origin unreachable"), wantCode: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			survivingWorkForBeadFn = func(_, beadID string) (string, error) {
				if beadID != "gt-elvf4" {
					t.Fatalf("asked about %s", beadID)
				}
				return tc.branch, tc.err
			}
			var out, errOut bytes.Buffer
			err := reportSurvivingWork(&out, &errOut, "/town", "gt-elvf4")
			code := 0
			if err != nil {
				var silent *SilentExitError
				if !errors.As(err, &silent) {
					t.Fatalf("want a silent exit, got %v", err)
				}
				code = silent.Code
			}
			if code != tc.wantCode || out.String() != tc.wantOut {
				t.Fatalf("code=%d out=%q, want code=%d out=%q", code, out.String(), tc.wantCode, tc.wantOut)
			}
			if code == 2 && !strings.Contains(errOut.String(), "cannot tell") {
				t.Fatalf("an unknown answer must say so on stderr: %q", errOut.String())
			}
		})
	}
}
