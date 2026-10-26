package installer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	full         = "c2222222"
	installed    = "c1111111"
	daemonBehind = "d1111111"
	pendingAhead = "e2222222"
	pendingOld   = "e0000000"
)

// Noop follows the script's no-op section: the installed binary already holds
// the target, so only the daemon's own build decides whether anything happens.
func TestNoop(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		setup     func(*testRun)
		ended     bool
		exit      Exit
		lines     []string
		marker    string // the marker the run leaves, "" for none
		receipt   string // the reason the receipt names, "" for none
		escalated bool
	}{
		{
			name:  "nothing installed is not a no-op",
			setup: func(tr *testRun) { tr.Target = Target{FullSHA: full} },
		},
		{
			name: "an installed binary without the target is not a no-op",
			setup: func(tr *testRun) {
				tr.Target = Target{FullSHA: full, Prev: installed}
				tr.Repo.IsAncestor = ancestors()
			},
		},
		{
			name: "a daemon that already runs the target does nothing",
			setup: func(tr *testRun) {
				tr.Target = Target{FullSHA: full, Prev: installed}
				tr.Repo.IsAncestor = ancestors([2]string{full, installed}, [2]string{full, pendingAhead})
				tr.write(t, "state.json", `{"commit": "`+pendingAhead+`"}`)
			},
			ended: true,
			exit:  ExitOK,
			lines: []string{
				"[install-gt] Installed " + installed + " already contains " + full + "; nothing to do.",
				"install-gt: RESULT noop " + full + " " + installed + " already-installed",
			},
		},
		{
			name: "no daemon state does nothing",
			setup: func(tr *testRun) {
				tr.Target = Target{FullSHA: full, Prev: installed}
				tr.Repo.IsAncestor = ancestors([2]string{full, installed})
			},
			ended: true,
			exit:  ExitOK,
			lines: []string{
				"[install-gt] Installed " + installed + " already contains " + full + "; nothing to do.",
				"install-gt: RESULT noop " + full + " " + installed + " already-installed",
			},
		},
		{
			name: "a daemon behind the target is told to restart",
			setup: func(tr *testRun) {
				tr.Target = Target{FullSHA: full, Prev: installed}
				tr.Repo.IsAncestor = ancestors([2]string{full, installed})
				tr.write(t, "state.json", `{"commit": "`+daemonBehind+`"}`)
			},
			ended: true,
			exit:  ExitOK,
			lines: []string{
				"[install-gt] Installed " + installed + " already contains " + full + ", but the daemon is running " + daemonBehind + "; writing the restart marker.",
				"install-gt: RESULT noop " + full + " " + installed + " already-installed",
			},
			marker: `{
  "commit": "` + full + `",
  "repo": "RIG",
  "requested_at": "2026-10-26T19:20:36Z",
  "source": "manual"
}`,
		},
		{
			name: "a pending marker that already contains the target is left alone",
			setup: func(tr *testRun) {
				tr.Target = Target{FullSHA: full, Prev: installed}
				tr.Repo.IsAncestor = ancestors([2]string{full, installed}, [2]string{full, pendingAhead})
				tr.write(t, "state.json", `{"commit": "`+daemonBehind+`"}`)
				tr.write(t, "restart-pending.json", `{"commit": "`+pendingAhead+`"}`)
			},
			ended: true,
			exit:  ExitOK,
			lines: []string{
				"[install-gt] A pending restart marker for " + pendingAhead + " already contains " + full + "; leaving it in place.",
				"install-gt: RESULT noop " + full + " " + installed + " already-installed",
			},
			marker: `{"commit": "` + pendingAhead + `"}`,
		},
		{
			name: "a pending marker behind the target is replaced",
			setup: func(tr *testRun) {
				tr.Target = Target{FullSHA: full, Prev: installed}
				tr.Repo.IsAncestor = ancestors([2]string{full, installed})
				tr.write(t, "state.json", `{"commit": "`+daemonBehind+`"}`)
				tr.write(t, "restart-pending.json", `{"commit": "`+pendingOld+`"}`)
			},
			ended: true,
			exit:  ExitOK,
			lines: []string{
				"[install-gt] Installed " + installed + " already contains " + full + ", but the daemon is running " + daemonBehind + "; writing the restart marker.",
				"install-gt: RESULT noop " + full + " " + installed + " already-installed",
			},
			marker: `{
  "commit": "` + full + `",
  "repo": "RIG",
  "requested_at": "2026-10-26T19:20:36Z",
  "source": "manual"
}`,
		},
		{
			name: "a daemon commit the checkout lacks installs rather than reporting a no-op",
			setup: func(tr *testRun) {
				tr.Target = Target{FullSHA: full, Prev: installed}
				tr.Repo.IsAncestor = ancestors([2]string{full, installed})
				tr.Repo.Resolve = func(ref string) string {
					if ref == "deadbeef" {
						return ""
					}
					return ref
				}
				tr.write(t, "state.json", `{"commit": "deadbeef"}`)
			},
			lines: []string{
				"[install-gt] WARNING: the daemon's commit deadbeef is not in RIG; installing rather than leaving the daemon on an unknown build.",
			},
		},
		{
			name: "unreadable daemon state does nothing",
			setup: func(tr *testRun) {
				tr.Target = Target{FullSHA: full, Prev: installed}
				tr.Repo.IsAncestor = ancestors([2]string{full, installed})
				tr.write(t, "state.json", "not json")
			},
			ended: true,
			exit:  ExitOK,
			lines: []string{
				"[install-gt] Installed " + installed + " already contains " + full + "; nothing to do.",
				"install-gt: RESULT noop " + full + " " + installed + " already-installed",
			},
		},
		{
			name: "a marker that cannot be written fails the run",
			setup: func(tr *testRun) {
				tr.Target = Target{FullSHA: full, Prev: installed}
				tr.Repo.IsAncestor = ancestors([2]string{full, installed})
				tr.write(t, "state.json", `{"commit": "`+daemonBehind+`"}`)
				tr.Files.WriteFile = func(string, []byte, os.FileMode) error { return errors.New("read-only") }
			},
			ended: true,
			exit:  ExitFailed,
			lines: []string{
				"[install-gt] Installed " + installed + " already contains " + full + ", but the daemon is running " + daemonBehind + "; writing the restart marker.",
				"install-gt: RESULT failed " + full + " " + installed + " marker-write",
			},
			receipt:   "marker-write",
			escalated: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.C.Receipts = filepath.Join(t.TempDir(), "install-receipts.jsonl")
			if tc.setup != nil {
				tc.setup(tr)
			}

			exit, ended := tr.Noop()

			if ended != tc.ended || exit != tc.exit {
				t.Errorf("Noop() = (%d, %v), want (%d, %v)", exit, ended, tc.exit, tc.ended)
			}
			var wantLines []string
			for _, line := range tc.lines {
				wantLines = append(wantLines, strings.ReplaceAll(line, "RIG", tr.Paths.RigDir))
			}
			if got := tr.lines(); !reflect.DeepEqual(got, wantLines) {
				t.Errorf("stdout = %q, want %q", got, wantLines)
			}
			marker, err := os.ReadFile(tr.Paths.Marker())
			switch {
			case tc.marker == "" && err == nil:
				t.Errorf("marker = %q, want none", marker)
			case tc.marker != "" && err != nil:
				t.Errorf("marker missing: %v", err)
			case tc.marker != "" && string(marker) != strings.ReplaceAll(tc.marker, "RIG", tr.Paths.RigDir):
				t.Errorf("marker = %q, want %q", marker, strings.ReplaceAll(tc.marker, "RIG", tr.Paths.RigDir))
			}
			if tc.receipt != "" {
				receipts, err := os.ReadFile(tr.C.Receipts)
				if err != nil {
					t.Fatalf("receipts: %v", err)
				}
				if !strings.Contains(string(receipts), `"reason": "`+tc.receipt+`"`) {
					t.Errorf("receipts = %q, want reason %q", receipts, tc.receipt)
				}
			}
			if !tc.escalated {
				if got := tr.escalation(); got != nil {
					t.Errorf("escalated with %q, want nothing", got)
				}
				return
			}
			escalation := tr.escalation()
			if want := []string{tr.C.GT, "escalate", "install-gt: " + full + " is installed at " + tr.Paths.GT() +
				" and the running daemon is still on " + daemonBehind + ", but writing " + tr.Paths.Marker() +
				" failed — the binary stays installed; the daemon needs a manual restart to pick it up",
				"-s", "high", "--source", "script:install-gt", "--fingerprint", "install-gt:marker-write-failed"}; !reflect.DeepEqual(escalation, want) {
				t.Errorf("escalation = %q, want %q", escalation, want)
			}
		})
	}
}

// WriteRestartMarker writes the marker the daemon restarts into, byte for byte
// what the script's python writes.
func TestWriteRestartMarker(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)

	if err := tr.WriteRestartMarker(full); err != nil {
		t.Fatalf("WriteRestartMarker: %v", err)
	}

	got, err := os.ReadFile(tr.Paths.Marker())
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	want := "{\n  \"commit\": \"" + full + "\",\n  \"repo\": \"" + tr.Paths.RigDir +
		"\",\n  \"requested_at\": \"2026-10-26T19:20:36Z\",\n  \"source\": \"manual\"\n}"
	if string(got) != want {
		t.Errorf("marker = %q, want %q", got, want)
	}
}

// A marker the rename could not put in place leaves no temporary file behind.
func TestWriteRestartMarkerRenameFails(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Files.Rename = func(string, string) error { return errors.New("read-only") }

	if err := tr.WriteRestartMarker(full); err == nil {
		t.Fatal("WriteRestartMarker succeeded, want an error")
	}

	entries, err := os.ReadDir(tr.Paths.DaemonDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp.") {
			t.Errorf("left %q behind", entry.Name())
		}
	}
}
