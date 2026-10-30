package docslint

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestWordCountTable(t *testing.T) {
	t.Parallel()
	// The gt-shrnc fixture: 1,498 words plus five lone em dashes. The shell's
	// wc -w counted the dashes differently from one locale to the next, which
	// moved a file across the ceiling.
	gtShrnc := strings.Repeat("a ", 1498) + strings.Repeat("— ", 5) + "\n"
	tests := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"only whitespace", " \t\n\n", 0},
		{"one word", "word", 1},
		{"tabs and newlines separate", "one\ttwo\nthree\r\nfour", 4},
		{"a lone em dash is a word", "—", 1},
		{"an em dash between words", "one — two", 3},
		{"a non-breaking space separates", "one\u00a0two", 2},
		{"the gt-shrnc fixture", gtShrnc, 1503},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := WordCount(tc.text); got != tc.want {
				t.Errorf("WordCount(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

// The count must not move with an installed locale: gt-shrnc was a verdict
// that changed with the caller's LC_ALL, and this one holds where wc -w did
// not.
//
//testpolicy:allow parallel — t.Setenv may not run under t.Parallel
func TestWordCountIsLocaleFree(t *testing.T) {
	text := strings.Repeat("a ", 1498) + strings.Repeat("— ", 5) + "one\u00a0two\vthree"
	//testpolicy:allow no-env — LC_ALL is the input under test: the count must not depend on it
	t.Setenv("LC_ALL", "C")
	bytewise := WordCount(text)
	//testpolicy:allow no-env — LC_ALL is the input under test: the count must not depend on it
	t.Setenv("LC_ALL", "en_US.UTF-8")
	underUTF8 := WordCount(text)
	if bytewise != underUTF8 {
		t.Errorf("WordCount = %d under LC_ALL=C and %d under LC_ALL=en_US.UTF-8, want the same count", bytewise, underUTF8)
	}
	if want := 1506; bytewise != want {
		t.Errorf("WordCount = %d, want %d", bytewise, want)
	}
}

// TotalAndFailure: the tier total, then a read that fails. The two run in one
// test so the failing case is read against the passing total it would have
// contributed to.
func TestWordsTotalAndFailure(t *testing.T) {
	t.Parallel()
	files := memFS{
		"AGENTS.md":           "one two three\n",
		"docs/HOOKS.md":       "four five\n",
		"plugins/p/plugin.md": "",
		"internal/formula/formulas/mol-big.formula.toml":                   "six\tseven\neight",
		"internal/formula/formulas/mol-polecat-work.formula.toml":          "nine",
		"internal/formula/formulas/mol-polecat-work-monorepo.formula.toml": "ten eleven",
		// Neither is agent-facing, so neither may reach the total.
		"README.md":            strings.Repeat("out of tier ", 500),
		"docs/guides/guide.md": strings.Repeat("out of tier ", 500),
	}

	t.Run("the whole agent-facing tier, exempt formulas included", func(t *testing.T) {
		got, err := Words(files)
		if err != nil {
			t.Fatalf("Words(): %v", err)
		}
		// 3 + 2 + 0 + 3 + 1 + 2, with both exempt formulas counted.
		if want := 11; got != want {
			t.Errorf("Words() = %d, want %d", got, want)
		}
	})

	t.Run("a file that cannot be read stops the run", func(t *testing.T) {
		unreadable := unreadableFS{memFS: files, fail: "docs/HOOKS.md"}
		total, err := Words(unreadable)
		if err == nil {
			t.Fatalf("Words() with an unreadable file = %d, nil; want an error", total)
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("error = %v, want the read's %v", err, fs.ErrPermission)
		}
		if !strings.Contains(err.Error(), "docs/HOOKS.md") {
			t.Errorf("error = %q, want it to name the file it could not read", err)
		}
	})

	t.Run("a tier that cannot be listed stops the run", func(t *testing.T) {
		total, err := Words(walkErrorFS{memFS: files, failDir: "plugins"})
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("Words() error = %v, want the walker's %v", err, fs.ErrPermission)
		}
		if total != 0 {
			t.Errorf("Words() = %d alongside the error, want no total", total)
		}
	})
}

// unreadableFS is memFS with one file whose read always fails, the way a file
// the process may not read does.
type unreadableFS struct {
	memFS
	fail string
}

func (u unreadableFS) ReadFile(p string) ([]byte, error) {
	if p == u.fail {
		return nil, fs.ErrPermission
	}
	return u.memFS.ReadFile(p)
}
