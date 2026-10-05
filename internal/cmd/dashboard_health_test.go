package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// writeHealth hands back a temp town root whose health file is a fresh report,
// so dashboardHealth reads the report a test wrote and not the host's town.
func writeHealth(t *testing.T, verdict townhealth.Verdict, fields ...townhealth.Field) string {
	t.Helper()
	root := t.TempDir()
	if err := townhealth.Write(root, townhealth.Report{At: time.Now(), Verdict: verdict, Fields: fields}); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDashboardHealthCauses(t *testing.T) {
	t.Parallel()

	t.Run("all green yields no causes", func(t *testing.T) {
		t.Parallel()
		h := dashboardHealth(writeHealth(t, townhealth.Green,
			townhealth.Field{Name: townhealth.FieldDolt, Tag: townhealth.Recorded, Verdict: townhealth.Green, Value: "3ms"},
			townhealth.Field{Name: townhealth.FieldLanding, Rig: "mango", Tag: townhealth.Recorded, Verdict: townhealth.Green, Value: "0 pending"},
		))
		if h.Verdict != "green" {
			t.Fatalf("verdict = %q, want green", h.Verdict)
		}
		if len(h.Causes) != 0 {
			t.Fatalf("green report has causes: %+v", h.Causes)
		}
	})

	t.Run("unknown-only report names the unknown fields", func(t *testing.T) {
		t.Parallel()
		h := dashboardHealth(writeHealth(t, townhealth.VerdictUnknown,
			townhealth.Field{Name: townhealth.FieldDolt, Tag: townhealth.Unknown, Verdict: townhealth.VerdictUnknown, Value: "?", Detail: "not wired"},
			townhealth.Field{Name: townhealth.FieldMain, Rig: "mango", Tag: townhealth.Unknown, Verdict: townhealth.VerdictUnknown, Value: "?", Detail: "no post-landing verdict recorded"},
		))
		if h.Verdict != "unknown" {
			t.Fatalf("verdict = %q, want unknown", h.Verdict)
		}
		want := []dashboard.HealthCause{
			{Name: townhealth.FieldDolt, Detail: "not wired"},
			{Name: townhealth.FieldMain, Rig: "mango", Detail: "no post-landing verdict recorded"},
		}
		if !equalCauses(h.Causes, want) {
			t.Fatalf("causes = %+v, want %+v", h.Causes, want)
		}
	})

	t.Run("unknown outranks red which outranks degraded", func(t *testing.T) {
		t.Parallel()
		h := dashboardHealth(writeHealth(t, townhealth.Red,
			townhealth.Field{Name: townhealth.FieldLanding, Rig: "gastown", Tag: townhealth.Recorded, Verdict: townhealth.Degraded, Value: "1 pending", Detail: "waiting 20m"},
			townhealth.Field{Name: townhealth.FieldMain, Rig: "mango", Tag: townhealth.Unknown, Verdict: townhealth.VerdictUnknown, Value: "?", Detail: "no post-landing verdict recorded"},
			townhealth.Field{Name: townhealth.FieldDolt, Tag: townhealth.Live, Verdict: townhealth.Red, Value: "3s", Detail: "p50 over the red limit"},
		))
		if h.Verdict != "red" {
			t.Fatalf("verdict = %q, want red", h.Verdict)
		}
		want := []string{"main/mango", "dolt", "landing/gastown"}
		if got := causeKeys(h.Causes); !equalStrings(got, want) {
			t.Fatalf("cause order = %v, want %v", got, want)
		}
		if h.Causes[0].Detail != "no post-landing verdict recorded" {
			t.Errorf("first cause detail = %q", h.Causes[0].Detail)
		}
	})

	t.Run("causes are capped keeping the worst", func(t *testing.T) {
		t.Parallel()
		h := dashboardHealth(writeHealth(t, townhealth.VerdictUnknown,
			townhealth.Field{Name: "a", Tag: townhealth.Unknown, Verdict: townhealth.VerdictUnknown, Value: "?"},
			townhealth.Field{Name: "b", Tag: townhealth.Live, Verdict: townhealth.Red, Value: "red"},
			townhealth.Field{Name: "c", Tag: townhealth.Live, Verdict: townhealth.Degraded, Value: "slow"},
			townhealth.Field{Name: "d", Tag: townhealth.Live, Verdict: townhealth.Degraded, Value: "slow"},
		))
		if got := causeKeys(h.Causes); !equalStrings(got, []string{"a", "b", "c"}) {
			t.Fatalf("capped causes = %v, want [a b c]", got)
		}
	})
}

// TestDashboardHealthWithoutAReport: a town that never wrote a health file is
// UNKNOWN with nothing to name, not a crash.
func TestDashboardHealthWithoutAReport(t *testing.T) {
	t.Parallel()
	h := dashboardHealth(t.TempDir())
	if h.Verdict != "unknown" {
		t.Fatalf("verdict = %q, want unknown", h.Verdict)
	}
	if len(h.Causes) != 0 {
		t.Fatalf("no report has causes: %+v", h.Causes)
	}
}

func causeKeys(causes []dashboard.HealthCause) []string {
	keys := make([]string, 0, len(causes))
	for _, c := range causes {
		k := c.Name
		if c.Rig != "" {
			k += "/" + c.Rig
		}
		if c.Subject != "" {
			k += ":" + c.Subject
		}
		keys = append(keys, k)
	}
	return keys
}

func equalCauses(got, want []dashboard.HealthCause) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
