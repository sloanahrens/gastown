package gtwrap

import (
	"slices"
	"strings"
	"testing"
)

// TestScanFileArgsRewritesToStdin: each file flag the tool reads from stdin has
// its value replaced by "-", in both the spaced and the "=" form, and the file
// comes back for the caller to feed.
func TestScanFileArgsRewritesToStdin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		tool     string
		args     []string
		want     []string
		wantFile FileArg
	}{
		{"create body file", "bd", []string{"create", "--title", "t", "--body-file", "a.md"},
			[]string{"create", "--title", "t", "--body-file", "-"}, FileArg{"--body-file", "a.md"}},
		{"update body file with =", "bd", []string{"update", "gt-x", "--body-file=/tmp/b.md"},
			[]string{"update", "gt-x", "--body-file=-"}, FileArg{"--body-file", "/tmp/b.md"}},
		{"description-file alias", "bd", []string{"create", "--description-file", "d"},
			[]string{"create", "--description-file", "-"}, FileArg{"--description-file", "d"}},
		{"close reason file", "bd", []string{"close", "gt-x", "--reason-file", "r"},
			[]string{"close", "gt-x", "--reason-file", "-"}, FileArg{"--reason-file", "r"}},
		{"comments add -f", "bd", []string{"comments", "add", "gt-x", "-f", "n.txt"},
			[]string{"comments", "add", "gt-x", "-f", "-"}, FileArg{"-f", "n.txt"}},
		{"note --file", "bd", []string{"note", "gt-x", "--file", "n"},
			[]string{"note", "gt-x", "--file", "-"}, FileArg{"--file", "n"}},
		{"human respond --file", "bd", []string{"human", "respond", "gt-x", "--file=n"},
			[]string{"human", "respond", "gt-x", "--file=-"}, FileArg{"--file", "n"}},
		{"dep add --file", "bd", []string{"dep", "add", "--file", "edges.jsonl"},
			[]string{"dep", "add", "--file", "-"}, FileArg{"--file", "edges.jsonl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			orig := slices.Clone(tc.args)
			got, file, err := ScanFileArgs(tc.tool, tc.args)
			if err != nil || file == nil || *file != tc.wantFile || !slices.Equal(got, tc.want) {
				t.Fatalf("ScanFileArgs = %q, %v, %v; want %q with %v", got, file, err, tc.want, tc.wantFile)
			}
			if !slices.Equal(tc.args, orig) {
				t.Errorf("the caller's arguments were changed to %q", tc.args)
			}
		})
	}
}

// TestScanFileArgsRefusals: a file the tool opens as a path, two files, and a
// file beside a stdin already in use are each refused with the cause named.
func TestScanFileArgsRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tool string
		args []string
		want string
	}{
		{"bd create -f", "bd", []string{"create", "-f", "plan.md"}, "-f names a Mac file the pod cannot read"},
		{"bd create --graph", "bd", []string{"create", "--graph", "p.json"}, "--graph names a Mac file"},
		{"bd create metadata file", "bd", []string{"create", "--metadata", "@m.json"}, "--metadata names a Mac file"},
		{"bd update metadata file", "bd", []string{"update", "x", "--metadata=@m.json"}, "--metadata names a Mac file"},
		{"bd delete --from-file", "bd", []string{"delete", "--from-file", "ids"}, "--from-file names a Mac file"},
		{"bd migrate issues --ids-file", "bd", []string{"migrate", "issues", "--ids-file", "i"}, "--ids-file names a Mac file"},
		{"bd import -i", "bd", []string{"import", "-i", "x.jsonl"}, "-i names a Mac file"},
		{"bd export -o", "bd", []string{"export", "-o", "x.jsonl"}, "-o names a Mac file"},
		{"gt spec lint --template", "gt", []string{"spec", "lint", "gt-x", "--template", "t.md"}, "--template names a Mac file"},
		{"gt session inject -f", "gt", []string{"session", "inject", "s", "-f", "m"}, "-f names a Mac file"},
		{"gt sandbox endpoint --from-export", "gt", []string{"sandbox", "endpoint", "--render", "--from-export", "e"}, "--from-export names a Mac file"},
		{"two files", "bd", []string{"create", "--body-file", "a", "--design-file", "b"}, "names two files (--body-file a, --design-file b) and stdin carries one"},
		{"file with --stdin", "bd", []string{"create", "--stdin", "--design-file", "b"}, "cannot be sent because stdin is already in use"},
		{"file with a - value", "bd", []string{"comments", "add", "x", "-f", "n", "--design-file", "-"}, "cannot be sent because stdin is already in use"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, file, err := ScanFileArgs(tc.tool, tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) || got != nil || file != nil {
				t.Fatalf("ScanFileArgs = %q, %v, %v; want a refusal containing %q", got, file, err, tc.want)
			}
		})
	}
}

// TestScanFileArgsLeavesOtherCallsAlone: a call with no file argument, a flag
// that merely shares a name under another command, an inline metadata value and
// the other tools come back unchanged.
func TestScanFileArgsLeavesOtherCallsAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tool string
		args []string
	}{
		{"no flags", "bd", []string{"show", "gt-x"}},
		{"stdin already named", "bd", []string{"create", "--stdin", "--title", "t"}},
		{"- value", "bd", []string{"create", "--body-file", "-"}},
		{"-f under another command", "bd", []string{"list", "-f", "x"}},
		{"inline metadata", "bd", []string{"update", "x", "--metadata", `{"a":1}`}},
		{"gt escalate stdin", "gt", []string{"escalate", "--stdin", "msg"}},
		{"gt spec lint", "gt", []string{"spec", "lint", "gt-x"}},
		{"om review", "om", []string{"review", "--body-file", "x"}},
		{"hm query", "hm", []string{"query", "-f", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			orig := slices.Clone(tc.args)
			got, file, err := ScanFileArgs(tc.tool, tc.args)
			if err != nil || file != nil || !slices.Equal(got, orig) {
				t.Fatalf("ScanFileArgs = %q, %v, %v; want the arguments unchanged", got, file, err)
			}
		})
	}
}
