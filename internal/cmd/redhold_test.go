package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// redHoldTestFailingWriter fails every write, driving the print error branches.
type redHoldTestFailingWriter struct{}

func (redHoldTestFailingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// redHoldTestRun executes `gt red-hold status` over root and returns its output.
func redHoldTestRun(t *testing.T, root string, extra ...string) (string, error) {
	t.Helper()
	c := newRedHoldCmd()
	var out bytes.Buffer
	c.SetArgs(append([]string{"status", "--town-root", root}, extra...))
	c.SetOut(&out)
	c.SetErr(io.Discard)
	err := c.Execute()
	return out.String(), err
}

func TestRedHoldCmdStatusPrintsReasons(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rec := dispatch.RedHoldRecord{Reasons: []dispatch.RedHoldReason{
		{Kind: dispatch.RedHoldMainRed, Since: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), Detail: "main is red", CureBeads: []string{"gt-aaa", "gt-bbb"}},
		{Kind: dispatch.RedHoldBehindMain, Since: time.Date(2026, 11, 20, 13, 30, 0, 0, time.UTC), Detail: "12 commits behind"},
	}}
	if err := dispatch.WriteRedHold(root, rec); err != nil {
		t.Fatal(err)
	}
	got, err := redHoldTestRun(t, root)
	if err != nil {
		t.Fatalf("exit %d, want 0: %v", exitCodeForError(err), err)
	}
	want := "main-red since 2026-11-20T12:00:00Z main is red cure: gt-aaa gt-bbb\nbehind-main since 2026-11-20T13:30:00Z 12 commits behind\n"
	if got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestRedHoldCmdStatusOpen(t *testing.T) {
	t.Parallel()
	got, err := redHoldTestRun(t, t.TempDir())
	if err != nil {
		t.Fatalf("exit %d, want 0: %v", exitCodeForError(err), err)
	}
	if got != "red hold: open\n" {
		t.Errorf("output = %q, want %q", got, "red hold: open\n")
	}
}

func TestRedHoldCmdStatusBadFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(dispatch.RedHoldPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dispatch.RedHoldPath(root), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := redHoldTestRun(t, root)
	if err == nil {
		t.Fatalf("output = %q, a corrupt file exited 0; want an error", got)
	}
}

func TestRedHoldCmdStatusNoTown(t *testing.T) {
	t.Parallel()
	c := newRedHoldCmdWith(func() (string, error) { return "", errors.New("no town") })
	c.SetArgs([]string{"status"})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.Execute(); err == nil {
		t.Fatal("a failed town lookup exited 0; want an error")
	}
}

// redHoldTestHeldTown is a town root holding one reason.
func redHoldTestHeldTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	rec := dispatch.RedHoldRecord{Reasons: []dispatch.RedHoldReason{
		{Kind: dispatch.RedHoldSweepRed, Since: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)},
	}}
	if err := dispatch.WriteRedHold(root, rec); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRedHoldCmdStatusJSON(t *testing.T) {
	t.Parallel()
	want := dispatch.RedHoldRecord{Reasons: []dispatch.RedHoldReason{
		{Kind: dispatch.RedHoldMainRed, Since: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), CureBeads: []string{"gt-aaa", "gt-bbb"}},
		{Kind: dispatch.RedHoldBehindMain, Since: time.Date(2026, 11, 20, 13, 30, 0, 0, time.UTC), Detail: "12 commits behind"},
	}}
	root := t.TempDir()
	if err := dispatch.WriteRedHold(root, want); err != nil {
		t.Fatal(err)
	}
	got, err := redHoldTestRun(t, root, "--json")
	if err != nil {
		t.Fatalf("exit %d, want 0: %v", exitCodeForError(err), err)
	}
	var back dispatch.RedHoldRecord
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("output %q does not decode: %v", got, err)
	}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("record = %+v, want %+v", back, want)
	}
	open, err := redHoldTestRun(t, t.TempDir(), "--json")
	if err != nil {
		t.Fatalf("open: exit %d, want 0: %v", exitCodeForError(err), err)
	}
	if open != "{\n  \"reasons\": []\n}\n" {
		t.Errorf("open output = %q, want an empty reasons list", open)
	}
}

func TestRedHoldCmdStatusWriteError(t *testing.T) {
	t.Parallel()
	for _, root := range []string{t.TempDir(), redHoldTestHeldTown(t)} {
		for _, args := range [][]string{
			{"status", "--town-root", root}, {"status", "--json", "--town-root", root},
		} {
			c := newRedHoldCmd()
			c.SetArgs(args)
			c.SetOut(redHoldTestFailingWriter{})
			c.SetErr(io.Discard)
			if err := c.Execute(); err == nil {
				t.Errorf("a failing writer over %s exited 0; want an error", root)
			}
		}
	}
}
