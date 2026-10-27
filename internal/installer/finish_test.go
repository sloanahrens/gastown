package installer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Syncs runs the two convenience syncs in the checkout. Neither can fail the
// install: a failed one is a log line and the other still runs (gt-tpg0p.11).
func TestSyncs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		respond func(dir, name string, args []string) ([]byte, []byte, int)
		lines   []string
		want    [][]string
	}{
		{
			name: "each sync's output is logged",
			respond: func(dir, name string, args []string) ([]byte, []byte, int) {
				if args[0] == "formula" {
					return []byte("synced 3 formulas\n"), nil, 0
				}
				return []byte("synced 1 plugin\n"), nil, 0
			},
			lines: []string{"[install-gt] synced 3 formulas", "[install-gt] synced 1 plugin"},
			want:  [][]string{{"GT", "formula", "sync"}, {"GT", "plugin", "sync"}},
		},
		{
			name: "a failed sync is non-fatal and the next one still runs",
			respond: func(dir, name string, args []string) ([]byte, []byte, int) {
				if args[0] == "formula" {
					return nil, []byte("town is locked\n"), 1
				}
				return []byte("synced 1 plugin\n"), nil, 0
			},
			lines: []string{
				"[install-gt] formula sync failed (non-fatal): town is locked",
				"[install-gt] synced 1 plugin",
			},
			want: [][]string{{"GT", "formula", "sync"}, {"GT", "plugin", "sync"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.respond = tc.respond

			tr.Syncs()

			if got, want := tr.lines(), tc.lines; !reflect.DeepEqual(got, want) {
				t.Errorf("stdout = %q, want %q", got, want)
			}
			if got, want := tr.cmds, substituteAll(tc.want, tr); !reflect.DeepEqual(got, want) {
				t.Errorf("commands = %q, want %q", got, want)
			}
			for _, dir := range tr.dirs {
				if dir != tr.Paths.RigDir {
					t.Errorf("ran in %q, want the checkout %q", dir, tr.Paths.RigDir)
				}
			}
		})
	}
}

// Finalize writes the restart marker, records the receipt and clears the
// fingerprints the install proves stale. A marker that cannot be written fails
// the run with the binary left installed: the marker is not the install
// (gt-tpg0p.11).
func TestFinalize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		prev  string
		setup func(tr *testRun)
		ended Exit
		lines []string
		// wantClear is the escalate-clear argv, nil when the run never got
		// there. receipt is a field the receipt line must hold.
		wantClear []string
		receipt   string
		escalated string
		severity  string
	}{
		{
			name:    "a written marker installs the commit",
			prev:    "c1111111",
			ended:   ExitOK,
			receipt: `"event": "installed"`,
			lines: []string{
				"[install-gt] In force: c1111111 -> c2222222. The daemon restarts itself when idle.",
				"install-gt: RESULT installed c2222222 c1111111 -",
			},
			wantClear: []string{"GT", "escalate", "clear",
				"--fingerprint", "install-gt:build-failed",
				"--fingerprint", "install-gt:smoke-failed",
				"--reason", "install-gt: c2222222 is in force"},
		},
		{
			name:    "a first install says the previous commit is unknown",
			ended:   ExitOK,
			receipt: `"event": "installed"`,
			lines: []string{
				"[install-gt] In force: unknown -> c2222222. The daemon restarts itself when idle.",
				"install-gt: RESULT installed c2222222 - -",
			},
			wantClear: []string{"GT", "escalate", "clear",
				"--fingerprint", "install-gt:build-failed",
				"--fingerprint", "install-gt:smoke-failed",
				"--reason", "install-gt: c2222222 is in force"},
		},
		{
			name: "a marker that cannot be written fails the run, binary and all",
			prev: "c1111111",
			setup: func(tr *testRun) {
				tr.Files.WriteFile = func(string, []byte, os.FileMode) error { return errors.New("read-only") }
			},
			ended: ExitFailed,
			lines: []string{
				"install-gt: RESULT failed c2222222 c1111111 marker-write",
			},
			receipt:   `"reason": "marker-write"`,
			escalated: "install-gt:marker-write-failed",
			severity:  "high",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.resolved(tc.prev)
			tr.C.Receipts = filepath.Join(t.TempDir(), "install-receipts.jsonl")
			if tc.setup != nil {
				tc.setup(tr)
			}

			if exit := tr.Finalize(); exit != tc.ended {
				t.Errorf("Finalize() = %d, want %d", exit, tc.ended)
			}
			wantLines := substitute(tc.lines, tr)
			if got := tr.lines(); !reflect.DeepEqual(got, wantLines) {
				t.Errorf("stdout = %q, want %q", got, wantLines)
			}
			var wantClear []string
			if tc.wantClear != nil {
				wantClear = substitute(tc.wantClear, tr)
			}
			if got := tr.command(0); !reflect.DeepEqual(got, wantClear) {
				t.Errorf("command = %q, want %q", got, wantClear)
			}
			if tc.receipt != "" {
				if got := read(t, tr.C.Receipts); !strings.Contains(got, tc.receipt) {
					t.Errorf("receipts = %q, want it to hold %q", got, tc.receipt)
				}
			}
			if tc.escalated != "" {
				if got := tr.escalation(); len(got) == 0 || got[len(got)-1] != tc.escalated {
					t.Errorf("escalation = %q, want fingerprint %q", got, tc.escalated)
				}
				marker, err := os.ReadFile(tr.Paths.Marker())
				if err == nil {
					t.Errorf("marker = %q, want none", marker)
				}
			}
		})
	}
}

// A marker that is written names the installed commit and its checkout.
func TestFinalizeWritesTheMarker(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.resolved("c1111111")
	tr.C.Receipts = filepath.Join(t.TempDir(), "install-receipts.jsonl")

	if exit := tr.Finalize(); exit != ExitOK {
		t.Fatalf("Finalize() = %d, want %d", exit, ExitOK)
	}

	marker := read(t, tr.Paths.Marker())
	for _, want := range []string{`"commit": "c2222222"`, `"source": "manual"`, `"repo": "` + tr.Paths.RigDir + `"`} {
		if !strings.Contains(marker, want) {
			t.Errorf("marker = %q, want it to hold %q", marker, want)
		}
	}
}

// The escalate clear is best effort: a clear that fails leaves the install
// standing, as the script's `|| true` does.
func TestFinalizeClearsBestEffort(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.resolved("c1111111")
	tr.C.Receipts = filepath.Join(t.TempDir(), "install-receipts.jsonl")
	tr.respond = func(string, string, []string) ([]byte, []byte, int) {
		return nil, []byte("escalate: not found\n"), 1
	}

	if exit := tr.Finalize(); exit != ExitOK {
		t.Errorf("Finalize() = %d, want %d", exit, ExitOK)
	}
	if want := "install-gt: RESULT installed c2222222 c1111111 -\n"; !strings.HasSuffix(tr.out.String(), want) {
		t.Errorf("stdout = %q, want it to end %q", tr.out.String(), want)
	}
}
