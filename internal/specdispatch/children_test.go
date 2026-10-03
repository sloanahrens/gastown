package specdispatch

import "testing"

func TestChildOpen(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status string
		open   bool
	}{
		{"open", true},
		{"in_progress", true},
		{"blocked", true},
		{"deferred", true},
		{"hooked", true},
		{"pinned", true},
		{"Closed", false},
		{" closed ", false},
		{"tombstone", false},
		{"", true}, // an unknown status is not a closed one
	}
	for _, c := range cases {
		if got := (Child{ID: "gt-x", Status: c.status}).Open(); got != c.open {
			t.Errorf("Child{Status: %q}.Open() = %v, want %v", c.status, got, c.open)
		}
	}
}

func TestOpenChildHold(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		children []Child
		want     string
	}{
		{"no children is not a container", nil, ""},
		{"all closed is not a container", []Child{{ID: "gt-a", Status: "closed"}, {ID: "gt-b", Status: "tombstone"}}, ""},
		{"one open child", []Child{{ID: "gt-a", Status: "open"}}, "container: open child gt-a"},
		{"a closed child does not mask an open one", []Child{{ID: "gt-a", Status: "closed"}, {ID: "gt-b", Status: "in_progress"}}, "container: open child gt-b"},
		{"a deferred child is still open", []Child{{ID: "gt-a", Status: "deferred"}}, "container: open child gt-a"},
		{"two open children", []Child{{ID: "gt-a", Status: "open"}, {ID: "gt-b", Status: "blocked"}}, "container: 2 open children: gt-a, gt-b"},
		{
			"a wide container names three and counts the rest",
			[]Child{{ID: "gt-a", Status: "open"}, {ID: "gt-b", Status: "open"}, {ID: "gt-c", Status: "open"},
				{ID: "gt-d", Status: "open"}, {ID: "gt-e", Status: "open"}},
			"container: 5 open children: gt-a, gt-b, gt-c (+2 more)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := Spec{ID: "gt-parent", Type: "task", Status: "open", Children: c.children}
			if got := OpenChildHold(s); got != c.want {
				t.Errorf("OpenChildHold() = %q, want %q", got, c.want)
			}
		})
	}
}
