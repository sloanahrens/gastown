package cmd

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// reworkLedger is n fake landings estimated at points, reworked of which needed
// a second attempt.
func reworkLedger(points, n, reworked int) []land.LandingRecord {
	recs := make([]land.LandingRecord, n)
	for i := range recs {
		attempts := []land.PolecatAttempt{{Seat: "first"}}
		if i < reworked {
			attempts = append(attempts, land.PolecatAttempt{Seat: "second"})
		}
		recs[i] = land.LandingRecord{SPEstimate: &points, PolecatAttempts: attempts}
	}
	return recs
}

// TestSpecLintReworkAdviceLine: a bead at 8 points prints the rework rate of
// the landings at 8 points and no other (the ledger holds ten reworked landings
// at 5 points too, so the share of all landings reads differently), a bead with
// no sp label prints no such line, and the lint exits 0 either way (gt-ntel2p).
func TestSpecLintReworkAdviceLine(t *testing.T) {
	t.Parallel()
	ledger := append(reworkLedger(8, 30, 8), reworkLedger(5, 10, 10)...)
	for _, tc := range []struct {
		name   string
		labels []string
		want   string
	}{
		{"at 8 points", []string{"sp:8", "sp-rung:all", "sp-minutes:8"}, "advice: beads estimated at 8 points needed a second attempt 27% of the time (30 landings, last 14 days)"},
		{"with no sp label", []string{"sp-rung:all", "sp-minutes:8"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := cleanSpec("gt-rework", 2, "2026-01-01")
			spec.Rig, spec.Labels = "gastown", tc.labels
			var out, errOut bytes.Buffer
			var rig string
			opts := specLintOpts{ReworkLandings: func(r string) ([]land.LandingRecord, error) {
				if rig = r; tc.want == "" {
					return nil, errors.New("read for a bead with no sp label")
				}
				return ledger, nil
			}}
			if err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), opts); err != nil {
				t.Fatalf("clean bead returned %v, want exit 0 (err %q)", err, errOut.String())
			}
			if tc.want == "" {
				if strings.Contains(errOut.String(), "rework rate") || strings.Contains(errOut.String(), "second attempt") {
					t.Errorf("a bead with no sp label got the rework advice:\n%s", errOut.String())
				}
				return
			}
			if !strings.Contains(errOut.String(), tc.want) || rig != "gastown" {
				t.Errorf("advice is missing %q (read the rig %q):\n%s", tc.want, rig, errOut.String())
			}
		})
	}
}

// TestSpecLintReworkAdviceUnreadable: a ledger that cannot be read prints the
// advice that says so and leaves the exit status to the verdict.
func TestSpecLintReworkAdviceUnreadable(t *testing.T) {
	t.Parallel()
	spec := cleanSpec("gt-rework", 2, "2026-01-01")
	spec.Labels = []string{"sp:8", "sp-rung:all", "sp-minutes:8"}
	var out, errOut bytes.Buffer
	opts := specLintOpts{ReworkLandings: func(string) ([]land.LandingRecord, error) { return nil, errors.New("ledger gone") }}
	if err := runSpecLintAtWith(&out, &errOut, "", spec.ID, spec, nil, "", nil, false, time.Now(), opts); err != nil {
		t.Fatalf("clean bead returned %v, want exit 0 (err %q)", err, errOut.String())
	}
	if want := "advice: the rework rate could not be read: ledger gone"; !strings.Contains(errOut.String(), want) {
		t.Errorf("advice is missing %q:\n%s", want, errOut.String())
	}
}

// TestSpecLintReworkAdviceIsWiredWithEstimate: the lint reads the landings ledger
// only when --estimate is set, as it runs the estimate only then.
func TestSpecLintReworkAdviceIsWiredWithEstimate(t *testing.T) {
	t.Parallel()
	for _, withEstimate := range []bool{true, false} {
		opts, err := specLintOptsFor(nil, "/town", withEstimate, false, io.Discard)
		if err != nil || (opts.ReworkLandings != nil) != withEstimate {
			t.Errorf("specLintOptsFor(estimate %v) read the ledger = %v, %v; want it wired exactly with the flag", withEstimate, opts.ReworkLandings != nil, err)
		}
	}
}
