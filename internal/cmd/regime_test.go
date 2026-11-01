package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/metrics"
)

// regimeTestNow is the instant the record command runs at in these tests.
var regimeTestNow = time.Date(2026, 11, 1, 10, 47, 0, 0, time.UTC)

// regimeTestCommand runs gt regime over town, the fixed instant and actor, as
// the process builds it over the working directory, the wall clock and
// detectActor.
func regimeTestCommand(t *testing.T, town string, args ...string) error {
	t.Helper()
	cmd := newRegimeCommand(
		func() (string, error) { return town, nil },
		func() time.Time { return regimeTestNow },
		func() string { return "gastown/polecats/capable" },
	)
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd.Execute()
}

// TestRegimeRecordRefusesBadInput runs the command as built and requires each
// bad input to exit 1, name what is wrong, and write no file.
func TestRegimeRecordRefusesBadInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "an unknown kind",
			args:    []string{"record", "--kind", "seat", "--before", "main@a", "--after", "main@b", "--reason", "seats changed"},
			wantErr: "unknown kind",
		},
		{
			name:    "an empty reason",
			args:    []string{"record", "--kind", "gate", "--before", "main@a", "--after", "main@b", "--reason", "  "},
			wantErr: "empty reason",
		},
		{
			name:    "no before ref",
			args:    []string{"record", "--kind", "gate", "--after", "main@b", "--reason", "gate raised"},
			wantErr: "before",
		},
		{
			name:    "no after ref",
			args:    []string{"record", "--kind", "gate", "--before", "main@a", "--reason", "gate raised"},
			wantErr: "after",
		},
		{
			name:    "no kind",
			args:    []string{"record", "--before", "main@a", "--after", "main@b", "--reason", "gate raised"},
			wantErr: "kind",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			err := regimeTestCommand(t, town, tc.args...)
			if got := exitCodeForError(err); got != 1 {
				t.Fatalf("record %v exits %d, want 1 (err %v)", tc.args, got, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantErr)
			}
			if _, statErr := os.Stat(metrics.RegimeEventsPath(town)); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("a refused record left %s behind (%v)", metrics.RegimeEventsPath(town), statErr)
			}
		})
	}
}

// TestRegimeRecordAppendsTheEvent runs the command as built and reads the line
// it wrote: the record's fields as the flags supplied them, stamped at the
// command's instant, in the town's runtime file.
func TestRegimeRecordAppendsTheEvent(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	err := regimeTestCommand(t, town,
		"record", "--kind", "gate", "--before", "main@a", "--after", "main@b",
		"--commit", "abc123", "--reason", "gate raised to 3")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	path := metrics.RegimeEventsPath(town)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1: %q", len(lines), data)
	}
	var got metrics.RegimeEventRecord
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("decoding the record: %v", err)
	}
	want := metrics.RegimeEventRecord{
		TS:        regimeTestNow,
		Kind:      "gate",
		BeforeRef: "main@a",
		AfterRef:  "main@b",
		Commit:    "abc123",
		Reason:    "gate raised to 3",
		Actor:     "gastown/polecats/capable",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record = %#v, want %#v", got, want)
	}
}

// TestRegimeRecordActorFlagOverridesTheSession requires --actor to name the
// record's actor over the session's own.
func TestRegimeRecordActorFlagOverridesTheSession(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	err := regimeTestCommand(t, town,
		"record", "--kind", "install", "--before", "none", "--after", "main@a",
		"--reason", "gt installed", "--actor", "overseer")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	data, err := os.ReadFile(metrics.RegimeEventsPath(town))
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	var got metrics.RegimeEventRecord
	if err := json.Unmarshal(bytes.TrimSpace(data), &got); err != nil {
		t.Fatalf("decoding the record: %v", err)
	}
	if got.Actor != "overseer" {
		t.Fatalf("actor = %q, want the --actor value %q", got.Actor, "overseer")
	}
}

// TestRegimeRecordOutsideATownExitsOne requires a record that cannot find the
// town to exit 1 with a message rather than write anywhere.
func TestRegimeRecordOutsideATownExitsOne(t *testing.T) {
	t.Parallel()
	cmd := newRegimeCommand(
		func() (string, error) { return "", errors.New("no workspace") },
		func() time.Time { return regimeTestNow },
		func() string { return "gastown/polecats/capable" },
	)
	cmd.SetArgs([]string{"record", "--kind", "gate", "--before", "a", "--after", "b", "--reason", "gate raised"})
	err := cmd.Execute()
	if got := exitCodeForError(err); got != 1 {
		t.Fatalf("record with no town exits %d, want 1 (err %v)", got, err)
	}
	if !strings.Contains(err.Error(), "Gas Town workspace") {
		t.Fatalf("error = %q, want it to say there is no Gas Town workspace", err)
	}
}
