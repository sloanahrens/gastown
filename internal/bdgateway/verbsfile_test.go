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

// bdTreeGapsSnapshot is the command tree snapshot bdTreeGaps is written
// against: beads b9f68260ed2a494972fe8aa922b6408a91905f0c, which predates the
// sample verb. The exceptions apply only while the embedded snapshot is this
// commit, so a refreshed snapshot re-checks every verb and a bd that renamed
// the verb cannot stay excused.
const bdTreeGapsSnapshot = "b9f68260ed2a494972fe8aa922b6408a91905f0c"

// bdTreeGaps names the verbs deploy/bd/verbs.json allows that the snapshot
// above does not run, with the bd commit that offers them. The town runs bd
// b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f. An entry excuses only that exact
// verb; once a refreshed snapshot runs it, staleGapFailures reports the row so
// it is deleted.
var bdTreeGaps = map[string]string{
	"sample add":  "b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f",
	"sample list": "b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f",
}

// TestVerbsFileMatchesCommandTree: every verb deploy/bd/verbs.json allows runs
// in the bd the repository's command tree records, except a verb in
// bdTreeGaps, which names the bd commit that runs it while the snapshot is
// bdTreeGapsSnapshot. A bd upgrade that renames a verb therefore fails this
// gate instead of failing a call behind the gateway. internal/bdshim tests the
// other half, that every verb the shim answers is in the file, beside the
// handler registry it can read.
func TestVerbsFileMatchesCommandTree(t *testing.T) {
	t.Parallel()
	p := loadDeployPolicy(t)
	tree, snap, err := cmdtree.LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}
	var exceptions map[string]string
	if snap.Commit == bdTreeGapsSnapshot {
		exceptions = bdTreeGaps
	}
	for _, msg := range treeGapFailures(verbsAbsentFromTree(p, tree), exceptions) {
		t.Error(msg)
	}
	for _, msg := range staleGapFailures(bdTreeGaps, tree) {
		t.Error(msg)
	}
}

// verbGap is one allowed verb the command tree does not run, with the message
// the gate reports for it.
type verbGap struct {
	name    string
	message string
}

// verbsAbsentFromTree returns one gap per allowed verb the tree does not run.
func verbsAbsentFromTree(p *Policy, tree *cmdtree.Tree) []verbGap {
	var missing []verbGap
	for _, name := range sortedVerbNames(p) {
		res := tree.Resolve(p.verbs[name].Path)
		if res.OK {
			continue
		}
		detail := fmt.Sprintf("no such command word %q", res.Unknown)
		if res.HelpOnly {
			detail = "stops on a help-only parent"
		}
		missing = append(missing, verbGap{
			name:    name,
			message: fmt.Sprintf("deploy/bd/verbs.json allows %q, which bd's command tree does not run: %s", name, detail),
		})
	}
	return missing
}

// treeGapFailures returns one message per gap the exception list does not
// name, so an exception excuses only the verbs it lists.
func treeGapFailures(gaps []verbGap, exceptions map[string]string) []string {
	var failures []string
	for _, gap := range gaps {
		if _, ok := exceptions[gap.name]; ok {
			continue
		}
		failures = append(failures, gap.message)
	}
	return failures
}

// staleGapFailures returns one message per exception row the command tree now
// runs, so the list cannot outlive the snapshot it works around.
func staleGapFailures(exceptions map[string]string, tree *cmdtree.Tree) []string {
	var failures []string
	for name, commit := range exceptions {
		if tree.Resolve(strings.Fields(name)).OK {
			failures = append(failures, fmt.Sprintf("bdTreeGaps lists %q (bd %s), but the command tree runs it: refresh the snapshot and delete the row", name, commit))
		}
	}
	return failures
}

// TestVerbsAbsentFromTreeFindsAMissingVerb drives the failing branch of the
// command-tree gate with a verb bd does not hold, so a check that passes every
// file cannot land unnoticed.
func TestVerbsAbsentFromTreeFindsAMissingVerb(t *testing.T) {
	t.Parallel()
	p, err := ParsePolicy(strings.NewReader(`{"refused_flags": ["db"], "verbs": [
		{"path": "show", "class": "read"},
		{"path": "not-a-bd-verb", "class": "read"}
	]}`))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	tree, _, err := cmdtree.LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}
	missing := verbsAbsentFromTree(p, tree)
	if len(missing) != 1 || missing[0].name != "not-a-bd-verb" || !strings.Contains(missing[0].message, "not-a-bd-verb") {
		t.Fatalf("verbsAbsentFromTree = %v, want one gap naming not-a-bd-verb", missing)
	}
}

// TestTreeGapFailures: a gap the exception list does not name is reported and
// a listed gap is excused, so the list cannot widen the gate.
func TestTreeGapFailures(t *testing.T) {
	t.Parallel()
	exceptions := map[string]string{"sample add": "b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f"}
	gaps := []verbGap{
		{name: "sample add", message: "sample add is missing"},
		{name: "sample list", message: "sample list is missing"},
	}
	got := treeGapFailures(gaps, exceptions)
	if len(got) != 1 || !strings.Contains(got[0], "sample list is missing") {
		t.Errorf("treeGapFailures = %v, want only the unlisted gap reported", got)
	}
}

// TestStaleGapFailures: an exception row the command tree now runs is
// reported, and one the tree still lacks is not, so the row is deleted when a
// refreshed snapshot stops needing it.
func TestStaleGapFailures(t *testing.T) {
	t.Parallel()
	tree := cmdtree.NewTree()
	tree.Add([]string{"sample", "add"}, nil, false)
	exceptions := map[string]string{
		"sample add":  "b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f",
		"sample list": "b7e5e9ceefaf49a701f3e2c1487a72aa0feb077f",
	}
	got := staleGapFailures(exceptions, tree)
	if len(got) != 1 || !strings.Contains(got[0], "sample add") {
		t.Errorf("staleGapFailures = %v, want only sample add reported as stale", got)
	}
}

// TestAuthorizeSample: the file the gateway image ships allows bd sample add
// for the daemon and operator classes only, bd sample list for every class,
// and still refuses a verb the file does not name.
func TestAuthorizeSample(t *testing.T) {
	t.Parallel()
	p := loadDeployPolicy(t)
	cases := []struct {
		name      string
		class     TokenClass
		argv      []string
		wantErr   error
		wantVerb  string
		wantClass VerbClass
	}{
		{"daemon token writes a sample row", TokenDaemon, []string{"sample", "add", "--kind", "claude_plan", "--at", "2026-11-17T20:00:00Z", "--json", "-"}, nil, "sample add", ClassWrite},
		{"operator token writes a sample row", TokenOperator, []string{"sample", "add", "--kind", "claude_plan"}, nil, "sample add", ClassWrite},
		{"read token cannot write a sample row", TokenRead, []string{"sample", "add", "--kind", "claude_plan"}, ErrClassDenied, "", ""},
		{"read token lists samples", TokenRead, []string{"sample", "list", "--kind", "claude_plan", "--json"}, nil, "sample list", ClassRead},
		{"daemon token lists samples", TokenDaemon, []string{"sample", "list"}, nil, "sample list", ClassRead},
		{"operator token lists samples", TokenOperator, []string{"sample", "list", "--limit", "10"}, nil, "sample list", ClassRead},
		{"a verb the file does not name is refused", TokenOperator, []string{"dolt", "push"}, ErrVerbNotAllowed, "", ""},
		{"an unnamed sample subcommand is refused", TokenDaemon, []string{"sample", "purge"}, ErrVerbNotAllowed, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := p.Authorize(tc.class, tc.argv)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Authorize(%q, %q) = %v, want %v", tc.class, tc.argv, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authorize(%q, %q) = %v, want it allowed", tc.class, tc.argv, err)
			}
			if v.Name() != tc.wantVerb || v.Class != tc.wantClass {
				t.Errorf("Authorize(%q, %q) matched %q (%s), want %q (%s)", tc.class, tc.argv, v.Name(), v.Class, tc.wantVerb, tc.wantClass)
			}
		})
	}
}

// TestAuthorizeGateCheck: the file the gateway image ships allows bd gate
// check, the call the daemon's patrol scan runs for every rig on every tick
// ("gate check --type=timer"), for the daemon and operator classes only. It
// evaluates timer gates and closes resolved ones, so the read class is
// refused with the class error.
func TestAuthorizeGateCheck(t *testing.T) {
	t.Parallel()
	p := loadDeployPolicy(t)
	cases := []struct {
		name      string
		class     TokenClass
		argv      []string
		wantErr   error
		wantVerb  string
		wantClass VerbClass
	}{
		{"daemon token checks timer gates", TokenDaemon, []string{"gate", "check", "--type=timer"}, nil, "gate check", ClassWrite},
		{"operator token checks timer gates", TokenOperator, []string{"gate", "check"}, nil, "gate check", ClassWrite},
		{"read token cannot check gates", TokenRead, []string{"gate", "check", "--type=timer"}, ErrClassDenied, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, err := p.Authorize(tc.class, tc.argv)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Authorize(%q, %q) = %v, want %v", tc.class, tc.argv, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authorize(%q, %q) = %v, want it allowed", tc.class, tc.argv, err)
			}
			if v.Name() != tc.wantVerb || v.Class != tc.wantClass {
				t.Errorf("Authorize(%q, %q) matched %q (%s), want %q (%s)", tc.class, tc.argv, v.Name(), v.Class, tc.wantVerb, tc.wantClass)
			}
		})
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
