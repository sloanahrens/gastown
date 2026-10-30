package docslint

import (
	"errors"
	"io/fs"
	"reflect"
	"syscall"
	"testing"
	"testing/fstest"
)

// oneUnreadableFS is memFS with one file every read of which fails, the way a
// file the process may not read fails on disk.
type oneUnreadableFS struct {
	memFS
	unreadable string
}

func (u oneUnreadableFS) ReadFile(p string) ([]byte, error) {
	if p == u.unreadable {
		return nil, fs.ErrPermission
	}
	return u.memFS.ReadFile(p)
}

// findingStrings renders findings as the checks print them, or nil for none.
func findingStrings(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.String())
	}
	return out
}

func TestStripFencesKeepsLineNumbers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want []Line
	}{
		{
			name: "no fence keeps every line",
			text: "one\ntwo\n",
			want: []Line{{1, "one"}, {2, "two"}},
		},
		{
			name: "a link after a two-line fence reports its true line",
			text: "a\n```\nx\ny\n```\nlink\n",
			want: []Line{{1, "a"}, {6, "link"}},
		},
		{
			name: "an unclosed fence hides the rest of the file",
			text: "a\n```\nb\nc\n",
			want: []Line{{1, "a"}},
		},
		{
			name: "two fences each keep their own numbering",
			text: "```\na\n```\nb\n```\nc\n```\nd\n",
			want: []Line{{4, "b"}, {8, "d"}},
		},
		{
			name: "a fence with an info string opens and closes",
			text: "```go\nx\n```\ny\n",
			want: []Line{{4, "y"}},
		},
		{
			name: "backticks mid-line do not open a fence",
			text: "text ```\nafter\n",
			want: []Line{{1, "text ```"}, {2, "after"}},
		},
		{
			name: "empty text has no lines",
			text: "",
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StripFences(tc.text)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("StripFences(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestDeadLinksTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files memFS
		want  []string
	}{
		{
			name: "existing and missing relative targets",
			files: memFS{
				"docs/guide.md": "[ok](other.md) and [gone](missing.md)\n",
				"docs/other.md": "# other\n",
			},
			want: []string{"docs/guide.md:1: dead-link: missing.md does not exist"},
		},
		{
			name: "a fragment-only target is not judged",
			files: memFS{
				"docs/guide.md": "[top](#section)\n",
			},
		},
		{
			name: "a fragment on an existing file is dropped",
			files: memFS{
				"docs/guide.md": "[s](./other.md#section)\n",
				"docs/other.md": "# other\n",
			},
		},
		{
			name: "a fragment on a missing file names the file",
			files: memFS{
				"docs/guide.md": "[s](gone.md#section)\n",
			},
			want: []string{"docs/guide.md:1: dead-link: gone.md does not exist"},
		},
		{
			name: "a title after the path is dropped",
			files: memFS{
				"docs/guide.md": "[a](other.md \"Title\") [b](gone.md \"Title\")\n",
				"docs/other.md": "# other\n",
			},
			want: []string{"docs/guide.md:1: dead-link: gone.md does not exist"},
		},
		{
			name: "http, https, mailto and any :// are skipped",
			files: memFS{
				"docs/guide.md": "[a](https://example.com/x) [b](http://example.com) " +
					"[c](mailto:me@example.com) [d](ftp://example.com/gone.md) [e](custom://gone.md)\n",
			},
		},
		{
			name: "two links on one line each report",
			files: memFS{
				"docs/guide.md": "[a](gone1.md) [b](gone2.md)\n",
			},
			want: []string{
				"docs/guide.md:1: dead-link: gone1.md does not exist",
				"docs/guide.md:1: dead-link: gone2.md does not exist",
			},
		},
		{
			name: "a link inside a fence is ignored and later lines keep their number",
			files: memFS{
				"docs/guide.md": "```\n[x](gone.md)\n```\n[z](gone2.md)\n",
			},
			want: []string{"docs/guide.md:4: dead-link: gone2.md does not exist"},
		},
		{
			name: "a template resolves its links from the repository root",
			files: memFS{
				"internal/templates/polecat-CLAUDE.md": "[g](docs/guide.md)\n",
				"docs/guide.md":                        "# g\n",
			},
		},
		{
			name: "a template link is not resolved from its own directory",
			files: memFS{
				"internal/templates/polecat-CLAUDE.md": "[g](guide.md)\n",
				"internal/templates/guide.md":          "# g\n",
			},
			want: []string{"internal/templates/polecat-CLAUDE.md:1: dead-link: guide.md does not exist"},
		},
		{
			name: "a link in a docs file resolves from its own directory",
			files: memFS{
				"docs/sub/page.md":    "[up](../guide.md) [sib](sibling.md)\n",
				"docs/sub/sibling.md": "# sibling\n",
				"docs/guide.md":       "# g\n",
			},
		},
		{
			name: "an empty target is skipped",
			files: memFS{
				"docs/guide.md": "[x]() and [y]( \"Title\")\n",
			},
		},
		{
			name: "a target that is a directory exists",
			files: memFS{
				"docs/guide.md":    "[d](sub/)\n",
				"docs/sub/page.md": "# page\n",
			},
		},
		{
			name: "an agent-facing file is checked",
			files: memFS{
				"AGENTS.md": "[g](docs/gone.md)\n",
			},
			want: []string{"AGENTS.md:1: dead-link: docs/gone.md does not exist"},
		},
		{
			name: "the historical tier is checked",
			files: memFS{
				"docs/plans/2026-01-01-x.md": "[g](gone.md)\n",
			},
			want: []string{"docs/plans/2026-01-01-x.md:1: dead-link: gone.md does not exist"},
		},
		{
			name: "a file in no tier is not checked",
			files: memFS{
				"notes/scratch.md": "[g](gone.md)\n",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckDeadLinks(tc.files)
			if err != nil {
				t.Fatalf("CheckDeadLinks(): %v", err)
			}
			if gotStrings := findingStrings(got); !reflect.DeepEqual(gotStrings, tc.want) {
				t.Errorf("CheckDeadLinks() = %q, want %q", gotStrings, tc.want)
			}
		})
	}
}

func TestDeadLinksReadErrorIsReturned(t *testing.T) {
	t.Parallel()
	// A file the check cannot read is an error, never a file with no dead links.
	files := oneUnreadableFS{
		memFS:      memFS{"docs/guide.md": "[g](gone.md)\n"},
		unreadable: "docs/guide.md",
	}
	got, err := CheckDeadLinks(files)
	if err == nil {
		t.Fatalf("CheckDeadLinks() with an unreadable file returned no error (findings %q)", findingStrings(got))
	}
}

func TestDeadMakeTargetsTable(t *testing.T) {
	t.Parallel()
	makefile := "build:\n\tgo build ./...\nlint-tools: \n\t@true\ngate2:\n\t@true\n"
	tests := []struct {
		name  string
		files memFS
		want  []string
	}{
		{
			name: "a real target, with a digit and a hyphen in its name",
			files: memFS{
				"Makefile":  makefile,
				"AGENTS.md": "Run `make build`, `make gate2` and `make lint-tools`.\n",
			},
		},
		{
			name: "a missing target is named with its line",
			files: memFS{
				"Makefile":      makefile,
				"docs/guide.md": "# g\n\nRun `make missing`.\n",
			},
			want: []string{"docs/guide.md:3: dead-make-target: make missing is not a Makefile target"},
		},
		{
			name: "a make inside a fence is still checked",
			files: memFS{
				"Makefile":      makefile,
				"docs/guide.md": "```\n`make gone`\n```\n",
			},
			want: []string{"docs/guide.md:2: dead-make-target: make gone is not a Makefile target"},
		},
		{
			name: "a make without a backtick is not judged",
			files: memFS{
				"Makefile":  makefile,
				"AGENTS.md": "Run make missing by hand.\n",
			},
		},
		{
			name: "a name that is only the prefix of a target is missing",
			files: memFS{
				"Makefile":  "build-all:\n\t@true\n",
				"AGENTS.md": "Run `make build`.\n",
			},
			want: []string{"AGENTS.md:1: dead-make-target: make build is not a Makefile target"},
		},
		{
			name: "two references on one line each report",
			files: memFS{
				"Makefile":  makefile,
				"AGENTS.md": "`make gone1` and `make gone2`\n",
			},
			want: []string{
				"AGENTS.md:1: dead-make-target: make gone1 is not a Makefile target",
				"AGENTS.md:1: dead-make-target: make gone2 is not a Makefile target",
			},
		},
		{
			name: "a reference-tier file is checked",
			files: memFS{
				"Makefile":      makefile,
				"docs/guide.md": "`make missing`\n",
			},
			want: []string{"docs/guide.md:1: dead-make-target: make missing is not a Makefile target"},
		},
		{
			name: "a non-markdown agent-facing file is not checked",
			files: memFS{
				"Makefile": makefile,
				"internal/formula/formulas/mol-a.formula.toml": "description = \"run `make missing`\"\n",
			},
		},
		{
			name: "no Makefile means no findings",
			files: memFS{
				"AGENTS.md": "Run `make missing`.\n",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckDeadMakeTargets(tc.files)
			if err != nil {
				t.Fatalf("CheckDeadMakeTargets(): %v", err)
			}
			if gotStrings := findingStrings(got); !reflect.DeepEqual(gotStrings, tc.want) {
				t.Errorf("CheckDeadMakeTargets() = %q, want %q", gotStrings, tc.want)
			}
		})
	}
}

func TestDeadMakeTargetsReadErrorIsReturned(t *testing.T) {
	t.Parallel()
	// A Makefile that cannot be read is an error: judging every reference
	// against an empty target list would report every one of them dead.
	files := oneUnreadableFS{
		memFS:      memFS{"Makefile": "build:\n", "AGENTS.md": "`make build`\n"},
		unreadable: "Makefile",
	}
	if _, err := CheckDeadMakeTargets(files); err == nil {
		t.Fatal("CheckDeadMakeTargets() with an unreadable Makefile returned no error")
	}
}

func TestDeadLinksWalkErrorIsReturned(t *testing.T) {
	t.Parallel()
	// A tier that cannot be listed stops the check: a shorter list would report
	// a clean tree the check never finished reading.
	files := memFS{
		"AGENTS.md":           "# agents\n",
		"plugins/p/plugin.md": "# plugin\n",
		"docs/guide.md":       "# guide\n",
		"docs/plans/p.md":     "# plan\n",
	}
	tests := []struct {
		name    string
		failDir string
	}{
		{"the agent-facing tier", "plugins"},
		{"the reference tier", "docs"},
		{"the historical tier", "docs/plans"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := CheckDeadLinks(walkErrorFS{memFS: files, failDir: tc.failDir})
			if err == nil {
				t.Fatalf("CheckDeadLinks() with %s unlistable returned no error (findings %q)", tc.name, findingStrings(got))
			}
		})
	}
}

func TestDeadMakeTargetsWalkErrorIsReturned(t *testing.T) {
	t.Parallel()
	files := memFS{
		"Makefile":            "build:\n",
		"AGENTS.md":           "`make build`\n",
		"plugins/p/plugin.md": "# plugin\n",
		"docs/guide.md":       "# guide\n",
	}
	tests := []struct {
		name    string
		failDir string
	}{
		{"the agent-facing tier", "plugins"},
		{"the reference tier", "docs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := CheckDeadMakeTargets(walkErrorFS{memFS: files, failDir: tc.failDir})
			if err == nil {
				t.Fatalf("CheckDeadMakeTargets() with %s unlistable returned no error (findings %q)", tc.name, findingStrings(got))
			}
		})
	}
}

func TestDeadMakeTargetsDocReadErrorIsReturned(t *testing.T) {
	t.Parallel()
	// A document that cannot be read is an error, never a document with no
	// missing targets.
	files := oneUnreadableFS{
		memFS:      memFS{"Makefile": "build:\n", "AGENTS.md": "`make gone`\n"},
		unreadable: "AGENTS.md",
	}
	if _, err := CheckDeadMakeTargets(files); err == nil {
		t.Fatal("CheckDeadMakeTargets() with an unreadable document returned no error")
	}
}

// probeFS is memFS that records each path the dead-link check probes, so a test
// can show a target was never handed to the filesystem.
type probeFS struct {
	memFS
	probed []string
}

func (p *probeFS) PathExists(name string) (bool, error) {
	p.probed = append(p.probed, name)
	return p.memFS.PathExists(name)
}

// statErrFS is memFS whose stat of one path fails with err, the way a path the
// process may not stat fails on disk.
type statErrFS struct {
	memFS
	path string
	err  error
}

func (s statErrFS) FileExists(p string) (bool, error) {
	if p == s.path {
		return false, s.err
	}
	return s.memFS.FileExists(p)
}

func (s statErrFS) PathExists(p string) (bool, error) {
	if p == s.path {
		return false, s.err
	}
	return s.memFS.PathExists(p)
}

func TestDeadLinksConfinedTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		files      memFS
		want       []string
		wantProbed []string
	}{
		{
			name:  "a target that climbs above the root from the repository root is not probed",
			files: memFS{"AGENTS.md": "[x](../x)\n"},
			want:  []string{"AGENTS.md:1: dead-link: ../x points outside the repository"},
		},
		{
			name:  "a target that climbs above the root from a nested doc is not probed",
			files: memFS{"docs/guide.md": "[x](../../etc/passwd)\n"},
			want:  []string{"docs/guide.md:1: dead-link: ../../etc/passwd points outside the repository"},
		},
		{
			name:  "a target that climbs out after a directory is not probed",
			files: memFS{"AGENTS.md": "[x](docs/../../x)\n"},
			want:  []string{"AGENTS.md:1: dead-link: docs/../../x points outside the repository"},
		},
		{
			name:  "an absolute target is not joined onto its base or probed",
			files: memFS{"docs/guide.md": "[x](/etc/hosts)\n"},
			want:  []string{"docs/guide.md:1: dead-link: /etc/hosts points outside the repository"},
		},
		{
			name:  "a template climbing above the root is not probed",
			files: memFS{"internal/templates/polecat-CLAUDE.md": "[x](../../x)\n"},
			want:  []string{"internal/templates/polecat-CLAUDE.md:1: dead-link: ../../x points outside the repository"},
		},
		{
			name:       "a target that climbs and returns inside the repository is still probed",
			files:      memFS{"AGENTS.md": "[r](docs/../README.md)\n", "README.md": "# r\n"},
			wantProbed: []string{"README.md"},
		},
		{
			name:       "a parent-directory target that stays inside is still probed",
			files:      memFS{"docs/guide.md": "[x](../x.md)\n", "x.md": "# x\n"},
			wantProbed: []string{"x.md"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := &probeFS{memFS: tc.files}
			got, err := CheckDeadLinks(files)
			if err != nil {
				t.Fatalf("CheckDeadLinks(): %v", err)
			}
			if gotStrings := findingStrings(got); !reflect.DeepEqual(gotStrings, tc.want) {
				t.Errorf("CheckDeadLinks() = %q, want %q", gotStrings, tc.want)
			}
			if !reflect.DeepEqual(files.probed, tc.wantProbed) {
				t.Errorf("PathExists was called for %q, want %q", files.probed, tc.wantProbed)
			}
		})
	}
}

func TestDeadLinksProbeErrorIsReturned(t *testing.T) {
	t.Parallel()
	// A target that cannot be stat'ed is neither a live link nor a dead one, so
	// the check fails rather than reporting "does not exist".
	files := statErrFS{
		memFS: memFS{"docs/guide.md": "[g](gone.md)\n"},
		path:  "docs/gone.md",
		err:   fs.ErrPermission,
	}
	got, err := CheckDeadLinks(files)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("CheckDeadLinks() error = %v, want the stat's %v", err, fs.ErrPermission)
	}
	if got != nil {
		t.Errorf("CheckDeadLinks() findings = %q alongside the error, want none", findingStrings(got))
	}
}

func TestDeadMakeTargetsStatError(t *testing.T) {
	t.Parallel()
	// A Makefile that cannot be stat'ed is not a Makefile that is absent. Taking
	// it for absent would pass every reference unjudged.
	files := statErrFS{
		memFS: memFS{"Makefile": "build:\n", "AGENTS.md": "`make build`\n"},
		path:  "Makefile",
		err:   fs.ErrPermission,
	}
	got, err := CheckDeadMakeTargets(files)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("CheckDeadMakeTargets() error = %v, want the stat's %v", err, fs.ErrPermission)
	}
	if got != nil {
		t.Errorf("CheckDeadMakeTargets() findings = %q alongside the error, want none", findingStrings(got))
	}
}

// TestDeadLinksStatMapping drives the stat results OSFiles turns into the
// (bool, error) answers both checks rely on. The stat is a seam over an
// in-memory tree, so no real path is touched.
func TestDeadLinksStatMapping(t *testing.T) {
	t.Parallel()
	tree := fstest.MapFS{"file.md": {}, "dir/x.md": {}}
	denied := func(name string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrPermission}
	}
	notDir := func(name string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: syscall.ENOTDIR}
	}
	tests := []struct {
		name     string
		path     string
		stat     func(string) (fs.FileInfo, error)
		wantFile bool
		wantPath bool
		wantErr  bool
	}{
		{name: "a regular file", path: "file.md", stat: tree.Stat, wantFile: true, wantPath: true},
		{name: "a directory exists but is not a file", path: "dir", stat: tree.Stat, wantPath: true},
		{name: "an absent path", path: "gone.md", stat: tree.Stat},
		{name: "a path under a file is absent", path: "file.md/x", stat: notDir},
		{name: "a permission failure is an error, not absence", path: "gone.md", stat: denied, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := OSFiles{stat: tc.stat}
			file, fileErr := fsys.FileExists(tc.path)
			if file != tc.wantFile || (fileErr != nil) != tc.wantErr {
				t.Errorf("FileExists(%s) = %v, %v; want %v, error %v", tc.path, file, fileErr, tc.wantFile, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(fileErr, fs.ErrPermission) {
				t.Errorf("FileExists(%s) error = %v, want the stat's %v", tc.path, fileErr, fs.ErrPermission)
			}
			found, pathErr := fsys.PathExists(tc.path)
			if found != tc.wantPath || (pathErr != nil) != tc.wantErr {
				t.Errorf("PathExists(%s) = %v, %v; want %v, error %v", tc.path, found, pathErr, tc.wantPath, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(pathErr, fs.ErrPermission) {
				t.Errorf("PathExists(%s) error = %v, want the stat's %v", tc.path, pathErr, fs.ErrPermission)
			}
		})
	}
}
