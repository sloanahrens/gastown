package jobs

import (
	"strings"
	"testing"
	"time"
)

// writeNodeDiskRecord writes the town's node disk record body.
func writeNodeDiskRecord(t *testing.T, root, body string) {
	t.Helper()
	writeFile(t, nodeDiskRecordPath(root), body)
}

// nodeDiskRowsNamed returns the node_disk rows whose name starts with prefix.
func nodeDiskRowsNamed(rows []Job, prefix string) []Job {
	var found []Job
	for _, j := range rows {
		if strings.HasPrefix(j.Name, prefix) {
			found = append(found, j)
		}
	}
	return found
}

// a reading at the staleness bound is still current; one second past it, or
// dated after now, is unknown, so the bound and the clock alone decide. The
// byte counts appear only when the entry holds them.
func TestJobsNodeDisk(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		ago       time.Duration
		wantState State
	}{
		{name: "a fresh reading is ok", ago: 5 * time.Minute, wantState: StateOK},
		{name: "a reading at the bound is ok", ago: nodeDiskStaleAfter, wantState: StateOK},
		{name: "a reading past the bound is unknown", ago: nodeDiskStaleAfter + time.Second, wantState: StateUnknown},
		{name: "a reading after now is unknown", ago: -time.Second, wantState: StateUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			readAt := testNow.Add(-c.ago)
			root := t.TempDir()
			writeNodeDiskRecord(t, root, `{"nodes":{`+
				`"colima-gt-control":{"used_percent":20,"used_bytes":20,"read_at":"`+readAt.Format(time.RFC3339)+`"},`+
				`"colima-gt-worker":{"used_percent":90,"read_at":"`+readAt.Format(time.RFC3339)+`"}}}`)

			rows, err := List(root, Options{}, testNow)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			control := nodeDiskRowsNamed(rows, "node_disk:colima-gt-control")
			worker := nodeDiskRowsNamed(rows, "node_disk:colima-gt-worker")
			if len(control) != 1 || len(worker) != 1 {
				t.Fatalf("got %d control rows and %d worker rows, want one each: %+v", len(control), len(worker), rows)
			}

			j := control[0]
			if j.State != c.wantState {
				t.Errorf("State = %q, want %q", j.State, c.wantState)
			}
			if j.LastRun == nil || !j.LastRun.Equal(readAt) {
				t.Errorf("LastRun = %v, want %v", j.LastRun, readAt)
			}
			if j.Outcome != OutcomeOK {
				t.Errorf("Outcome = %q, want ok", j.Outcome)
			}
			if got := j.Stats["used_percent"]; got != 20 {
				t.Errorf("used_percent = %d, want 20", got)
			}
			if got := j.Stats["used_bytes"]; got != 20 {
				t.Errorf("used_bytes = %d, want 20", got)
			}

			w := worker[0]
			if got := w.Stats["used_percent"]; got != 90 {
				t.Errorf("worker used_percent = %d, want 90", got)
			}
			if _, ok := w.Stats["used_bytes"]; ok {
				t.Errorf("worker holds used_bytes = %d, want none: the entry holds no byte count", w.Stats["used_bytes"])
			}
		})
	}
}

// A missing, corrupt or node-less record each gives one unknown row named
// node_disk, never an empty list; a record that exists but cannot be parsed is
// a failed row.
func TestJobsNodeDiskRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		body        string // empty means no record file
		wantOutcome Outcome
	}{
		{name: "missing record", wantOutcome: ""},
		{name: "corrupt record", body: `{"nodes":`, wantOutcome: OutcomeFailed},
		{name: "record with no nodes", body: `{"nodes":{}}`, wantOutcome: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if c.body != "" {
				writeNodeDiskRecord(t, root, c.body)
			}

			rows, err := List(root, Options{}, testNow)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			found := nodeDiskRowsNamed(rows, nodeDiskJobName)
			if len(found) != 1 {
				t.Fatalf("got %d node_disk rows, want one: %+v", len(found), rows)
			}
			j := found[0]
			if j.Name != nodeDiskJobName {
				t.Errorf("Name = %q, want %q", j.Name, nodeDiskJobName)
			}
			if j.State != StateUnknown {
				t.Errorf("State = %q, want unknown", j.State)
			}
			if j.Outcome != c.wantOutcome {
				t.Errorf("Outcome = %q, want %q", j.Outcome, c.wantOutcome)
			}
			if j.LastRun != nil {
				t.Errorf("LastRun = %v, want none", j.LastRun)
			}
		})
	}
}

// A failed read is a failed outcome over the figures the last good reading
// left; a node whose every read failed has no figures, so the row invents none.
func TestJobsNodeDiskErrorEntry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		entry       string
		wantStats   map[string]int64
		wantLastRun bool
		wantState   State
	}{
		{
			name:      "failed read with no previous reading",
			entry:     `{"error":"connection refused","error_at":"` + testNow.Format(time.RFC3339) + `"}`,
			wantState: StateUnknown,
		},
		{
			name: "failed read keeps the last good reading",
			entry: `{"used_percent":20,"used_bytes":20,"read_at":"` + testNow.Add(-5*time.Minute).Format(time.RFC3339) +
				`","error":"connection refused","error_at":"` + testNow.Format(time.RFC3339) + `"}`,
			wantStats:   map[string]int64{"used_percent": 20, "used_bytes": 20},
			wantLastRun: true,
			wantState:   StateOK,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeNodeDiskRecord(t, root, `{"nodes":{"colima-gt-control":`+c.entry+`}}`)

			rows, err := List(root, Options{}, testNow)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			found := nodeDiskRowsNamed(rows, "node_disk:colima-gt-control")
			if len(found) != 1 {
				t.Fatalf("got %d control rows, want one: %+v", len(found), rows)
			}
			j := found[0]
			if j.Outcome != OutcomeFailed {
				t.Errorf("Outcome = %q, want failed", j.Outcome)
			}
			if j.State != c.wantState {
				t.Errorf("State = %q, want %q", j.State, c.wantState)
			}
			if (j.LastRun != nil) != c.wantLastRun {
				t.Errorf("LastRun = %v, want present = %v", j.LastRun, c.wantLastRun)
			}
			if len(j.Stats) != len(c.wantStats) {
				t.Errorf("Stats = %v, want %v", j.Stats, c.wantStats)
			}
			for name, want := range c.wantStats {
				if got := j.Stats[name]; got != want {
					t.Errorf("Stats[%s] = %d, want %d", name, got, want)
				}
			}
		})
	}
}

// The sandbox entry is the cluster's busiest node alone, so its row's name says
// so and it carries the used percent; a control node keeps its own row.
func TestJobsNodeDiskSandbox(t *testing.T) {
	t.Parallel()
	readAt := testNow.Add(-time.Minute)
	root := t.TempDir()
	writeNodeDiskRecord(t, root, `{"nodes":{`+
		`"sandbox":{"used_percent":88,"read_at":"`+readAt.Format(time.RFC3339)+`"},`+
		`"colima-gt-control":{"used_percent":20,"read_at":"`+readAt.Format(time.RFC3339)+`"}}}`)

	rows, err := List(root, Options{}, testNow)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := nodeDiskRowsNamed(rows, "node_disk:sandbox")
	if len(found) != 1 {
		t.Fatalf("got %d sandbox rows, want one: %+v", len(found), rows)
	}
	sandbox := found[0]
	if sandbox.Name != "node_disk:sandbox"+nodeDiskSandboxNote {
		t.Errorf("Name = %q, want %q", sandbox.Name, "node_disk:sandbox"+nodeDiskSandboxNote)
	}
	if !strings.Contains(sandbox.Name, "highest-used") {
		t.Errorf("Name = %q, want the highest-used-node note", sandbox.Name)
	}
	if got := sandbox.Stats["used_percent"]; got != 88 {
		t.Errorf("used_percent = %d, want 88", got)
	}
	if sandbox.State != StateOK {
		t.Errorf("State = %q, want ok", sandbox.State)
	}

	control := nodeDiskRowsNamed(rows, "node_disk:colima-gt-control")
	if len(control) != 1 {
		t.Fatalf("got %d control rows, want one: %+v", len(control), rows)
	}
	if control[0].Name != "node_disk:colima-gt-control" {
		t.Errorf("control Name = %q, want its own row", control[0].Name)
	}
	if got := control[0].Stats["used_percent"]; got != 20 {
		t.Errorf("control used_percent = %d, want 20", got)
	}
}
