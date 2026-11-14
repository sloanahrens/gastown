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

			reg := NewRegistry()
			reg.Register(tc.entries...)
			got := reg.Validate(tc.paths, tc.unplaced)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Validate(%v, %v) = %q, want %q", tc.paths, tc.unplaced, got, tc.want)
			}
		})
	}
}

// TestRegistryQueries drives the registry the placement files fill, on a map
// this test owns: the default map grows with every placement bead, so an
// assertion that counts its entries would break as the map fills.
func TestRegistryQueries(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(
		Entry{Path: "pool set", Group: "seats", Use: "Use this to move a seat."},
		Entry{Path: "pool list", Group: "seats", Use: "Use this to see the seats."},
		Entry{Path: "status", Group: "orient", Use: "Use this to see where the town stands."},
	)

	if e, ok := reg.Lookup("pool set"); !ok || e.Group != "seats" {
		t.Errorf("Lookup(pool set) = %+v, %v; want the registered entry", e, ok)
	}
	if _, ok := reg.Lookup("missing"); ok {
		t.Error("Lookup(missing) found an entry")
	}
	if got := reg.ByGroup("seats"); len(got) != 2 || got[0].Path != "pool list" || got[1].Path != "pool set" {
		t.Errorf("ByGroup(seats) = %+v; want both seats entries sorted by path", got)
	}
	if got := reg.Search("POOL"); len(got) != 2 || got[0].Path != "pool list" || got[1].Path != "pool set" {
		t.Errorf("Search(POOL) = %+v; want both pool entries sorted by path", got)
	}
	if got := reg.Search("move a seat"); len(got) != 1 || got[0].Path != "pool set" {
		t.Errorf("Search(move a seat) = %+v; want only the entry whose Use matches", got)
	}
}

// TestPackageFunctions drives the package-level entry points the placement
// files and callers use, so deleting a delegation turns it red. It asserts
// membership, never counts: the default map is shared with the placement
// files' inits, so a count here would break as the map fills.
//
//testpolicy:allow parallel — registers into the shared default map, which no other test may read mid-run
func TestPackageFunctions(t *testing.T) {
	entry := Entry{Path: "zz-wiring-alpha", Group: "orient", Use: "Use this to prove registration reaches the default map."}
	Register(entry)

	if got, ok := Lookup(entry.Path); !ok || got != entry {
		t.Errorf("Lookup(%s) = %+v, %v; want the registered entry", entry.Path, got, ok)
	}
	if got := ByGroup(entry.Group); !slices.Contains(got, entry) {
		t.Errorf("ByGroup(%s) = %+v; want the registered entry", entry.Group, got)
	}
	if got := Search(entry.Path); !slices.Contains(got, entry) {
		t.Errorf("Search(%s) = %+v; want the registered entry", entry.Path, got)
	}
	// A registered entry names a command the caller did not pass, so at least
	// this entry must be reported however many placement beads have landed.
	if problems := Validate(nil, nil); len(problems) == 0 {
		t.Error("Validate(nil, nil) reported no problem; want the registered entry flagged as naming no command")
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

// TestHandRunMetricsCommandsSitWithMetrics pins the metrics pipeline steps to
// host, beside the gt metrics entry: internal is gt's own plumbing, and a person
// starts these steps by hand (gt-c9tmkq).
func TestHandRunMetricsCommandsSitWithMetrics(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"metrics all",
		"metrics analyse",
		"metrics extract",
		"metrics import-versions",
		"metrics run",
		"metrics snapshot",
		"metrics storypoints-train",
	} {
		e, ok := Lookup(path)
		if !ok {
			t.Errorf("Lookup(%q) found no entry; the command needs a task map entry", path)
			continue
		}
		if e.Group != "host" {
			t.Errorf("Lookup(%q).Group = %q, want \"host\" beside gt metrics", path, e.Group)
		}
	}
}

// TestInternalHoldsNoMetricsCommands guards the same rule from the other side:
// no metrics command belongs in internal, so one added there later fails this
// test (gt-c9tmkq).
func TestInternalHoldsNoMetricsCommands(t *testing.T) {
	t.Parallel()

	for _, e := range ByGroup("internal") {
		if strings.HasPrefix(e.Path, "metrics") {
			t.Errorf("ByGroup(internal) holds %q; a metrics command is a tool a person runs, not gt plumbing", e.Path)
		}
	}
}

// TestSeatCycleUseNamesTheSituation pins the task map's rule for the cycle
// command (gt-qme1t2): a Use names the situation that calls for the command,
// so the tmux keybinding that usually invokes it is no part of the entry.
func TestSeatCycleUseNamesTheSituation(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("cycle next")
	if !ok {
		t.Fatal(`Lookup("cycle next") found no entry`)
	}
	for _, binding := range []string{"C-b", "keybinding"} {
		if strings.Contains(e.Use, binding) {
			t.Errorf("cycle next Use = %q; it names a tmux keybinding (%q), not the situation that calls for the command", e.Use, binding)
		}
	}
}

// TestMailDeleteUseNamesTheSituation applies the same rule to mail delete
// (gt-jii0vu): "acknowledge" is the help's word for what the command does,
// so the Use names the situation instead.
func TestMailDeleteUseNamesTheSituation(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("mail delete")
	if !ok {
		t.Fatal(`Lookup("mail delete") found no entry`)
	}
	if strings.Contains(e.Use, "acknowledge") {
		t.Errorf("mail delete Use = %q; it names the mechanism the help labels, not the situation that calls for the command", e.Use)
	}
}

// TestChannelAdminStaysInternal pins the grouping judgement place_p9a.go
// records (gt-jii0vu): channel create and delete administer a channel, which
// no task group describes, so a move needs that judgement revisited.
func TestChannelAdminStaysInternal(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"mail channel create", "mail channel delete"} {
		e, ok := Lookup(path)
		if !ok {
			t.Errorf("Lookup(%q) found no entry; the command needs a task map entry", path)
			continue
		}
		if e.Group != "internal" {
			t.Errorf("Lookup(%q).Group = %q, want \"internal\" per the recorded judgement", path, e.Group)
		}
	}
}
