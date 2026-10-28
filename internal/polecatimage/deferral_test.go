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
		// sandboxes is what openshell prints unless openshellFail is set, in
		// which case it exits 1.
		sandboxes     string
		openshellFail bool
		// records are the sandbox names with a registry record under
		// SandboxRecords; directory names make a record that is not a file.
		records  []string
		dirNames []string
		want     string
	}{
		{
			name: "an empty listing cannot be read",
			want: "cannot read the sandbox listing (no NAME header)",
		},
		{
			name:      "no sandboxes found",
			sandboxes: "No sandboxes found.\n",
			want:      "",
		},
		{
			name:      "no sandboxes found with padding",
			sandboxes: "  No sandboxes found.  \n",
			want:      "",
		},
		{
			name:      "a listing with no header cannot be read",
			sandboxes: "gt-corpus 2026-10-23\n",
			want:      "cannot read the sandbox listing (no NAME header)",
		},
		{
			name:      "a name that escapes the record directory cannot be read",
			sandboxes: "NAME CREATED\n../../etc/passwd 2026-10-23\n",
			want:      "cannot read the sandbox listing (a sandbox name is not a plain file name)",
		},
		{
			name:      "a dot-dot name cannot be read",
			sandboxes: "NAME CREATED\n.. 2026-10-23\n",
			want:      "cannot read the sandbox listing (a sandbox name is not a plain file name)",
		},
		{
			name:      "a recorded sandbox does not defer",
			sandboxes: "NAME   CREATED\n",
			records:   []string{"gt-corpus"},
			want:      "",
		},
		{
			name:      "an unrecorded sandbox defers",
			sandboxes: "NAME   CREATED\ngt-corpus 2026-10-23\n",
			want:      "sandbox gt-corpus has no registry record yet",
		},
		{
			name:      "rows before the header are not sandboxes",
			sandboxes: "gt-corpus 2026-10-23\nNAME CREATED\n",
			want:      "",
		},
		{
			name:      "blank and indented rows are skipped",
			sandboxes: "NAME CREATED\n\n   \n\tgt-corpus 2026-10-23\n",
			want:      "sandbox gt-corpus has no registry record yet",
		},
		{
			name:      "the first unrecorded sandbox is named",
			sandboxes: "NAME CREATED\ngt-one 2026-10-23\ngt-two 2026-10-24\n",
			records:   []string{"gt-one"},
			want:      "sandbox gt-two has no registry record yet",
		},
		{
			name:      "a directory named like a record is not one",
			sandboxes: "NAME CREATED\ngt-corpus 2026-10-23\n",
			dirNames:  []string{"gt-corpus"},
			want:      "sandbox gt-corpus has no registry record yet",
		},
		{
			name:          "openshell cannot answer",
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
				if c.name == "openshell" {
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
			tr.wantArgs(t, "openshell", "sandbox", "list")
		})
	}
}
