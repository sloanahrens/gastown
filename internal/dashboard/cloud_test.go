package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"
)

// The panel's clock: every fixture below is written against this instant, so an
// age in a note is the age the test meant.
var cloudNow = time.Date(2026, 10, 8, 15, 30, 0, 0, time.UTC)

// cloudFixture writes a reports directory holding report.json and a heartbeat,
// and returns its path. Either file is left out when its argument is empty, and
// the report is written verbatim.
func cloudFixture(t *testing.T, report, heartbeat string) string {
	t.Helper()
	dir := t.TempDir()
	if report != "" {
		if err := os.WriteFile(filepath.Join(dir, cloudReportFile), []byte(report), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if heartbeat != "" {
		if err := os.WriteFile(filepath.Join(dir, cloudHeartbeatFile), []byte(heartbeat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// cloudReportJSON builds a valid report around one project and one finding per
// severity given.
func cloudReportJSON(t *testing.T, findings ...map[string]any) string {
	t.Helper()
	rep := map[string]any{
		"SchemaVersion": cloudSchema,
		"RunID":         "98e841ea48f17737c2bc79a4f1e88839",
		"StartedAt":     "2026-10-08T15:24:32-05:00",
		"FinishedAt":    "2026-10-08T15:24:42-05:00",
		"Projects":      []string{"gt-fractals-m4xq", "gt-platform-m4xq"},
		"Findings":      findings,
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func cloudFindingMap(severity, kind, project, resource, detail string) map[string]any {
	return map[string]any{
		"ID": "519c33e73434a4af", "Kind": kind, "Severity": severity,
		"Project": project, "Resource": resource, "Detail": detail,
		"ObservedAt": "2026-10-08T15:24:32-05:00",
	}
}

// A report the patrol wrote a minute ago reads fresh, with its run, its
// projects and its findings.
func TestCloudReadsAFreshReport(t *testing.T) {
	t.Parallel()

	dir := cloudFixture(t,
		cloudReportJSON(t,
			cloudFindingMap("normal", "state_missing", "gt-fractals-m4xq", "fractals/prod", "no checkpoint"),
			cloudFindingMap("urgent", "orphan_resource", "gt-platform-m4xq", "run/api", "no stack"),
		),
		cloudNow.Add(-time.Minute).Format(time.RFC3339))

	c := NewCloudReader(dir).Read(cloudNow)
	if c.State != CloudFresh || c.Note != "fresh" {
		t.Fatalf("state %q note %q, want fresh", c.State, c.Note)
	}
	if c.NoPatrol {
		t.Error("a machine with a reports directory reports no patrol")
	}
	if c.LastRun == nil || !c.LastRun.Equal(cloudNow.Add(-time.Minute)) {
		t.Errorf("last run %v, want the heartbeat", c.LastRun)
	}
	if got := strings.Join(c.Projects, ","); got != "gt-fractals-m4xq,gt-platform-m4xq" {
		t.Errorf("projects %q", got)
	}
	if c.Total != 2 || c.More != 0 {
		t.Errorf("total %d more %d, want 2 and 0", c.Total, c.More)
	}
	if c.Counts != (CloudCounts{Urgent: 1, Normal: 1}) {
		t.Errorf("counts %+v", c.Counts)
	}
	if len(c.Findings) != 2 || c.Findings[0].Severity != "urgent" {
		t.Errorf("findings are not the report's, worst first: %+v", c.Findings)
	}
	if !c.Findings[0].ObservedAt.Equal(time.Date(2026, 10, 8, 20, 24, 32, 0, time.UTC)) {
		t.Errorf("observed at %v", c.Findings[0].ObservedAt)
	}
}

// A report that found nothing is a fresh report with an empty list, not an
// error: the panel says there are no findings rather than that it could not
// read any.
func TestCloudReadsAPatrolThatFoundNothing(t *testing.T) {
	t.Parallel()

	dir := cloudFixture(t, cloudReportJSON(t), cloudNow.Format(time.RFC3339))
	c := NewCloudReader(dir).Read(cloudNow)
	if c.State != CloudFresh {
		t.Fatalf("state %q note %q", c.State, c.Note)
	}
	if c.Total != 0 || c.More != 0 || len(c.Findings) != 0 {
		t.Errorf("a report with no findings has %d of them", c.Total)
	}
	if c.Counts != (CloudCounts{}) {
		t.Errorf("counts %+v", c.Counts)
	}
	if c.Findings == nil {
		t.Error("the page's list is null rather than empty")
	}
}

// A heartbeat older than the patrol's own run interval reads overdue, and a
// heartbeat still inside it does not: the boundary is what tells a late patrol
// from one that simply has not run yet.
func TestCloudReadsALatePatrolAsOverdue(t *testing.T) {
	t.Parallel()

	report := cloudReportJSON(t, cloudFindingMap("low", "state_missing", "gt-fractals-m4xq", "fractals/prod", "no checkpoint"))
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{cloudOverdue - time.Minute, CloudFresh},
		{cloudOverdue, CloudFresh},
		{cloudOverdue + time.Minute, CloudOverdue},
		{48 * time.Hour, CloudOverdue},
	} {
		dir := cloudFixture(t, report, cloudNow.Add(-tc.age).Format(time.RFC3339))
		c := NewCloudReader(dir).Read(cloudNow)
		if c.State != tc.want {
			t.Errorf("a heartbeat %v old reads %q, want %q", tc.age, c.State, tc.want)
		}
		if tc.want == CloudOverdue && c.Note != "overdue" {
			t.Errorf("an overdue panel notes %q", c.Note)
		}
	}
}

// Every input the panel cannot use is a named state rather than a blank pane or
// a crash: no directory, no report yet, a report too big to be the patrol's, one
// that is not JSON, one from a schema this panel does not know, and a heartbeat
// that is missing or is not a timestamp.
func TestCloudNamesEveryStateItCannotRead(t *testing.T) {
	t.Parallel()

	valid := cloudReportJSON(t, cloudFindingMap("normal", "state_missing", "gt-fractals-m4xq", "fractals/prod", "no checkpoint"))
	heartbeat := cloudNow.Format(time.RFC3339)
	empty := t.TempDir()
	noDir := filepath.Join(t.TempDir(), "gone")
	oversize := cloudFixture(t, `{"SchemaVersion":1,"Projects":[],"Findings":[],"Pad":"`+strings.Repeat("x", cloudReportMaxBytes)+`"}`, heartbeat)

	for _, tc := range []struct {
		name   string
		dir    string
		state  string
		note   string
		patrol bool
	}{
		{"no directory", noDir, CloudMissing, cloudNoPatrol, true},
		{"directory with no report", empty, CloudMissing, cloudNoReport, false},
		{"oversize report", oversize, CloudError, cloudOversize, false},
		{"report that is not JSON", cloudFixture(t, "{not json", heartbeat), CloudError, "the report is not valid JSON", false},
		{"unknown schema", cloudFixture(t, `{"SchemaVersion":7,"Projects":[],"Findings":[]}`, heartbeat), CloudError, "unknown schema version 7", false},
		{"no heartbeat", cloudFixture(t, valid, ""), CloudError, "the patrol's heartbeat is missing", false},
		{"heartbeat that is not a time", cloudFixture(t, valid, "yesterday"), CloudError, cloudBadHeartbeat, false},
	} {
		c := NewCloudReader(tc.dir).Read(cloudNow)
		if c.State != tc.state || c.Note != tc.note {
			t.Errorf("%s: state %q note %q, want %q %q", tc.name, c.State, c.Note, tc.state, tc.note)
		}
		if c.NoPatrol != tc.patrol {
			t.Errorf("%s: no_patrol %v", tc.name, c.NoPatrol)
		}
		// A state the reader could not read past still leaves the page a page:
		// the fields it always draws are there.
		if c.Projects == nil || c.Findings == nil {
			t.Errorf("%s: projects or findings are null in the page's payload", tc.name)
		}
	}
}

// The report is another account's text. Control characters and invisible
// format characters are stripped, an over-long field is cut to the cap, and an
// HTML tag is left as the text it is — the page escapes it by writing it with
// textContent.
func TestCloudStripsAndCapsHostileReportText(t *testing.T) {
	t.Parallel()

	hostile := "bell:\x07 esc:\x1b[31m nul:\x00 rlo:‮invisible:​ <b>bold</b> " + strings.Repeat("x", 400)
	dir := cloudFixture(t,
		cloudReportJSON(t, cloudFindingMap("urgent", "state_\x07missing", "gt-\x00fractals", "fractals/‮prod", hostile)),
		cloudNow.Format(time.RFC3339))

	c := NewCloudReader(dir).Read(cloudNow)
	if c.State != CloudFresh {
		t.Fatalf("state %q note %q", c.State, c.Note)
	}
	f := c.Findings[0]
	for name, s := range map[string]string{
		"kind": f.Kind, "project": f.Project, "resource": f.Resource, "detail": f.Detail,
	} {
		for _, r := range s {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				t.Errorf("%s kept the invisible rune %U: %q", name, r, s)
			}
		}
		if strings.ContainsAny(s, "\n\r\t") {
			t.Errorf("%s kept a line break: %q", name, s)
		}
	}
	for name, tc := range map[string]struct {
		got string
		max int
	}{
		"kind":     {f.Kind, cloudKindMax},
		"project":  {f.Project, cloudProjectMax},
		"resource": {f.Resource, cloudResourceMax},
		"detail":   {f.Detail, cloudDetailMax},
	} {
		if n := len([]rune(tc.got)); n > tc.max+1 {
			t.Errorf("%s is %d runes, over the %d-rune cap: %q", name, n, tc.max, tc.got)
		}
		if !strings.HasSuffix(tc.got, "…") && len([]rune(tc.got)) > tc.max {
			t.Errorf("%s was cut without a mark: %q", name, tc.got)
		}
	}
	// The tag is text: nothing here escapes it, because nothing here renders
	// markup. The page's renderCloud writes every cell with el()'s textContent.
	if !strings.Contains(f.Detail, "<b>bold</b>") {
		t.Errorf("the reader mangled punctuation it was not asked to touch: %q", f.Detail)
	}
	if f.Kind != "state_missing" || f.Project != "gt-fractals" || f.Resource != "fractals/prod" {
		t.Errorf("stripping changed more than the invisible runes: %+v", f)
	}
}

// A report with more findings than the page draws keeps the hundred worst, and
// still says how many there were and at which severities.
func TestCloudCapsFindingsButCountsThemAll(t *testing.T) {
	t.Parallel()

	var findings []map[string]any
	for i := 0; i < 150; i++ {
		severity := "low"
		switch {
		case i < 5:
			severity = "urgent"
		case i < 40:
			severity = "normal"
		case i == 149:
			severity = "weird"
		}
		findings = append(findings, cloudFindingMap(severity, "state_missing", "gt-fractals-m4xq", "fractals/prod", "detail"))
	}
	dir := cloudFixture(t, cloudReportJSON(t, findings...), cloudNow.Format(time.RFC3339))

	c := NewCloudReader(dir).Read(cloudNow)
	if len(c.Findings) != cloudMaxFindings {
		t.Errorf("kept %d findings, want %d", len(c.Findings), cloudMaxFindings)
	}
	if c.Total != 150 || c.More != 50 {
		t.Errorf("total %d more %d, want 150 and 50", c.Total, c.More)
	}
	if c.Counts != (CloudCounts{Urgent: 5, Normal: 35, Low: 109, Other: 1}) {
		t.Errorf("counts %+v, want the whole report", c.Counts)
	}
	// The cap keeps the worst: the five urgent and the forty-five next.
	if c.Findings[0].Severity != "urgent" || c.Findings[4].Severity != "urgent" || c.Findings[5].Severity != "normal" {
		t.Errorf("the kept list is not worst first: %+v", c.Findings[:6])
	}
	if !sort.SliceIsSorted(c.Findings, func(i, j int) bool { return cloudRank(c.Findings[i].Severity) < cloudRank(c.Findings[j].Severity) }) {
		t.Error("the findings are not ordered by severity")
	}
}

// The panel lists findings worst first, and a severity it does not know — the
// report's word is not checked against a list, since a patrol that grows a new
// one should not lose the finding — is listed after the three it does.
func TestCloudListsFindingsWorstFirst(t *testing.T) {
	t.Parallel()

	dir := cloudFixture(t, cloudReportJSON(t,
		cloudFindingMap("low", "state_missing", "gt-fractals-m4xq", "fractals/prod", "d"),
		cloudFindingMap("weird", "state_missing", "gt-fractals-m4xq", "fractals/prod", "d"),
		cloudFindingMap("urgent", "orphan_resource", "gt-platform-m4xq", "run/api", "d"),
		cloudFindingMap("normal", "state_missing", "gt-fractals-m4xq", "fractals/prod", "d"),
		cloudFindingMap("URGENT", "orphan_resource", "gt-platform-m4xq", "run/api", "d"),
	), cloudNow.Format(time.RFC3339))

	c := NewCloudReader(dir).Read(cloudNow)
	var got []string
	for _, f := range c.Findings {
		got = append(got, f.Severity)
	}
	want := "urgent,urgent,normal,low,weird"
	if strings.Join(got, ",") != want {
		t.Errorf("findings are %s, want %s", strings.Join(got, ","), want)
	}
	if c.Counts.Other != 1 {
		t.Errorf("counts %+v, want one of the unknown severity", c.Counts)
	}
}

// The reader only reads: a pass over the reports directory in every state
// leaves the directory exactly as it was, so nothing on this page can write to
// another account's output.
func TestCloudReaderWritesNothingUnderTheReportsDir(t *testing.T) {
	t.Parallel()

	dir := cloudFixture(t, cloudReportJSON(t, cloudFindingMap("urgent", "orphan_resource", "gt-platform-m4xq", "run/api", "no stack")), cloudNow.Format(time.RFC3339))
	before := cloudTree(t, dir)
	if len(before) != 2 {
		t.Fatalf("the fixture is not the two files it should be: %v", before)
	}
	r := NewCloudReader(dir)
	r.Read(cloudNow)                                          // fresh
	r.Read(cloudNow.Add(48 * time.Hour))                      // overdue
	r.Read(cloudNow)                                          // again, from a warm reader
	NewCloudReader(filepath.Join(dir, "gone")).Read(cloudNow) // no directory
	if after := cloudTree(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("a read changed the reports directory:\nbefore %v\nafter  %v", before, after)
	}
}

// cloudTree is every file under dir with its bytes, for the comparison above.
func cloudTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// The hub reads a machine with no patrol at the slow interval, and a machine
// with one at the page's own. A patrol is installed rarely and its report
// changes every six hours, so re-reading a directory that is not there at the
// page's rate would only be a minute's worth of the same sentence.
func TestCloudWorkerReadsAPatrolAtTheIntervalAndItsAbsenceSlowly(t *testing.T) {
	t.Parallel()

	now := cloudNow
	patrol := false
	reads := 0
	h := NewHub(Config{Now: func() time.Time { return now }, Cloud: func() *Cloud {
		reads++
		if !patrol {
			return &Cloud{At: now, State: CloudMissing, Note: cloudNoPatrol, NoPatrol: true}
		}
		return &Cloud{At: now, State: CloudFresh, Note: "fresh"}
	}})

	h.pollCloud()
	if reads != 1 {
		t.Fatalf("the first poll made %d reads, want 1", reads)
	}
	if got := h.State().Cloud; got == nil || got.State != CloudMissing {
		t.Fatalf("the panel does not hold the reader's report: %+v", got)
	}

	// Inside the slow interval the hub leaves a machine with no patrol alone.
	now = now.Add(time.Minute)
	h.pollCloud()
	if reads != 1 {
		t.Errorf("a machine with no patrol was re-read after a minute (%d reads)", reads)
	}
	// At the slow interval it looks again, and a patrol that has appeared since
	// is picked up at the next one.
	now = now.Add(cloudSlowEvery - time.Minute)
	h.pollCloud()
	if reads != 2 {
		t.Errorf("the slow interval did not re-read (%d reads)", reads)
	}
	patrol = true
	now = now.Add(cloudSlowEvery)
	h.pollCloud()
	if reads != 3 {
		t.Fatalf("a patrol that appeared was not read (%d reads)", reads)
	}
	if got := h.State().Cloud; got == nil || got.State != CloudFresh {
		t.Fatalf("the panel did not take up the fresh report: %+v", got)
	}
	// From then on the machine is read like any other panel: the tick after the
	// last read goes through.
	now = now.Add(time.Minute)
	h.pollCloud()
	if reads != 4 {
		t.Errorf("a fresh report was not re-read after an interval (%d reads)", reads)
	}
}
