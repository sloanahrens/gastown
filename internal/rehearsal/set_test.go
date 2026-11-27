package rehearsal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// beadText is a valid bead definition: a title line, a body, and an Acceptance heading.
func beadText(i int) string {
	return fmt.Sprintf("# bead %02d title\n\nbody of bead %02d\n\n## Acceptance\n\n- [ ] box %02d\n", i, i, i)
}

// writeSet writes n valid bead files named %02d.md under a fresh temp directory and returns it.
func writeSet(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	for i := 1; i <= n; i++ {
		writeFile(t, dir, fmt.Sprintf("%02d.md", i), beadText(i))
	}
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}

func TestRehearsalSetLoadsSixteenFiles(t *testing.T) {
	t.Parallel()
	dir := writeSet(t, 16)
	// A non-markdown file and a directory are not beads and are ignored.
	writeFile(t, dir, "notes.txt", "not a bead")
	if err := os.Mkdir(filepath.Join(dir, "subdir.md"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	got, err := LoadSet(dir)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	if len(got) != 16 {
		t.Fatalf("LoadSet returned %d tasks, want 16", len(got))
	}
	for i, task := range got {
		n := i + 1
		wantName := fmt.Sprintf("%02d.md", n)
		wantTitle := fmt.Sprintf("bead %02d title", n)
		wantBody := fmt.Sprintf("\nbody of bead %02d\n\n## Acceptance\n\n- [ ] box %02d\n", n, n)
		if task.Name != wantName || task.Title != wantTitle || task.Body != wantBody {
			t.Errorf("task %d = %+v, want Name %q Title %q Body %q", i, task, wantName, wantTitle, wantBody)
		}
	}
}

func TestRehearsalSetRefusals(t *testing.T) {
	t.Parallel()
	// Each row asserts the file name (or count) and the cause, so a guard that
	// refuses for the wrong reason still fails its row.
	cases := []struct {
		name    string
		edit    func(t *testing.T, dir string)
		wantErr []string
	}{
		{
			name: "fifteen files name the count",
			edit: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "16.md")); err != nil {
					t.Fatalf("Remove: %v", err)
				}
			},
			wantErr: []string{"has 15 files, want 16"},
		},
		{
			name: "seventeen files name the count",
			edit: func(t *testing.T, dir string) {
				writeFile(t, dir, "17.md", beadText(17))
			},
			wantErr: []string{"has 17 files, want 16"},
		},
		{
			name: "no title line names the file",
			edit: func(t *testing.T, dir string) {
				writeFile(t, dir, "05.md", "bead without a title\n\nbody\n\n## Acceptance\n\n- [ ] box\n")
			},
			wantErr: []string{"05.md", "first line is not"},
		},
		{
			name: "a level 3 title is not a title line",
			edit: func(t *testing.T, dir string) {
				writeFile(t, dir, "05.md", "### bead title\n\nbody\n\n## Acceptance\n\n- [ ] box\n")
			},
			wantErr: []string{"05.md", "first line is not"},
		},
		{
			name: "an empty title names the file",
			edit: func(t *testing.T, dir string) {
				writeFile(t, dir, "07.md", "#   \n\nbody\n\n## Acceptance\n\n- [ ] box\n")
			},
			wantErr: []string{"07.md", "title is empty"},
		},
		{
			name: "an empty body names the file",
			edit: func(t *testing.T, dir string) {
				writeFile(t, dir, "09.md", "# title only\n")
			},
			wantErr: []string{"09.md", "body is empty"},
		},
		{
			name: "a whitespace-only body names the file",
			edit: func(t *testing.T, dir string) {
				writeFile(t, dir, "09.md", "# title\n\n  \n\t\n")
			},
			wantErr: []string{"09.md", "body is empty"},
		},
		{
			name: "a body without an Acceptance heading names the file",
			edit: func(t *testing.T, dir string) {
				writeFile(t, dir, "11.md", "# title\n\nbody\n\n- [ ] box\n")
			},
			wantErr: []string{"11.md", "Acceptance"},
		},
		{
			name: "an Acceptance heading at level 3 does not count",
			edit: func(t *testing.T, dir string) {
				writeFile(t, dir, "11.md", "# title\n\nbody\n\n### Acceptance\n\n- [ ] box\n")
			},
			wantErr: []string{"11.md", "Acceptance"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := writeSet(t, 16)
			tc.edit(t, dir)

			_, err := LoadSet(dir)
			if err == nil {
				t.Fatalf("LoadSet accepted the set, want a refusal naming %q", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("LoadSet error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}
