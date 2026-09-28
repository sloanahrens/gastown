package testutil

import (
	"errors"
	"strings"
	"testing"
)

func TestTestCountFromArgs(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{nil, 1},
		{[]string{"-test.v=true", "-test.count=3"}, 3},
		{[]string{"-test.count", "5"}, 5},
		{[]string{"--test.count=2"}, 2},
		{[]string{"-test.count=0"}, 1},
		{[]string{"-test.count=x"}, 1},
		{[]string{"-test.run", "TestX"}, 1},
	}
	for _, c := range cases {
		if got := testCountFromArgs(c.args); got != c.want {
			t.Errorf("testCountFromArgs(%q) = %d, want %d", c.args, got, c.want)
		}
	}
}

func TestTakeDoltPoolPathExhausts(t *testing.T) {
	doltPool.Lock()
	saved := doltPool.entries
	savedNext := doltPool.next
	doltPool.entries = []string{t.TempDir() + "/a/.beads/dolt"}
	doltPool.next = 0
	doltPool.Unlock()
	t.Cleanup(func() {
		doltPool.Lock()
		doltPool.entries, doltPool.next = saved, savedNext
		doltPool.Unlock()
	})
	if _, err := takeDoltPoolPath(); err != nil {
		t.Fatalf("first take: %v", err)
	}
	if _, err := takeDoltPoolPath(); err == nil {
		t.Fatal("second take from a one-entry pool succeeded; an exhausted pool must fail, never create a database mid-run")
	}
}

func TestCatalogViolations(t *testing.T) {
	pool := map[string]bool{"testdb_a": true, "testdb_b": true}
	image := []string{"gt_test", "information_schema", "mysql"}
	with := func(extra ...string) []string { return append(append([]string{}, image...), extra...) }
	cases := []struct {
		name    string
		present []string
		dropped []string
		want    []string // fragments the error must carry; nil = no error
	}{
		{name: "clean", present: with("testdb_a", "testdb_b")},
		{name: "stray", present: with("testdb_a", "testdb_b", "beads"),
			want: []string{`database "beads" was created`}},
		{name: "pool database dropped", present: with("testdb_a"),
			want: []string{`database "testdb_b" was dropped`}},
		{name: "image database dropped", present: []string{"information_schema", "mysql", "testdb_a", "testdb_b"},
			want: []string{`database "gt_test" was dropped`}},
		{name: "created then dropped", present: with("testdb_a", "testdb_b"), dropped: []string{"hq"},
			want: []string{`database "hq" was dropped (Dolt still holds it for dolt_undrop)`}},
		{name: "dropped and recreated", present: with("testdb_a", "testdb_b"), dropped: []string{"testdb_a"},
			want: []string{`database "testdb_a" was dropped and created again`}},
		{name: "several", present: with("testdb_a", "x", "y"),
			want: []string{`"x" was created`, `"y" was created`, `"testdb_b" was dropped`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := catalogViolations(c.present, c.dropped, pool)
			if c.want == nil {
				if err != nil {
					t.Fatalf("catalogViolations = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrDoltCatalogChanged) {
				t.Fatalf("catalogViolations = %v, want an error wrapping ErrDoltCatalogChanged", err)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

// The refusals are dolt 2.0.7's own words, captured from DoltDockerImage;
// TestDoltCatalogGuardFiresOnRealServer checks them against the real server.
func TestParseUndropRefusal(t *testing.T) {
	cases := []struct {
		msg     string
		want    []string
		wantErr bool
	}{
		{msg: "Error 1105 (HY000): no database name specified. there are no databases currently available to be undropped"},
		{msg: "Error 1105 (HY000): no database name specified. available databases that can be undropped: stray2",
			want: []string{"stray2"}},
		{msg: "Error 1105 (HY000): no database name specified. available databases that can be undropped: gt_x-y, stray2",
			want: []string{"gt_x-y", "stray2"}},
		{msg: "Error 1045: access denied", wantErr: true},
	}
	for _, c := range cases {
		got, err := parseUndropRefusal(c.msg)
		if (err != nil) != c.wantErr {
			t.Errorf("parseUndropRefusal(%q) err = %v, wantErr %v", c.msg, err, c.wantErr)
			continue
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("parseUndropRefusal(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}
