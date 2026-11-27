package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// TestCallLogRunRecordsTheLaunch: each launch records one line with the status run returns.
func TestCallLogRunRecordsTheLaunch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		exit      int
		launchErr error
		want      int
	}{
		{7, nil, 7},
		{0, errors.New("gtwrap: Docker is down"), 1},
	} {
		h := newHarness(t)
		f := newFakeCallLog(0)
		h.surface.newCallLog = func(string) *callLog { return f.callLog }
		h.fake.exit, h.fake.launchErr = tc.exit, tc.launchErr
		if exit := h.run("gt", "status", "--json"); exit != tc.want {
			t.Fatalf("run = %d, want %d", exit, tc.want)
		}
		want := fmt.Sprintf("2026-11-26T20:27:03Z\tgt\tstatus\t%d\t0\t%s\n", tc.want, h.town)
		if f.file.written != want {
			t.Errorf("line = %q, want %q", f.file.written, want)
		}
	}
}

// TestCallLogRunOSSurfaceSetsNewCallLog: the real surface builds a log at its path.
func TestCallLogRunOSSurfaceSetsNewCallLog(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "calls.log")
	if got := osSurface().newCallLog(path); got == nil || got.path != path {
		t.Errorf("newCallLog(%q) built %+v, want a log at that path", path, got)
	}
}
