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
