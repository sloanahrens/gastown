package townhealth

import (
	"context"
	"strings"
	"testing"
	"time"
)

// infraReport is a fake town with one rig landing through Forgejo and one
// probe's outcome set, so each rule is read on its own.
func infraReport(rig string, fj, rn InfraProbe) LandingInfraReport {
	return LandingInfraReport{Configured: true, Rigs: []RigInfra{{Rig: rig, Forgejo: fj, Runner: rn}}}
}

// healthyInfra is one probe that answered healthy.
func healthyInfra() InfraProbe { return InfraProbe{OK: true} }

// failingInfra is one probe that has been failing for age.
func failingInfra(age time.Duration, detail string) InfraProbe {
	return InfraProbe{Since: ago(age), Detail: detail}
}

// infraCompute computes a healthy town with the given landing-infrastructure
// observation.
func infraCompute(t *testing.T, rep LandingInfraReport) Report {
	t.Helper()
	f := healthy()
	f.infra = rep
	return Compute(context.Background(), inputs(f))
}

// infraFieldOf returns the report's forgejo or runner field for rig.
func infraFieldOf(t *testing.T, r Report, name, rig string) Field {
	t.Helper()
	return field(t, r, name+"/"+rig)
}

// TestInfraFieldsAreAbsentWithoutAForgejoRig: a town where nothing lands
// through Forgejo has no forgejo and no runner field — not an unknown one.
// The question the probes answer is not asked there, and a field nobody can
// answer would hold the line's verdict down forever (gt-fn9e6.11).
func TestInfraFieldsAreAbsentWithoutAForgejoRig(t *testing.T) {
	t.Parallel()
	// A town whose health computation is not wired to a probe at all.
	unwired := Compute(context.Background(), inputs(healthy()))
	if k := infraFieldKey(unwired); k != "" {
		t.Errorf("unwired town grew field %s", k)
	}
	// A probe that reports no rig landing through Forgejo.
	empty := infraCompute(t, LandingInfraReport{})
	if k := infraFieldKey(empty); k != "" {
		t.Errorf("town with no forgejo rig grew field %s", k)
	}
	if empty.Verdict != Green {
		t.Errorf("verdict = %s, want green: no field is not a bad field", empty.Verdict)
	}
}

// infraFieldKey returns the key of the first forgejo or runner field, or "".
func infraFieldKey(r Report) string {
	for _, f := range r.Fields {
		if f.Name == FieldForgejo || f.Name == FieldRunner {
			return f.Key()
		}
	}
	return ""
}

// TestForgejoFieldFollowsTheVersionProbe: a version endpoint that answers
// reads green; one that does not is degraded while the failure is younger
// than InfraRedAfter and red once it is older.
func TestForgejoFieldFollowsTheVersionProbe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fj   InfraProbe
		want Verdict
	}{
		{"answered", healthyInfra(), Green},
		{"unreachable just now", failingInfra(30*time.Second, "dial tcp: connection refused"), Degraded},
		{"unreachable four minutes", failingInfra(4*time.Minute, "dial tcp: connection refused"), Degraded},
		{"unreachable five minutes", failingInfra(InfraRedAfter, "dial tcp: connection refused"), Red},
		{"unreachable an hour", failingInfra(time.Hour, "dial tcp: connection refused"), Red},
		{"non-200", failingInfra(time.Hour, "forgejo: GET /version returned 503"), Red},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := infraFieldOf(t, infraCompute(t, infraReport("gastown", tc.fj, healthyInfra())), FieldForgejo, "gastown")
			if f.Verdict != tc.want {
				t.Errorf("forgejo verdict = %s, want %s", f.Verdict, tc.want)
			}
			if tc.want == Green && f.Tag != Recorded {
				t.Errorf("forgejo tag = %s, want %s: the field reads the patrol's record", f.Tag, Recorded)
			}
			if tc.want != Green && f.Detail == "" {
				t.Errorf("forgejo verdict %s carries no reason", tc.want)
			}
		})
	}
}

// TestRunnerFieldFollowsTheOnlineRunner: at least one online runner is green,
// none online is degraded and then red on the same thresholds, and an
// unanswerable probe is never red.
func TestRunnerFieldFollowsTheOnlineRunner(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rn   InfraProbe
		want Verdict
	}{
		{"one online", InfraProbe{OK: true, Online: 1}, Green},
		{"none online just now", failingInfra(0, "0 of 1 runners online"), Degraded},
		{"none online two minutes", failingInfra(2*time.Minute, "0 of 1 runners online"), Degraded},
		{"none online five minutes", failingInfra(InfraRedAfter, "0 of 1 runners online"), Red},
		{"no token file", InfraProbe{Unavailable: "the landing bot's token is unusable: no such file"}, VerdictUnknown},
		{"no base URL", InfraProbe{Unavailable: "the rig's Forgejo settings name no usable remote URL"}, VerdictUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := infraFieldOf(t, infraCompute(t, infraReport("gastown", healthyInfra(), tc.rn)), FieldRunner, "gastown")
			if f.Verdict != tc.want {
				t.Fatalf("runner verdict = %s, want %s", f.Verdict, tc.want)
			}
			if tc.want == VerdictUnknown {
				if f.Value != "?" {
					t.Errorf("unknown runner value = %q, want %q", f.Value, "?")
				}
				if !strings.Contains(f.Detail, tc.rn.Unavailable) {
					t.Errorf("unknown runner detail = %q, want it to carry %q", f.Detail, tc.rn.Unavailable)
				}
			}
		})
	}
}

// TestInfraProbeThatCannotBeMadeIsNeverRed: an unanswerable probe stays
// unknown however long it has been unanswerable. A missing token or a rig
// with no base URL is a configuration question, and paging red on it would
// report an outage nobody observed (gt-fn9e6.11).
func TestInfraProbeThatCannotBeMadeIsNeverRed(t *testing.T) {
	t.Parallel()
	p := InfraProbe{Unavailable: "the landing bot's token is unusable", Since: ago(48 * time.Hour)}
	if got := InfraVerdict(p, now); got != VerdictUnknown {
		t.Errorf("InfraVerdict = %s, want %s", got, VerdictUnknown)
	}
	r := infraCompute(t, infraReport("gastown", p, p))
	if r.Verdict != VerdictUnknown {
		t.Errorf("town verdict = %s, want %s: an unanswerable probe is not red", r.Verdict, VerdictUnknown)
	}
}

// TestInfraVerdictIsTheRuleTheLineShows pins the exported rule the daemon's
// patrol escalates on: it must agree with the fields, or an alert would name
// a condition the line does not show.
func TestInfraVerdictIsTheRuleTheLineShows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    InfraProbe
		want Verdict
	}{
		{"healthy", InfraProbe{OK: true, Online: 2}, Green},
		{"failing", failingInfra(time.Minute, "boom"), Degraded},
		{"failing past the limit", failingInfra(InfraRedAfter+time.Second, "boom"), Red},
		{"not run yet", InfraProbe{}, Degraded},
		{"unavailable", InfraProbe{Unavailable: "no token"}, VerdictUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := InfraVerdict(tc.p, now); got != tc.want {
				t.Errorf("InfraVerdict(%+v) = %s, want %s", tc.p, got, tc.want)
			}
		})
	}
}
