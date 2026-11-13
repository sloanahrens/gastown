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
// in the bd the repository's command tree records. A bd upgrade that renames a
// verb therefore fails this gate instead of failing a call behind the gateway.
// internal/bdshim tests the other half, that every verb the shim answers is in
// the file, beside the handler registry it can read.
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

// TestRootFlagsMatchCommandTree: the resolver's flag table is the bd tree's
// persistent flags, split into value-taking by the same list the command-tree
// scan uses. A bd upgrade that adds, drops or retypes a persistent flag fails
// here rather than shifting the verb words at the gateway.
func TestRootFlagsMatchCommandTree(t *testing.T) {
	t.Parallel()
	_, snap, err := cmdtree.LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}

	var valueFlags, longFlags []string
	for spelling, kind := range bdRootFlags {
		if kind == flagValue {
			valueFlags = append(valueFlags, spelling)
		}
		if spelling == "-C" {
			continue
		}
		if !strings.HasPrefix(spelling, "--") {
			t.Errorf("bdRootFlags holds %q, which is neither -C nor a long flag", spelling)
			continue
		}
		longFlags = append(longFlags, strings.TrimPrefix(spelling, "--"))
	}
	sort.Strings(longFlags)
	if !slices.Equal(longFlags, snap.PersistentFlags) {
		t.Errorf("bdRootFlags long names = %v, want bd's persistent flags %v", longFlags, snap.PersistentFlags)
	}
	sort.Strings(valueFlags)
	if want := cmdtree.BdGlobalValueFlagNames(); !slices.Equal(valueFlags, want) {
		t.Errorf("bdRootFlags value-taking flags = %v, want %v", valueFlags, want)
	}
}
