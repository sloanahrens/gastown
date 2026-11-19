package bdgateway

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// TestCallRefusalLine: a line carries refusal only for a record that names a
// refusal, and Stats counts that reason once.
func TestCallRefusalLine(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	l, err := NewCallLog(opts)
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	l.Log(CallRecord{Class: "read", Caller: "c", Argv: []string{"show", "--json", "gt-abc"}, ExitCode: ExitRefused, Refusal: "forbidden"})
	l.Log(CallRecord{Class: "read", Caller: "c", Argv: []string{"show", "gt-abc"}})
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, filepath.Join(opts.Dir, DefaultCallLogName))
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2", len(lines))
	}
	if got := decodeLine(t, lines[0])["refusal"]; got != "forbidden" {
		t.Errorf("refused line refusal = %v, want forbidden", got)
	}
	if _, ok := decodeLine(t, lines[1])["refusal"]; ok {
		t.Errorf("unrefused line carries a refusal key")
	}
	if got := l.Stats().Refusals; !reflect.DeepEqual(got, map[string]uint64{"forbidden": 1}) {
		t.Errorf("Refusals = %v, want map[forbidden:1]", got)
	}
}

// TestCallRefusalStatsCap: the reason past maxRefusalKeys is counted under
// otherRefusal, and each refusal still counts as a call and as an error.
func TestCallRefusalStatsCap(t *testing.T) {
	t.Parallel()
	const reasons = maxRefusalKeys + 1
	l, err := newCallLog(testOptions(t), func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("newCallLog: %v", err)
	}
	for i := 0; i < reasons; i++ {
		l.Log(CallRecord{Class: "read", Caller: "c", Argv: []string{"show", "gt-abc"}, ExitCode: ExitRefused, Refusal: fmt.Sprintf("reason%02d", i)})
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := l.Stats()
	if len(got.Refusals) != maxRefusalKeys+1 {
		t.Errorf("kept %d refusal keys, want %d", len(got.Refusals), maxRefusalKeys+1)
	}
	if n := got.Refusals[otherRefusal]; n != 1 {
		t.Errorf("refusals[%s] = %d, want 1", otherRefusal, n)
	}
	if _, ok := got.Refusals[fmt.Sprintf("reason%02d", maxRefusalKeys)]; ok {
		t.Errorf("reason%02d kept its own key past the cap", maxRefusalKeys)
	}
	if got.Calls != reasons || got.Errors != reasons {
		t.Errorf("Calls, Errors = %d, %d; want %d, %d", got.Calls, got.Errors, reasons, reasons)
	}
}
