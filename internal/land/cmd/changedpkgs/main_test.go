package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/presubmit"
)

// testDoc is a pod-engine document with one inventory row, whose Evidence cell
// cites internal/a/a.go:4, and a prose line ref to internal/c/c.go:12.
const testDoc = "## The inventory\n\n" +
	"| File | Function | Line | Source | Asks the engine for | Disposition | Evidence |\n" +
	"| --- | --- | --- | --- | --- | --- | --- |\n" +
	"| `internal/a/a.go` | `F` | 4 | `docker x` | one | mac-job | `internal/a/a.go:4` |\n\n" +
	"## Named, not a call site\n\n" +
	"- `internal/c/c.go:12` names docker in prose.\n"

// fakeSeams records what a run asked for; its runInventory returns inventory.
type fakeSeams struct {
	doc       string
	docErr    error
	inventory error
	runs      int
}

func (f *fakeSeams) seams() seams {
	return seams{
		readDoc: func() ([]byte, error) {
			if f.docErr != nil {
				return nil, f.docErr
			}
			return []byte(f.doc), nil
		},
		runInventory: func() error {
			f.runs++
			return f.inventory
		},
		hasGo: func(string) bool { return true },
	}
}

// nameStatus builds one git diff --name-status -z record per change, the form
// the selector reads.
func nameStatus(records ...string) string {
	var b strings.Builder
	for _, r := range records {
		b.WriteString(strings.ReplaceAll(r, " ", "\x00"))
		b.WriteString("\x00")
	}
	return b.String()
}

func TestRunRefusesAnUnreadableDocumentInEveryModeWithoutRunningTheInventory(t *testing.T) {
	t.Parallel()
	for _, o := range []options{{}, {recovery: true}} {
		f := &fakeSeams{docErr: errors.New("permission denied"), inventory: errors.New("FAIL")}
		var stdout, stderr bytes.Buffer
		code := run(o, nameStatus("M internal/a/a.go"), f.seams(), &stdout, &stderr)
		if code != 1 {
			t.Fatalf("run(%+v) = %d, want 1", o, code)
		}
		if !strings.Contains(stderr.String(), "reading "+presubmit.PodEngineDoc) {
			t.Fatalf("stderr = %q, want the doc named", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q beside a refusal, want nothing", stdout.String())
		}
		if f.runs != 0 {
			t.Fatalf("run(%+v) ran the inventory %d times beside an unreadable doc", o, f.runs)
		}
	}
}

func TestRunRefusesAnInventoryWithNoRows(t *testing.T) {
	t.Parallel()
	f := &fakeSeams{doc: "## How to read a row\n\n| a | b |\n| - | - |\n", inventory: nil}
	var stdout, stderr bytes.Buffer
	if code := run(options{}, nameStatus("M internal/a/a.go"), f.seams(), &stdout, &stderr); code != 1 {
		t.Fatalf("run = %d, want 1 for an inventory with no rows", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q beside a refusal, want nothing", stdout.String())
	}
}

func TestRunPrintsThePodEnginePackageOnlyWhenAPinnedFileChanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		nameStatus string
		want       string
	}{
		{name: "the inventory's File column adds the inventory package", nameStatus: nameStatus("M internal/a/a.go"), want: "./internal/a\n./internal/podengine\n"},
		{name: "a prose line ref adds the inventory package", nameStatus: nameStatus("M internal/c/c.go"), want: "./internal/c\n./internal/podengine\n"},
		{name: "an unpinned file leaves the inventory package out", nameStatus: nameStatus("M internal/z/z.go"), want: "./internal/z\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeSeams{doc: testDoc}
			var stdout, stderr bytes.Buffer
			if code := run(options{}, tt.nameStatus, f.seams(), &stdout, &stderr); code != 0 {
				t.Fatalf("run = %d, stderr %q; want 0", code, stderr.String())
			}
			if stdout.String() != tt.want {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tt.want)
			}
			if f.runs != 0 {
				t.Fatalf("the default mode ran the inventory %d times, want 0", f.runs)
			}
		})
	}
}

func TestRunPrintsTheRecoveryOnceWhenAPinnedFileMovedAndTheInventoryFails(t *testing.T) {
	t.Parallel()
	f := &fakeSeams{doc: testDoc, inventory: errors.New("FAIL")}
	var stdout, stderr bytes.Buffer
	code := run(options{recovery: true}, nameStatus("M internal/a/a.go"), f.seams(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run = %d, stderr %q; want 0", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), presubmit.PodEngineRecovery); got != 1 {
		t.Fatalf("the recovery printed %d times, want once; stdout %q", got, stdout.String())
	}
	if stdout.String() != presubmit.PodEngineRecovery+"\n" {
		t.Fatalf("stdout = %q, want the recovery line alone", stdout.String())
	}
	if f.runs != 1 {
		t.Fatalf("the inventory ran %d times, want once", f.runs)
	}
}

func TestRunPrintsNoRecoveryWhenTheInventoryPassesOrNothingPinnedMoved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		nameStatus string
		inventory  error
		wantRuns   int
	}{
		{name: "a pinned file moved and the inventory passes", nameStatus: nameStatus("M internal/a/a.go"), inventory: nil, wantRuns: 1},
		{name: "no pinned file moved", nameStatus: nameStatus("M internal/z/z.go"), inventory: errors.New("FAIL"), wantRuns: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeSeams{doc: testDoc, inventory: tt.inventory}
			var stdout, stderr bytes.Buffer
			if code := run(options{recovery: true}, tt.nameStatus, f.seams(), &stdout, &stderr); code != 0 {
				t.Fatalf("run = %d, stderr %q; want 0", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want no recovery", stdout.String())
			}
			if f.runs != tt.wantRuns {
				t.Fatalf("the inventory ran %d times, want %d", f.runs, tt.wantRuns)
			}
		})
	}
}
