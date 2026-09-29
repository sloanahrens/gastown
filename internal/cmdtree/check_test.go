package cmdtree

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	t.Parallel()
	gt := testTree()
	bd := NewTree()
	bd.Add([]string{"sync"}, nil, true)
	bd.Add([]string{"list"}, nil, true)
	refs := []Ref{
		{File: "a.toml", Line: 1, Bin: "gt", Words: []string{"show", "x"}},
		{File: "a.toml", Line: 2, Bin: "gt", Words: []string{"rigs"}},
		{File: "a.toml", Line: 3, Bin: "gt", Words: []string{"mq", "close"}},
		{File: "a.toml", Line: 4, Bin: "bd", Words: []string{"sync"}},
		{File: "a.toml", Line: 5, Bin: "bd", Words: []string{"list"}},
		{File: "a.toml", Line: 6, Bin: "bd", Words: []string{"daemons", "killall"}},
	}
	var got []string
	for _, v := range Check(refs, map[string]*Tree{"gt": gt, "bd": bd}) {
		got = append(got, v.String())
	}
	want := []string{
		`a.toml:2: gt rigs: unknown command "rigs" for gt`,
		`a.toml:3: gt mq close: unknown subcommand "close" for "gt mq"`,
		`a.toml:4: bd sync: ` + BdSyncDenied,
		`a.toml:6: bd daemons killall: unknown command "daemons" for bd`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Check:\n got\n%s\n want\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCheckMissingTree(t *testing.T) {
	t.Parallel()
	v := Check([]Ref{{File: "f", Line: 1, Bin: "bd", Words: []string{"list"}}}, map[string]*Tree{})
	if len(v) != 1 || !strings.Contains(v[0].Reason, "no command tree") {
		t.Fatalf("Check with no bd tree = %+v; want a violation, not a silent pass", v)
	}
}
