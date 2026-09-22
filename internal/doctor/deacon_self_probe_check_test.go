package doctor

import (
	"testing"

	"github.com/steveyegge/gastown/internal/daemon"
)

// TestDeaconSelfProbeCheck_MapsVerdictToStatus covers the three doctor
// statuses the deacon-self-probe check must produce: the actual
// send/evaluate logic (sent+acked OK, sent+unacked Error, send fails
// Skipped) is integration-tested against fakes in
// internal/daemon/deacon_self_probe_test.go, since that's where the real
// mail send/read happens. This test only checks the thin translation from
// daemon.DeaconSelfProbeVerdict to doctor.CheckStatus.
func TestDeaconSelfProbeCheck_MapsVerdictToStatus(t *testing.T) {
	cases := []struct {
		verdict string
		want    CheckStatus
	}{
		{"ok", StatusOK},
		{"error", StatusError},
		{"skipped", StatusSkipped},
		{"", StatusSkipped}, // never OK/Error on an unrecognized verdict
	}

	for _, tc := range cases {
		check := &DeaconSelfProbeCheck{
			BaseCheck: NewDeaconSelfProbeCheck().BaseCheck,
			evaluate: func(string) daemon.DeaconSelfProbeVerdict {
				return daemon.DeaconSelfProbeVerdict{Verdict: tc.verdict, Message: "test message"}
			},
		}

		result := check.Run(&CheckContext{TownRoot: t.TempDir()})
		if result.Status != tc.want {
			t.Errorf("verdict %q: Status = %v, want %v", tc.verdict, result.Status, tc.want)
		}
		if result.Message != "test message" {
			t.Errorf("verdict %q: Message = %q, want %q", tc.verdict, result.Message, "test message")
		}
	}
}
