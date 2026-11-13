package taskmap

import (
	"slices"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	placed := Entry{Path: "status", Group: "orient", Use: "Use this to see where the town stands."}
	tests := []struct {
		name     string
		entries  []Entry
		paths    []string
		unplaced []string
		want     []string
	}{
		{
			name:     "clean",
			entries:  []Entry{placed},
			paths:    []string{"status", "new-thing"},
			unplaced: []string{"new-thing"},
		},
		{
			name:    "command with no entry and no unplaced line",
			entries: []Entry{placed},
			paths:   []string{"status", "orphan"},
			want:    []string{`"orphan": no task map entry and no line in testdata/unplaced`},
		},
		{
			name:     "unplaced line has an entry",
			entries:  []Entry{placed},
			paths:    []string{"status"},
			unplaced: []string{"status"},
			want:     []string{`"status": line in testdata/unplaced but the command has an entry (remove the line)`},
		},
		{
			name:     "unplaced line names no command",
			unplaced: []string{"gone"},
			want:     []string{`"gone": line in testdata/unplaced but no such command (remove the line)`},
		},
		{
			name:    "entry has unknown group",
			entries: []Entry{{Path: "status", Group: "nowhere", Use: "Use this to see where the town stands."}},
			paths:   []string{"status"},
			want:    []string{`"status": task map entry has unknown group "nowhere"`},
		},
		{
			name:    "entry has empty Use",
			entries: []Entry{{Path: "status", Group: "orient"}},
			paths:   []string{"status"},
			want:    []string{`"status": task map entry has an empty Use`},
		},
		{
			name:    "entry has multi-line Use",
			entries: []Entry{{Path: "status", Group: "orient", Use: "Use this to see where the town stands.\nRead it before you act."}},
			paths:   []string{"status"},
			want:    []string{`"status": task map entry Use spans more than one line`},
		},
		{
			name:    "entry names no command",
			entries: []Entry{placed},
			want:    []string{`"status": task map entry names no command`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := newRegistry()
			reg.register(tc.entries...)
			got := reg.validate(tc.paths, tc.unplaced)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("validate(%v, %v) = %q, want %q", tc.paths, tc.unplaced, got, tc.want)
			}
		})
	}
}

// TestPackageFunctions drives the package-level entry points the placement
// files and the command walk use, so deleting any delegation turns it red.
//
//testpolicy:allow parallel — registers into the package-level map, which no other test may read mid-run
func TestPackageFunctions(t *testing.T) {
	Register(
		Entry{Path: "zz-wiring-alpha", Group: "orient", Use: "Use this to prove registration reaches the default map."},
		Entry{Path: "zz-wiring-beta", Group: "dispatch", Use: "Use this to prove search reads the use line."},
	)

	if e, ok := Lookup("zz-wiring-alpha"); !ok || e.Group != "orient" {
		t.Errorf("Lookup(zz-wiring-alpha) = %+v, %v; want the registered entry", e, ok)
	}
	if _, ok := Lookup("zz-wiring-missing"); ok {
		t.Error("Lookup(zz-wiring-missing) found an entry")
	}

	got := Search("WIRING")
	if len(got) != 2 || got[0].Path != "zz-wiring-alpha" || got[1].Path != "zz-wiring-beta" {
		t.Errorf("Search(WIRING) = %+v; want both wiring entries sorted by path", got)
	}
	if got := Search("prove search"); len(got) != 1 || got[0].Path != "zz-wiring-beta" {
		t.Errorf("Search(prove search) = %+v; want only the entry whose Use matches", got)
	}

	// A clean map: every registered entry is a command and every command has
	// an entry.
	var paths []string
	for _, g := range Groups() {
		for _, e := range ByGroup(g) {
			paths = append(paths, e.Path)
		}
	}
	paths = append(paths, "zz-wiring-alpha", "zz-wiring-beta")
	if problems := Validate(paths, nil); len(problems) > 0 {
		t.Errorf("Validate over the registered entries = %q; want clean", problems)
	}
	if problems := Validate(nil, nil); len(problems) == 0 {
		t.Error("Validate(nil, nil) reported no problem; want the registered entries to be flagged as naming no command")
	}
}

func TestGroups(t *testing.T) {
	t.Parallel()

	want := []Group{"orient", "dispatch", "land", "review", "roll", "seats", "sandboxes", "birds", "host", "dolt", "cost", "shutdown", "internal"}
	got := Groups()
	if !slices.Equal(got, want) {
		t.Fatalf("Groups() = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if Groups()[0] != "orient" {
		t.Error("Groups() shares its slice with the caller")
	}
	for _, g := range want {
		d := Describe(g)
		if !strings.HasPrefix(d, "Use this ") || strings.Contains(d, "\n") {
			t.Errorf("Describe(%s) = %q; want one line starting \"Use this \"", g, d)
		}
	}
	if d := Describe("nowhere"); d != "" {
		t.Errorf("Describe(nowhere) = %q; want empty", d)
	}
}
