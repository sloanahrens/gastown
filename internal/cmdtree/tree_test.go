package cmdtree

import (
	"testing"

	"github.com/spf13/cobra"
)

func testTree() *Tree {
	t := NewTree()
	t.Add([]string{"show"}, nil, true)
	t.Add([]string{"close"}, []string{"done"}, true)
	t.Add([]string{"mol"}, nil, false)
	t.Add([]string{"mol", "wisp"}, nil, false)
	t.Add([]string{"mol", "wisp", "list"}, nil, true)
	t.Add([]string{"mq"}, nil, false)
	t.Add([]string{"mq", "reject"}, nil, true)
	t.Add([]string{"hook"}, nil, true)
	t.Add([]string{"hook", "show"}, nil, true)
	return t
}

func TestResolve(t *testing.T) {
	t.Parallel()
	tree := testTree()
	cases := []struct {
		name    string
		words   []string
		ok      bool
		matched string
		unknown string
	}{
		{"unknown top level", []string{"rigs"}, false, "", "rigs"},
		{"leaf with args", []string{"show", "gt-1"}, true, "show", ""},
		{"nested", []string{"mol", "wisp", "list"}, true, "mol wisp list", ""},
		{"alias", []string{"done"}, true, "done", ""},
		{"unknown sub under argument-free parent", []string{"mq", "close"}, false, "mq", "close"},
		{"unknown nested sub", []string{"mol", "wisp", "create"}, false, "mol wisp", "create"},
		{"word under args-taking parent", []string{"hook", "status"}, true, "hook", ""},
		{"known child under args-taking parent", []string{"hook", "show"}, true, "hook show", ""},
		{"argument-free parent alone", []string{"mq"}, true, "mq", ""},
		{"argument-free parent with non-word arg", []string{"mq", "gt-12"}, true, "mq", ""},
		{"no words", nil, true, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tree.Resolve(tc.words)
			if got.OK != tc.ok || got.MatchedPath() != tc.matched || got.Unknown != tc.unknown {
				t.Fatalf("Resolve(%q) = ok=%v matched=%q unknown=%q; want ok=%v matched=%q unknown=%q",
					tc.words, got.OK, got.MatchedPath(), got.Unknown, tc.ok, tc.matched, tc.unknown)
			}
		})
	}
}

func TestFromCobra(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "gt"}
	mq := &cobra.Command{Use: "mq"}
	reject := &cobra.Command{Use: "reject <id>", Aliases: []string{"rj"}, Run: func(*cobra.Command, []string) {}}
	hidden := &cobra.Command{Use: "secret", Hidden: true, Run: func(*cobra.Command, []string) {}}
	mq.AddCommand(reject, hidden)
	root.AddCommand(mq)

	tree := FromCobra(root, func(c *cobra.Command) bool { return c.Runnable() })
	for _, words := range [][]string{{"mq", "reject"}, {"mq", "rj"}, {"mq", "secret"}} {
		if r := tree.Resolve(words); !r.OK {
			t.Errorf("Resolve(%q) not OK: unknown %q", words, r.Unknown)
		}
	}
	if r := tree.Resolve([]string{"mq", "close"}); r.OK {
		t.Errorf("Resolve(mq close) OK; want unknown subcommand")
	}
}
