package bdgateway

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bdshim"
	"github.com/steveyegge/gastown/internal/cmdtree"
)

// loadDeployPolicy reads the policy the gateway image ships, from the
// repository's deploy/bd/verbs.json.
func loadDeployPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := LoadPolicyFile(filepath.Join("..", "..", "deploy", "bd", "verbs.json"))
	if err != nil {
		t.Fatalf("LoadPolicyFile(deploy/bd/verbs.json): %v", err)
	}
	return p
}

// TestLoadPolicyFile: the loader reads a file from a path, refuses a document
// it cannot trust while naming the file that holds it, and reports a missing
// file as the open failure it is.
func TestLoadPolicyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	good := filepath.Join(dir, "verbs.json")
	if err := os.WriteFile(good, []byte(fixturePolicy), 0o600); err != nil {
		t.Fatalf("write good policy: %v", err)
	}
	if _, err := LoadPolicyFile(good); err != nil {
		t.Fatalf("LoadPolicyFile(%s) = %v, want it to load", good, err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"refused_flags": [], "verbs": []}`), 0o600); err != nil {
		t.Fatalf("write bad policy: %v", err)
	}
	_, err := LoadPolicyFile(bad)
	if !errors.Is(err, ErrBadVerbsFile) {
		t.Fatalf("LoadPolicyFile(%s) = %v, want ErrBadVerbsFile", bad, err)
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("LoadPolicyFile(%s) error = %q, want it to name the file", bad, err)
	}

	if _, err := LoadPolicyFile(filepath.Join(dir, "missing.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadPolicyFile(missing) = %v, want fs.ErrNotExist", err)
	}
}

// sortedVerbNames returns the policy's verb paths, sorted, so a failure names
// them in one order.
func sortedVerbNames(p *Policy) []string {
	names := make([]string, 0, len(p.verbs))
	for name := range p.verbs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestVerbsFileMatchesCommandTree: every verb deploy/bd/verbs.json allows runs
// in the bd the repository's command tree records, and every verb the shim
// answers has a policy entry. A bd upgrade that renames a verb, or a handler
// added without a policy row, therefore fails this gate instead of failing a
// call behind the gateway.
func TestVerbsFileMatchesCommandTree(t *testing.T) {
	t.Parallel()
	p := loadDeployPolicy(t)
	tree, _, err := cmdtree.LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}

	names := sortedVerbNames(p)
	for _, name := range names {
		res := tree.Resolve(p.verbs[name].Path)
		if res.OK {
			continue
		}
		detail := fmt.Sprintf("no such command word %q", res.Unknown)
		if res.HelpOnly {
			detail = "stops on a help-only parent"
		}
		t.Errorf("deploy/bd/verbs.json allows %q, which bd's command tree does not run: %s", name, detail)
	}

	for _, verb := range bdshim.HandlerVerbs() {
		if !policyCoversVerb(p, verb) {
			t.Errorf("bdshim answers %q, but deploy/bd/verbs.json has no path that starts with it", verb)
		}
	}
}

// policyCoversVerb reports whether any allowed path names verb as its first
// word.
func policyCoversVerb(p *Policy, verb string) bool {
	for _, v := range p.verbs {
		if len(v.Path) > 0 && v.Path[0] == verb {
			return true
		}
	}
	return false
}

// TestRefusedFlagsExistInCommandTree: every refused flag is a persistent bd
// flag, so the refusal names something bd would otherwise accept. -C is the
// shorthand of --directory and is listed by its letter because that is how it
// appears on a command line.
func TestRefusedFlagsExistInCommandTree(t *testing.T) {
	t.Parallel()
	p := loadDeployPolicy(t)
	_, snap, err := cmdtree.LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}
	persistent := make(map[string]bool, len(snap.PersistentFlags))
	for _, name := range snap.PersistentFlags {
		persistent[name] = true
	}
	for name := range p.refusedFlags {
		if name == "C" {
			if !persistent["directory"] {
				t.Error("deploy/bd/verbs.json refuses -C, but bd's tree has no --directory for it to shorten")
			}
			continue
		}
		if !persistent[name] {
			t.Errorf("deploy/bd/verbs.json refuses --%s, which is not a persistent bd flag", name)
		}
	}
}

// TestLeadingValueFlagsMatchCommandTree: the flags the resolver skips a value
// for are the bd tree's value-taking persistent flags, so a bd upgrade that
// moves one cannot make the gateway read a value as the verb.
func TestLeadingValueFlagsMatchCommandTree(t *testing.T) {
	t.Parallel()
	got := make([]string, 0, len(leadingValueFlags))
	for name := range leadingValueFlags {
		got = append(got, name)
	}
	sort.Strings(got)
	want := cmdtree.BdGlobalValueFlagNames()
	if !slices.Equal(got, want) {
		t.Fatalf("leadingValueFlags = %v, want bd's value-taking persistent flags %v", got, want)
	}
}
