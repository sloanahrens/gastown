package polecatimage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeferReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// sling is what pgrep answers; sandboxes what openshell prints unless
		// openshellFail is set, in which case it exits 1.
		sling         int
		sandboxes     string
		openshellFail bool
		// records are the sandbox names with a registry record under
		// SandboxRecords; directory names make a record that is not a file.
		records  []string
		dirNames []string
		want     string
	}{
		{
			name:  "a sling is running",
			sling: 0,
			want:  "sandbox sling in progress",
		},
		{
			name:  "nothing running",
			sling: 1,
			want:  "",
		},
		{
			name:      "no sandboxes found",
			sling:     1,
			sandboxes: "No sandboxes found.\n",
			want:      "",
		},
		{
			name:      "a recorded sandbox does not defer",
			sling:     1,
			sandboxes: "NAME   CREATED\n",
			records:   []string{"gt-corpus"},
			want:      "",
		},
		{
			name:      "an unrecorded sandbox defers",
			sling:     1,
			sandboxes: "NAME   CREATED\ngt-corpus 2026-10-23\n",
			want:      "sandbox gt-corpus has no registry record yet",
		},
		{
			name:      "rows before the header are not sandboxes",
			sling:     1,
			sandboxes: "gt-corpus 2026-10-23\nNAME CREATED\n",
			want:      "",
		},
		{
			name:      "blank and indented rows are skipped",
			sling:     1,
			sandboxes: "NAME CREATED\n\n   \n\tgt-corpus 2026-10-23\n",
			want:      "sandbox gt-corpus has no registry record yet",
		},
		{
			name:      "the first unrecorded sandbox is named",
			sling:     1,
			sandboxes: "NAME CREATED\ngt-one 2026-10-23\ngt-two 2026-10-24\n",
			records:   []string{"gt-one"},
			want:      "sandbox gt-two has no registry record yet",
		},
		{
			name:      "a directory named like a record is not one",
			sling:     1,
			sandboxes: "NAME CREATED\ngt-corpus 2026-10-23\n",
			dirNames:  []string{"gt-corpus"},
			want:      "sandbox gt-corpus has no registry record yet",
		},
		{
			name:  "pgrep cannot answer",
			sling: 2,
			want:  "cannot check for a running gt sling (pgrep exited 2)",
		},
		{
			name:  "pgrep is missing",
			sling: 127,
			want:  "cannot check for a running gt sling (pgrep exited 127)",
		},
		{
			name:          "openshell cannot answer",
			sling:         1,
			openshellFail: true,
			want:          "cannot list sandboxes (openshell exited 1)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			for _, name := range tc.records {
				if err := os.MkdirAll(tr.Cfg.SandboxRecords, 0o755); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(tr.Cfg.SandboxRecords, name+".json")
				if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range tc.dirNames {
				if err := os.MkdirAll(filepath.Join(tr.Cfg.SandboxRecords, name+".json"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			tr.reply = func(c command) result {
				switch c.name {
				case "pgrep":
					return result{exit: tc.sling}
				case "openshell":
					if tc.openshellFail {
						return result{exit: 1}
					}
					return result{stdout: tc.sandboxes}
				}
				return result{}
			}
			if got := tr.DeferReason(); got != tc.want {
				t.Errorf("DeferReason() = %q, want %q", got, tc.want)
			}
			if tc.sling != 1 {
				if _, ok := tr.argsOf("openshell"); ok {
					t.Error("openshell was asked after pgrep could not say nothing was running")
				}
				return
			}
			tr.wantArgs(t, "pgrep", "-f", `^([^ ]*/)?gt sling `)
			tr.wantArgs(t, "openshell", "sandbox", "list")
		})
	}
}
