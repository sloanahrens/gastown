package deepreview

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The parity fixture is one recorded run of scripts/deep-review.sh plan against
// a ledger and run notes checked into testdata/parity. The design keeps the
// shell as the reference until this test passes, so the recording is the
// expectation: no value here is recomputed from the fixture.
const (
	parityDir = "testdata/parity"
	// parityNow is the --now the recorded run passed: a cursor line sets the
	// window here, so the value only has to sit after the ledger's landings.
	parityNow = "2026-10-10T10:00:00Z"
)

// recordedLanding is the landings.jsonl line the shell wrote, parsed with the
// fields the parity test compares. landed_at stays a string so the test holds
// the shell's spelling (fraction and offset included) against the Go plan's.
type recordedLanding struct {
	Bead         string   `json:"bead"`
	Head         string   `json:"head"`
	LandedCommit string   `json:"landed_commit"`
	LandedAt     string   `json:"landed_at"`
	RiskPaths    []string `json:"risk_paths"`
}

// TestBuildPlanMatchesTheRecordedShellPlan is the design's parity gate before
// the bash is deleted: the same ledger and run notes go through the recorded
// shell plan and BuildPlan, and the slice, cursor and skipped count agree.
func TestBuildPlanMatchesTheRecordedShellPlan(t *testing.T) {
	t.Parallel()
	ledger := parityFile(t, "ledger.jsonl")
	notes := parityFile(t, "notes.txt")
	expected := parityFields(t, parityFile(t, "expected.txt"))
	recorded := recordedLandings(t)

	now, err := ParseTimestamp(parityNow)
	if err != nil {
		t.Fatalf("fixture now: %v", err)
	}
	plan, err := BuildPlan(notes, []byte(ledger), now.Time)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	// Landings mode is what makes the fixture repo-free: the shell builds the
	// risky landings and no rotation slice, so the recording depends on the
	// ledger and the notes alone.
	if mode := expected["mode"]; mode != "landings" {
		t.Fatalf("recorded mode = %q, want landings: the fixture must not claim a rotation slice", mode)
	}
	if plan.Quiet {
		t.Fatal("the fixture plan is quiet, so it compares no landing")
	}
	if plan.Unparsed == 0 {
		t.Fatal("the fixture plan skipped no line, so the skipped count compares nothing")
	}

	if got, want := beadsOf(plan), beadsOfRecorded(recorded); got != want {
		t.Errorf("slice = %q, the shell recorded %q", got, want)
	}
	for i, want := range recorded {
		if i >= len(plan.Landings) {
			break
		}
		got := plan.Landings[i]
		if got.Bead != want.Bead || got.Head != want.Head ||
			got.LandedCommit != want.LandedCommit || got.LandedAt.Text != want.LandedAt ||
			!reflect.DeepEqual(got.RiskPaths, want.RiskPaths) {
			t.Errorf("landing %d = %+v, the shell recorded %+v", i, got, want)
		}
	}

	if got, want := plan.ReviewedUntil.Text, expected["reviewed_until_new"]; got != want {
		t.Errorf("cursor = %s, the shell recorded %s", got, want)
	}
	if got, want := plan.Since.Text, expected["reviewed_until_old"]; got != want {
		t.Errorf("window start = %s, the shell recorded %s", got, want)
	}
	if got, want := strconv.Itoa(len(plan.Landings)), expected["landings"]; got != want {
		t.Errorf("slice size = %s, the shell recorded %s", got, want)
	}
	if got, want := strconv.Itoa(plan.Unparsed), expected["unparsed"]; got != want {
		t.Errorf("skipped count = %s, the shell recorded %s", got, want)
	}
	if got, want := strconv.Itoa(plan.Records), expected["records"]; got != want {
		t.Errorf("record count = %s, the shell recorded %s", got, want)
	}
	if got, want := strconv.FormatBool(plan.Quiet), expected["quiet"]; got != want {
		t.Errorf("quiet = %s, the shell recorded %s", got, want)
	}
}

// parityFile reads one fixture file, failing with the path so a moved fixture
// names itself rather than reading as a plan mismatch.
func parityFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(parityDir + "/" + name)
	if err != nil {
		t.Fatalf("read parity fixture: %v", err)
	}
	return string(data)
}

// parityFields parses the shell's stdout, which is one key=value line per field.
func parityFields(t *testing.T, out string) map[string]string {
	t.Helper()
	fields := make(map[string]string)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("parity recording has a line without '=': %q", line)
		}
		fields[key] = value
	}
	if len(fields) == 0 {
		t.Fatal("parity recording is empty")
	}
	return fields
}

// recordedLandings reads the shell's landings.jsonl, the run's slice.
func recordedLandings(t *testing.T) []recordedLanding {
	t.Helper()
	lines := strings.Split(strings.TrimRight(parityFile(t, "landings.jsonl"), "\n"), "\n")
	landings := make([]recordedLanding, 0, len(lines))
	for i, line := range lines {
		var l recordedLanding
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("recorded landing %d: %v", i+1, err)
		}
		landings = append(landings, l)
	}
	return landings
}

// beadsOfRecorded names the recorded slice in order, as beadsOf does for the
// plan, so a mismatch prints the two orders rather than a struct diff.
func beadsOfRecorded(landings []recordedLanding) string {
	beads := make([]string, len(landings))
	for i, l := range landings {
		beads[i] = l.Bead
	}
	return strings.Join(beads, " ")
}
