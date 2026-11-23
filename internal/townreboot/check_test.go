package townreboot

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestRebootCheckEngineOrder pins the engine's contract: every check runs once,
// in list order, and a check that reports a problem does not stop the rest.
func TestRebootCheckEngineOrder(t *testing.T) {
	t.Parallel()
	var ran []string
	checks := []Check{
		{Name: "databases", Run: func(context.Context) Result {
			ran = append(ran, "databases")
			return Result{OK: true, Detail: "3/3 readable"}
		}},
		{Name: "pods", Run: func(context.Context) Result {
			ran = append(ran, "pods")
			return Result{Detail: "gt-control not Running", Fix: "gt vms start"}
		}},
		{Name: "dns", Run: func(context.Context) Result {
			ran = append(ran, "dns")
			return Result{OK: true}
		}},
	}

	rows := RunAll(context.Background(), checks)

	if want := []string{"databases", "pods", "dns"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("checks ran %v, want %v", ran, want)
	}
	want := []Row{
		{Name: "databases", OK: true, Detail: "3/3 readable"},
		{Name: "pods", Detail: "gt-control not Running", Fix: "gt vms start"},
		{Name: "dns", OK: true},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v, want %+v", rows, want)
	}
}

// TestRebootCheckEngineNilRun pins the refusal: a check wired without a Run is
// a failed row, and the checks after it still run.
func TestRebootCheckEngineNilRun(t *testing.T) {
	t.Parallel()
	rows := RunAll(context.Background(), []Check{
		{Name: "unwired"},
		{Name: "databases", Run: func(context.Context) Result { return Result{OK: true} }},
	})

	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].OK || rows[0].Detail == "" {
		t.Errorf("unwired check row = %+v, want a failure that says why", rows[0])
	}
	if !rows[1].OK {
		t.Errorf("check after the unwired one = %+v, want it to have run and passed", rows[1])
	}
}

// TestRebootCheckEnginePanic pins the recovery: a panicking check becomes a
// failed row and the checks after it still run.
func TestRebootCheckEnginePanic(t *testing.T) {
	t.Parallel()
	rows := RunAll(context.Background(), []Check{
		{Name: "boom", Run: func(context.Context) Result { panic("kaboom") }},
		{Name: "after", Run: func(context.Context) Result { return Result{OK: true} }},
	})
	if len(rows) != 2 || rows[0].OK || rows[0].Detail != "check panicked: kaboom" || !rows[1].OK {
		t.Fatalf("rows = %+v, want a panic row then a passing check", rows)
	}
}

// TestRebootCheckClusterForgejoCompleted pins the pod phases: a completed job
// pod is not a failure, a namespace holding only completed pods fails, and a
// failed pod still fails. The fixtures and helpers come from cluster_test.go.
func TestRebootCheckClusterForgejoCompleted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, out, wantDetail string
		wantOK                bool
	}{
		{"ready pods beside a completed one", `{"items":[{"metadata":{"name":"forgejo-0"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},{"metadata":{"name":"forgejo-1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},{"metadata":{"name":"forgejo-runner-done"},"status":{"phase":"Succeeded","conditions":[{"type":"Ready","status":"False"}]}}]}`, "2 pods ready, 1 completed", true},
		{"only completed pods", `{"items":[{"metadata":{"name":"forgejo-runner-done"},"status":{"phase":"Succeeded","conditions":[{"type":"Ready","status":"False"}]}}]}`, "no running pods (1 completed)", false},
		{"a failed pod", `{"items":[{"metadata":{"name":"forgejo-runner-failed"},"status":{"phase":"Failed","conditions":[{"type":"Ready","status":"False"}]}}]}`, "not ready: forgejo-runner-failed", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := forgejoRow(podListing(tt.out))
			if row.OK != tt.wantOK || row.Detail != tt.wantDetail {
				t.Errorf("row = %+v, want OK=%v, detail %q", row, tt.wantOK, tt.wantDetail)
			}
		})
	}
}

// TestRebootCheckEngineFormat pins both output forms: the text is one line per
// row ending with the boot-noise note, and the JSON carries the same rows and
// the note.
func TestRebootCheckEngineFormat(t *testing.T) {
	t.Parallel()
	rows := []Row{
		{Name: "databases", OK: true, Detail: "3/3 readable"},
		{Name: "pods", Detail: "gt-control not Running", Fix: "gt vms start"},
		{Name: "dns"},
	}

	text := Format(rows, false)
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	wantLines := []string{
		"ok   databases: 3/3 readable",
		"FAIL pods: gt-control not Running (fix: gt vms start)",
		"FAIL dns",
		"note: " + bootNoiseNote,
	}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Errorf("text lines = %q, want %q", lines, wantLines)
	}

	out := Format(rows, true)
	var got report
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("JSON form does not parse: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(got.Rows, rows) {
		t.Errorf("JSON rows = %+v, want %+v", got.Rows, rows)
	}
	if got.Note != bootNoiseNote {
		t.Errorf("JSON note = %q, want %q", got.Note, bootNoiseNote)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("JSON form does not end with a newline: %q", out)
	}
}

// TestRebootCheckEngineEmpty pins the empty report: the text is the note alone,
// and the JSON rows field is an empty array a reader can range rather than null.
func TestRebootCheckEngineEmpty(t *testing.T) {
	t.Parallel()
	if text, want := Format(nil, false), "note: "+bootNoiseNote+"\n"; text != want {
		t.Errorf("empty text = %q, want %q", text, want)
	}

	out := Format(nil, true)
	var got report
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("JSON form does not parse: %v\n%s", err, out)
	}
	if got.Rows == nil || len(got.Rows) != 0 {
		t.Errorf("JSON rows = %#v, want an empty array", got.Rows)
	}
}
