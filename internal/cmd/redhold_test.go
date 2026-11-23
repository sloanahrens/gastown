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

// redHoldTestLiftRun executes `gt red-hold lift` over find with the actor fixed.
func redHoldTestLiftRun(t *testing.T, find func() (string, error), out io.Writer, args ...string) error {
	t.Helper()
	c := newRedHoldCmdWith(find, func(string) string { return "town/crew/overseer" })
	c.SetArgs(append([]string{"lift"}, args...))
	c.SetOut(out)
	c.SetErr(io.Discard)
	return c.Execute()
}

func TestRedHoldCmdLift(t *testing.T) {
	held, quiet, open := redHoldTestHeldTown(t), redHoldTestHeldTown(t), t.TempDir()
	corrupt := redHoldTestHeldTown(t)
	if err := os.WriteFile(dispatch.RedHoldPath(corrupt), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocked := redHoldTestHeldTown(t)
	// A directory where the request file goes makes the atomic rename fail.
	if err := os.MkdirAll(dispatch.RedHoldLiftPath(blocked), 0o755); err != nil {
		t.Fatal(err)
	}
	find := func(root string) func() (string, error) { return func() (string, error) { return root, nil } }
	cases := []struct {
		name, root, want string
		find             func() (string, error)
		args             []string
		wantReq, noRead  bool
	}{
		{name: "writes the request", root: held, find: find(held), want: "lift requested; the daemon decides on its next heartbeat; see gt red-hold status\n", args: []string{"--reason", "fixed"}, wantReq: true},
		{name: "missing reason", root: quiet, find: find(quiet)},
		{name: "blank reason", root: quiet, find: find(quiet), args: []string{"--reason", "  "}},
		{name: "nothing to lift", root: open, find: find(open), want: "nothing to lift\n", args: []string{"--reason", "fixed"}},
		{name: "no town", root: t.TempDir(), find: func() (string, error) { return "", errors.New("no town") }, args: []string{"--reason", "fixed"}},
		{name: "corrupt red hold", root: corrupt, find: find(corrupt), args: []string{"--reason", "fixed"}},
		{name: "request write fails", root: blocked, find: find(blocked), args: []string{"--reason", "fixed"}, noRead: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := redHoldTestLiftRun(t, tc.find, &out, tc.args...)
			if (err == nil) != tc.wantReq || (err != nil && exitCodeForError(err) != 1) {
				t.Fatalf("err = %v, want error %v exiting 1", err, !tc.wantReq)
			}
			if out.String() != tc.want {
				t.Errorf("output = %q, want %q", out.String(), tc.want)
			}
			if tc.noRead {
				return
			}
			req, ok, err := dispatch.ReadLift(tc.root)
			if err != nil || ok != tc.wantReq || (ok && (req.Reason != "fixed" || req.By != "town/crew/overseer" || req.At.IsZero())) {
				t.Fatalf("request = %+v present %v, err %v; want present %v, with reason and actor when present", req, ok, err, tc.wantReq)
			}
		})
	}
	// A failing writer over an open and a held town drives both print-error branches.
	for _, root := range []string{open, held} {
		if err := redHoldTestLiftRun(t, find(root), redHoldTestFailingWriter{}, "--reason", "fixed"); err == nil {
			t.Errorf("a failing writer over %s exited 0; want an error", root)
		}
	}
}
