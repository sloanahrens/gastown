package docslint

import (
	"reflect"
	"testing"
)

func TestStatusChecks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files memFS
		want  []string
	}{
		{
			name: "a plan without the header is a finding",
			files: memFS{
				"docs/plans/p.md": "# Plan\n\nBody.\n",
			},
			want: []string{`docs/plans/p.md:1: status-header: first non-blank line must start with "> Status:"`},
		},
		{
			name: "blank lines before the header are skipped",
			files: memFS{
				"docs/plans/p.md": "\n  \n> Status: Live\n\n# Plan\n",
			},
		},
		{
			name: "a research file without the header is a finding",
			files: memFS{
				"docs/research/r.md": "# Research\n",
			},
			want: []string{`docs/research/r.md:1: status-header: first non-blank line must start with "> Status:"`},
		},
		{
			name: "a research file with the header is clean",
			files: memFS{
				"docs/research/r.md": "> Status: Historical\n",
			},
		},
		{
			name: "an indented header is not the header",
			files: memFS{
				"docs/plans/p.md": "  > Status: Live\n",
			},
			want: []string{`docs/plans/p.md:1: status-header: first non-blank line must start with "> Status:"`},
		},
		{
			name: "a fenced status line does not satisfy the header rule",
			files: memFS{
				"docs/plans/p.md": "```\n> Status: Live\n```\n",
			},
			want: []string{`docs/plans/p.md:1: status-header: first non-blank line must start with "> Status:"`},
		},
		{
			name: "a file with two status lines is a conflict",
			files: memFS{
				"docs/plans/p.md": "> Status: Live\n\nbody\n\n> Status: Historical\n",
			},
			want: []string{`docs/plans/p.md:1: status-conflict: more than one "> Status:" line`},
		},
		{
			name: "a conflict in a reference doc is a finding",
			files: memFS{
				"docs/guide.md": "> Status: Live\n> Status: Retired\n",
			},
			want: []string{`docs/guide.md:1: status-conflict: more than one "> Status:" line`},
		},
		{
			name: "a status line inside a fence is not counted",
			files: memFS{
				"docs/guide.md": "> Status: Live\n\n```\n> Status: example\n```\n",
			},
		},
		{
			name: "one real status line beside a fenced one is not a conflict",
			files: memFS{
				"docs/guide.md": "```\n> Status: example\n```\n> Status: Live\n",
			},
		},
		{
			name: "a non-markdown file under a plan directory is not checked",
			files: memFS{
				"docs/plans/notes.txt": "no header\n",
			},
		},
		{
			name: "a conflict outside the doc tiers is not checked",
			files: memFS{
				"scratch/notes.md": "> Status: A\n> Status: B\n",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers, err := CheckStatusHeaders(tc.files)
			if err != nil {
				t.Fatalf("CheckStatusHeaders(): %v", err)
			}
			conflicts, err := CheckStatusConflict(tc.files)
			if err != nil {
				t.Fatalf("CheckStatusConflict(): %v", err)
			}
			got := append(findingStrings(headers), findingStrings(conflicts)...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("status checks = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStatusReadErrorsAreReturned(t *testing.T) {
	t.Parallel()
	files := memFS{
		"docs/plans/p.md": "> Status: Live\n",
		"docs/guide.md":   "> Status: Live\n",
	}
	unreadable := oneUnreadableFS{memFS: files, unreadable: "docs/plans/p.md"}
	if _, err := CheckStatusHeaders(unreadable); err == nil {
		t.Error("CheckStatusHeaders() with an unreadable plan returned no error")
	}
	if _, err := CheckStatusConflict(unreadable); err == nil {
		t.Error("CheckStatusConflict() with an unreadable plan returned no error")
	}
}

func TestStrayMarkupTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files memFS
		want  []string
	}{
		{
			name: "each of the six patterns is flagged on its line",
			files: memFS{
				"AGENTS.md": "<tool_call>\n</tool_call>\n<function=run>\n<parameter=cmd>\n<<<<<<< HEAD\n>>>>>>> main\n",
			},
			want: []string{
				"AGENTS.md:1: stray-markup: committed tool-call or merge-conflict markup",
				"AGENTS.md:2: stray-markup: committed tool-call or merge-conflict markup",
				"AGENTS.md:3: stray-markup: committed tool-call or merge-conflict markup",
				"AGENTS.md:4: stray-markup: committed tool-call or merge-conflict markup",
				"AGENTS.md:5: stray-markup: committed tool-call or merge-conflict markup",
				"AGENTS.md:6: stray-markup: committed tool-call or merge-conflict markup",
			},
		},
		{
			name: "a line that mentions tool_call without the angle brackets is clean",
			files: memFS{
				"docs/guide.md": "Set the tool_call field.\nUse tool_call> here.\n",
			},
		},
		{
			name: "a conflict marker not at the line start is clean",
			files: memFS{
				"docs/guide.md": "text <<<<<<< HEAD\n",
			},
		},
		{
			name: "a fenced tool call is still flagged",
			files: memFS{
				"docs/guide.md": "```\n<tool_call>\n```\n",
			},
			want: []string{"docs/guide.md:2: stray-markup: committed tool-call or merge-conflict markup"},
		},
		{
			name: "a go file is scanned",
			files: memFS{
				"internal/x/x.go": "package x\n\nvar s = \"<parameter=a>\"\n",
			},
			want: []string{"internal/x/x.go:3: stray-markup: committed tool-call or merge-conflict markup"},
		},
		{
			name: "a go test file is not scanned",
			files: memFS{
				"internal/x/x_test.go": "package x\n\nvar s = \"<parameter=a>\"\n",
			},
		},
		{
			name: "a toml formula in the agent-facing tier is scanned",
			files: memFS{
				"internal/formula/formulas/mol-a.formula.toml": "description = \"<tool_call>\"\n",
			},
			want: []string{"internal/formula/formulas/mol-a.formula.toml:1: stray-markup: committed tool-call or merge-conflict markup"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckStrayMarkup(tc.files)
			if err != nil {
				t.Fatalf("CheckStrayMarkup(): %v", err)
			}
			if gotStrings := findingStrings(got); !reflect.DeepEqual(gotStrings, tc.want) {
				t.Errorf("CheckStrayMarkup() = %q, want %q", gotStrings, tc.want)
			}
		})
	}
}

func TestStrayMarkupReadErrorIsReturned(t *testing.T) {
	t.Parallel()
	files := memFS{"AGENTS.md": "fine\n"}
	if _, err := CheckStrayMarkup(oneUnreadableFS{memFS: files, unreadable: "AGENTS.md"}); err == nil {
		t.Error("CheckStrayMarkup() with an unreadable file returned no error")
	}
}

func TestPolecatMainPushTable(t *testing.T) {
	t.Parallel()
	const sling = "gt sling rig/polecats/p1 --formula mol-release\n"
	tests := []struct {
		name string
		push string
		want bool
	}{
		{name: "push of HEAD to main", push: "git push origin HEAD:main", want: true},
		{name: "push of main", push: "git push origin main", want: true},
		{name: "push of master", push: "git push origin master", want: true},
		{name: "push with -C and a delete refspec", push: "git -C dir push origin :main", want: true},
		{name: "push with --force to a full ref", push: "git push --force origin refs/heads/main", want: true},
		{name: "push of a local main as a source is not a push to main", push: "git push origin main:polecat/x", want: false},
		{name: "push of a feature branch", push: "git push origin feature", want: false},
		{name: "no push line", push: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := sling
			if tc.push != "" {
				content = tc.push + "\n" + sling
			}
			files := memFS{"internal/formula/formulas/mol-a.formula.toml": content}
			got, err := CheckPolecatMainPush(files)
			if err != nil {
				t.Fatalf("CheckPolecatMainPush(): %v", err)
			}
			want := []string(nil)
			if tc.want {
				want = []string{"internal/formula/formulas/mol-a.formula.toml:2: polecat-main-push: pushes main/master and offers a polecat; the guards refuse a polecat session (gt-ibt8)"}
			}
			if gotStrings := findingStrings(got); !reflect.DeepEqual(gotStrings, want) {
				t.Errorf("CheckPolecatMainPush() = %q, want %q", gotStrings, want)
			}
		})
	}
}

func TestPolecatMainPushFlagsOnlyPolecatLines(t *testing.T) {
	t.Parallel()
	files := memFS{
		"internal/formula/formulas/mol-a.formula.toml": "git push origin HEAD:main\ngt sling mayor/x\ngt sling rig/polecats/p1 --formula mol-release\n",
	}
	got, err := CheckPolecatMainPush(files)
	if err != nil {
		t.Fatalf("CheckPolecatMainPush(): %v", err)
	}
	want := []string{"internal/formula/formulas/mol-a.formula.toml:3: polecat-main-push: pushes main/master and offers a polecat; the guards refuse a polecat session (gt-ibt8)"}
	if gotStrings := findingStrings(got); !reflect.DeepEqual(gotStrings, want) {
		t.Errorf("CheckPolecatMainPush() = %q, want %q", gotStrings, want)
	}
}

func TestPolecatMainPushExemptsPlugins(t *testing.T) {
	t.Parallel()
	files := memFS{
		"plugins/merge/plugin.md": "git push origin main\ngt sling rig/polecats/p1 --formula mol-release\n",
	}
	got, err := CheckPolecatMainPush(files)
	if err != nil {
		t.Fatalf("CheckPolecatMainPush(): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("CheckPolecatMainPush() on a plugin = %q, want none", findingStrings(got))
	}
}

func TestPolecatMainPushReadErrorIsReturned(t *testing.T) {
	t.Parallel()
	files := memFS{"internal/formula/formulas/mol-a.formula.toml": "git push origin main\n"}
	unreadable := oneUnreadableFS{memFS: files, unreadable: "internal/formula/formulas/mol-a.formula.toml"}
	if _, err := CheckPolecatMainPush(unreadable); err == nil {
		t.Error("CheckPolecatMainPush() with an unreadable formula returned no error")
	}
}

// Each check reads the tree through several walks. A walk that fails must come
// back as an error, never as a shorter list that reads as a clean tree.
func TestMarkupAndCeilingWalkErrorsAreReturned(t *testing.T) {
	t.Parallel()
	files := memFS{
		"AGENTS.md":       "ok\n",
		"docs/guide.md":   "ok\n",
		"internal/x/x.go": "package x\n",
	}
	statusHeaders := func(f FS) error { _, err := CheckStatusHeaders(f); return err }
	statusConflict := func(f FS) error { _, err := CheckStatusConflict(f); return err }
	strayMarkup := func(f FS) error { _, err := CheckStrayMarkup(f); return err }
	polecatPush := func(f FS) error { _, err := CheckPolecatMainPush(f); return err }
	wordCeiling := func(f FS) error { _, err := CheckWordCeiling(f, 1500); return err }
	tests := []struct {
		name    string
		failDir string
		check   func(FS) error
	}{
		{"status headers walk the plans", "docs/plans", statusHeaders},
		{"status conflict walks the agent-facing tier", "plugins", statusConflict},
		{"stray markup walks the agent-facing tier", "plugins", strayMarkup},
		{"stray markup walks the reference tier", "docs", strayMarkup},
		{"stray markup walks the historical tier", "docs/plans", strayMarkup},
		{"stray markup walks the go tier", "internal", strayMarkup},
		{"polecat main push walks the agent-facing tier", "plugins", polecatPush},
		{"word ceiling walks the agent-facing tier", "plugins", wordCeiling},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := walkErrorFS{memFS: files, failDir: tc.failDir}
			if err := tc.check(fsys); err == nil {
				t.Errorf("walk of %s failed, but the check returned no error", tc.failDir)
			}
		})
	}
}
