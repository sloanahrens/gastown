package slot

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// dockerCreatedAtLayout is the layout `docker ps --format {{json .}}` renders
// on this host; dockerPSLine writes it so fixtures go through the same parser
// production does.
const dockerCreatedAtLayout = "2006-01-02 15:04:05 -0700 MST"

// dockerPSLine renders one container the way docker's JSON listing does.
func dockerPSLine(id, image, name string, created time.Time, labels map[string]string) string {
	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return fmt.Sprintf(`{"ID":%q,"Image":%q,"Names":%q,"CreatedAt":%q,"Labels":%q}`,
		id, image, name, created.Format(dockerCreatedAtLayout), strings.Join(pairs, ","))
}

// sessionLabels is the label set a testcontainers suite puts on the containers
// of one session.
func sessionLabels(sessionID string) map[string]string {
	return map[string]string{
		"org.testcontainers.sessionId": sessionID,
		"org.testcontainers":           "true",
	}
}

func TestParseGateContainer(t *testing.T) {
	created := time.Date(2026, 9, 21, 10, 41, 0, 0, time.UTC)

	t.Run("docker json", func(t *testing.T) {
		line := dockerPSLine("abc123", "dolthub/dolt-sql-server:2.2.0", "wizardly_goldberg", created, sessionLabels("sess-1"))
		got := parseGateContainer(line)
		if got.ID != "abc123" || got.Image != "dolthub/dolt-sql-server:2.2.0" || got.Name != "wizardly_goldberg" {
			t.Fatalf("parsed identity = %+v, want the fields the JSON carried", got)
		}
		if !got.Created.Equal(created) {
			t.Fatalf("Created = %v, want %v", got.Created, created)
		}
		if got.SessionID() != "sess-1" {
			t.Fatalf("SessionID() = %q, want %q", got.SessionID(), "sess-1")
		}
	})

	t.Run("legacy image-and-name line", func(t *testing.T) {
		got := parseGateContainer("dolt/dolt-sql-server:2.2.0 some-suite")
		if got.Image != "dolt/dolt-sql-server:2.2.0" || got.Name != "some-suite" {
			t.Fatalf("parsed = %+v, want image and name split on the first space", got)
		}
		if !got.Created.IsZero() {
			t.Fatalf("Created = %v, want the zero time — a line with no start time has an unknown age", got.Created)
		}
	})

	t.Run("json without an image is not a container", func(t *testing.T) {
		got := parseGateContainer(`{"ID":"abc123","Image":"","Names":"x"}`)
		if got.Image != `{"ID":"abc123","Image":"","Names":"x"}` {
			t.Fatalf("parsed = %+v, want the fallback reading of a line docker did not produce", got)
		}
	})
}

func TestParseDockerCreatedAt(t *testing.T) {
	want := time.Date(2026, 9, 21, 20, 42, 23, 0, time.FixedZone("CDT", -5*3600))

	got := parseDockerCreatedAt("2026-09-21 20:42:23 -0500 CDT")
	if !got.Equal(want) {
		t.Fatalf("parseDockerCreatedAt = %v, want %v (docker's own layout)", got, want)
	}
	if got := parseDockerCreatedAt("2026-09-21T20:42:23Z"); got.IsZero() {
		t.Fatal("parseDockerCreatedAt rejected RFC 3339")
	}
	if got := parseDockerCreatedAt("last tuesday"); !got.IsZero() {
		t.Fatalf("parseDockerCreatedAt = %v, want the zero time for an unparseable string", got)
	}
}

func TestParseDockerLabels(t *testing.T) {
	got := parseDockerLabels("org.testcontainers.sessionId=sess-1,org.testcontainers=true")
	if got["org.testcontainers.sessionId"] != "sess-1" || got["org.testcontainers"] != "true" {
		t.Fatalf("parseDockerLabels = %v, want both pairs", got)
	}
	if got := parseDockerLabels(""); got != nil {
		t.Fatalf("parseDockerLabels(\"\") = %v, want nil", got)
	}
	if got := parseDockerLabels("no-equals-sign"); len(got) != 0 {
		t.Fatalf("parseDockerLabels = %v, want nothing for a pair with no \"=\"", got)
	}
}

// TestClassify is the rule the whole staleness verdict turns on (gt-ul1k):
// only a container that could still belong to a running suite blocks the gate.
func TestClassify(t *testing.T) {
	now := time.Date(2026, 9, 21, 17, 0, 0, 0, time.UTC)
	window := 30 * time.Minute

	young := now.Add(-2 * time.Minute)
	hoursOld := now.Add(-5 * time.Hour)

	tests := []struct {
		name     string
		holder   GateContainer
		others   []GateContainer
		want     Verdict
		reasonIn string
	}{
		{
			name:     "young container is a live suite",
			holder:   GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.2.0", Name: "suite", Created: young},
			want:     VerdictLive,
			reasonIn: "under the",
		},
		{
			name:     "old container with no reaper is debris",
			holder:   GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.2.0", Name: "wizardly_goldberg", Created: hoursOld},
			want:     VerdictDebris,
			reasonIn: "no reaper running",
		},
		{
			name:   "old container whose session's reaper is still alive is a live suite",
			holder: GateContainer{ID: "a", Image: "dolt/dolt-sql-server:2.2.0", Name: "suite", Created: hoursOld, Labels: sessionLabels("sess-1")},
			others: []GateContainer{
				{ID: "r", Image: "testcontainers/ryuk:0.14.0", Name: "reaper", Created: young, Labels: sessionLabels("sess-1")},
			},
			want:     VerdictLive,
			reasonIn: "reaper",
		},
		{
			name:   "old container whose reaper reaps a different session is debris",
			holder: GateContainer{ID: "a", Image: "dolt/dolt-sql-server:2.2.0", Name: "suite", Created: hoursOld, Labels: sessionLabels("sess-dead")},
			others: []GateContainer{
				{ID: "r", Image: "testcontainers/ryuk:0.14.0", Name: "reaper", Created: young, Labels: sessionLabels("sess-live")},
			},
			want:     VerdictDebris,
			reasonIn: "no reaper running",
		},
		{
			name:   "old reaper is debris — it is what exits last, so it leaked",
			holder: GateContainer{ID: "r", Image: "testcontainers/ryuk:0.14.0", Name: "reaper", Created: hoursOld, Labels: sessionLabels("sess-1")},
			want:     VerdictDebris,
			reasonIn: "session it reaped for is over",
		},
		{
			name:     "unknown age is not debris",
			holder:   GateContainer{ID: "a", Image: "dolt/dolt-sql-server:2.2.0", Name: "suite"},
			want:     VerdictUnknown,
			reasonIn: "no start time",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(append([]GateContainer{tt.holder}, tt.others...), now, window)
			if len(got) == 0 {
				t.Fatal("Classify returned no verdicts")
			}
			if got[0].Verdict != tt.want {
				t.Fatalf("Verdict = %q (%s), want %q", got[0].Verdict, got[0].Reason, tt.want)
			}
			if !strings.Contains(got[0].Reason, tt.reasonIn) {
				t.Fatalf("Reason = %q, want it to mention %q", got[0].Reason, tt.reasonIn)
			}
			if blocks := got[0].Blocks(); blocks != (tt.want != VerdictDebris) {
				t.Fatalf("Blocks() = %v for verdict %q", blocks, tt.want)
			}
		})
	}
}

// TestLogDebris pins the requirement that a grant made on debris leaves the
// evidence behind: the container, the age verdict, and the labels (gt-ul1k).
func TestLogDebris(t *testing.T) {
	orig := debrisWriter
	buf := &strings.Builder{}
	debrisWriter = buf
	t.Cleanup(func() { debrisWriter = orig })

	logDebris(ContainerVerdict{
		Container: GateContainer{ID: "orphan-id", Image: "dolthub/dolt-sql-server:2.2.0", Name: "wizardly_goldberg",
			Labels: map[string]string{"org.testcontainers.reuse.enable": ""}},
		Verdict: VerdictDebris,
		Reason:  "age 5h0m0s exceeds the 30m0s staleness window with no reaper running for its session",
	})

	got := buf.String()
	for _, want := range []string{"wizardly_goldberg", "age 5h0m0s", "org.testcontainers.reuse.enable=", "gt slot reap"} {
		if !strings.Contains(got, want) {
			t.Errorf("log line %q, want it to carry %q", got, want)
		}
	}
}

// TestDebrisLoggerAnnouncesEachContainerOnce keeps the wait quiet: Acquire
// polls while a live suite runs, so the same orphan must not be re-announced
// every DefaultPollInterval.
func TestDebrisLoggerAnnouncesEachContainerOnce(t *testing.T) {
	orig := debrisWriter
	buf := &strings.Builder{}
	debrisWriter = buf
	t.Cleanup(func() { debrisWriter = orig })

	logOnce := debrisLogger()
	orphan := ContainerVerdict{Container: GateContainer{ID: "orphan-id", Image: "dolt/dolt-sql-server:2.2.0", Name: "orphan"}}
	other := ContainerVerdict{Container: GateContainer{ID: "second-id", Image: "dolt/dolt-sql-server:2.2.0", Name: "second"}}

	logOnce(orphan)
	logOnce(orphan)
	logOnce(other)

	if got := strings.Count(buf.String(), "\n"); got != 2 {
		t.Fatalf("logged %d line(s), want one per distinct container:\n%s", got, buf.String())
	}
}

func TestGateContainerHelpers(t *testing.T) {
	reaper := GateContainer{Image: "testcontainers/ryuk:0.14.0", Name: "reaper"}
	if !reaper.IsReaper() {
		t.Error("IsReaper() = false for a ryuk image")
	}
	if (GateContainer{Image: "dolt/dolt-sql-server:2.2.0", Name: "ryuk-timer"}).IsReaper() != true {
		t.Error("IsReaper() should match on the name too")
	}
	if (GateContainer{Image: "alpine:3.20", Name: "plain"}).IsReaper() {
		t.Error("IsReaper() = true for an unrelated container")
	}

	hyphenated := GateContainer{Labels: map[string]string{"org.testcontainers.session-id": "s"}}
	if got := hyphenated.SessionID(); got != "s" {
		t.Errorf("SessionID() = %q, want %q — the key is matched, not spelled", got, "s")
	}
	if got := (GateContainer{}).SessionID(); got != "" {
		t.Errorf("SessionID() = %q, want empty for a container with no labels", got)
	}

	summary := (GateContainer{Labels: map[string]string{"b": "2", "a": "1"}}).LabelSummary()
	if summary != "a=1,b=2" {
		t.Errorf("LabelSummary() = %q, want sorted pairs", summary)
	}
	if got := (GateContainer{}).LabelSummary(); got != "(none)" {
		t.Errorf("LabelSummary() = %q, want \"(none)\"", got)
	}
}
