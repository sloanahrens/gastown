package cmd

import (
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
