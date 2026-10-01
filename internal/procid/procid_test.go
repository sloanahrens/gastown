package procid

import "testing"

// tokens is a scripted process table: pid -> current start token. A pid
// absent from it has no process.
type tokens map[int]string

func (tt tokens) start(pid int) (string, bool) {
	tok, ok := tt[pid]
	return tok, ok
}

func TestRunning(t *testing.T) {
	t.Parallel()
	recorded := ID{PID: 4242, Start: "1700000000.12"}
	cases := []struct {
		name  string
		id    ID
		table tokens
		want  bool
	}{
		{"same process", recorded, tokens{4242: "1700000000.12"}, true},
		// The case a bare pid gets wrong: the number is live again, but
		// it belongs to a process that started later.
		{"recycled pid", recorded, tokens{4242: "1700000999.5"}, false},
		{"dead pid", recorded, tokens{}, false},
		{"no recorded start", ID{PID: 4242}, tokens{4242: "1700000000.12"}, false},
		{"zero pid", ID{Start: "1"}, tokens{0: "1"}, false},
	}
	for _, tc := range cases {
		if got := tc.id.Running(tc.table.start); got != tc.want {
			t.Errorf("%s: Running = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOf(t *testing.T) {
	t.Parallel()
	table := tokens{7: "99.1", 8: ""}
	if id, ok := of(7, table.start); !ok || id != (ID{PID: 7, Start: "99.1"}) {
		t.Errorf("of(7) = %+v, %v; want {7 99.1}, true", id, ok)
	}
	if _, ok := of(8, table.start); ok {
		t.Error("of(8) succeeded with an empty start token")
	}
	if _, ok := of(9, table.start); ok {
		t.Error("of(9) succeeded for a pid with no process")
	}
}

func TestParseRoundTrip(t *testing.T) {
	t.Parallel()
	id := ID{PID: 123, Start: "1700000000.5"}
	got, err := Parse(id.String() + "\n")
	if err != nil || got != id {
		t.Fatalf("Parse(%q) = %+v, %v; want %+v", id.String(), got, err, id)
	}
	legacy, err := Parse("123")
	if err != nil || legacy != (ID{PID: 123}) {
		t.Errorf("Parse(bare pid) = %+v, %v; want {123 \"\"}", legacy, err)
	}
	for _, bad := range []string{"", "abc", "-5|x", "0"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", bad)
		}
	}
}
