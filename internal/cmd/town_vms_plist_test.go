package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestTownVMsPlistGolden pins the com.gastown.colima-vms job to the golden: a
// thin wrapper that runs gt town vms start at load, with the Mac's PATH and
// HOME, RunAtLoad true and KeepAlive false.
func TestTownVMsPlistGolden(t *testing.T) {
	t.Parallel()
	got, err := renderTownVMsPlist("/Users/x/.local/bin/gt", "/Users/x")
	if err != nil {
		t.Fatalf("rendering the com.gastown.colima-vms plist: %v", err)
	}
	if err := compareGolden("testdata/town_vms_plist.golden", string(got)); err != nil {
		t.Error(err)
	}
}

// TestTownVMsPlistEscapesValues checks that a path holding an XML special
// character reaches the plist escaped, so it cannot end a string node early.
func TestTownVMsPlistEscapesValues(t *testing.T) {
	t.Parallel()
	got, err := renderTownVMsPlist("/Users/a&b/.local/bin/gt", "/Users/a&b")
	if err != nil {
		t.Fatalf("rendering the plist: %v", err)
	}
	doc := string(got)
	if strings.Contains(doc, "a&b") {
		t.Errorf("the plist holds an unescaped home:\n%s", doc)
	}
	const wantLog = "<string>/Users/a&amp;b/gt/daemon/colima-vms.log</string>"
	if !strings.Contains(doc, wantLog) {
		t.Errorf("the plist lacks %s:\n%s", wantLog, doc)
	}
}

// TestTownVMsPlistWriteFailure checks that a failed write is an error, so a
// partial plist is never handed out as a rendered one.
func TestTownVMsPlistWriteFailure(t *testing.T) {
	t.Parallel()
	err := writeTownVMsPlist(forwardPlistFailingWriter{}, "/Users/x/.local/bin/gt", "/Users/x")
	if err == nil {
		t.Fatal("a failed write of the plist was reported as success")
	}
}

// TestTownVMsPlistCommandPrints: `gt town vms plist --gt <gt> --home <home>` prints
// exactly what renderTownVMsPlist renders for those arguments, and only prints: it
// takes no positional argument and is registered under `gt town vms` (gt-097npg).
func TestTownVMsPlistCommandPrints(t *testing.T) {
	t.Parallel()
	const gt, home = "/Users/x/.local/bin/gt", "/Users/x"
	want, err := renderTownVMsPlist(gt, home)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newTownVMsPlistCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--gt", gt, "--home", home})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("plist: %v", err)
	}
	if out.String() != string(want) {
		t.Errorf("stdout = %q, want renderTownVMsPlist's %q", out.String(), want)
	}
	registered := false
	for _, c := range townVmsCmd.Commands() {
		registered = registered || c.Name() == "plist"
	}
	if !registered {
		t.Error("`plist` is not registered under `gt town vms`")
	}
}

// TestTownVMsPlistCommandRefusals: both flags are required and no positional
// argument is taken, each refused with nothing printed; a stdout that fails to take
// the plist is the command's error (gt-097npg).
func TestTownVMsPlistCommandRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no --gt", []string{"--home", "/Users/x"}, "gt"},
		{"no --home", []string{"--gt", "/Users/x/.local/bin/gt"}, "home"},
		{"a positional argument", []string{"--gt", "/Users/x/.local/bin/gt", "--home", "/Users/x", "extra"}, "unknown command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := newTownVMsPlistCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want nothing printed on a refusal", out.String())
			}
		})
	}
	t.Run("a stdout that fails", func(t *testing.T) {
		t.Parallel()
		cmd := newTownVMsPlistCmd()
		cmd.SetOut(failingWriter{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--gt", "/Users/x/.local/bin/gt", "--home", "/Users/x"})
		if err := cmd.Execute(); err == nil {
			t.Error("a failed write of the plist to stdout was reported as success")
		}
	})
}
