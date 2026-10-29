package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// fakeTree answers a revision's file inventory from a fixed table, so a test
// reads the target branch without a git run.
type fakeTree map[string][]string

func (f fakeTree) TreeFileBlobs(rev string) (map[string]string, error) {
	files, ok := f[rev]
	if !ok {
		return nil, errors.New("fatal: Not a valid object name " + rev)
	}
	blobs := make(map[string]string, len(files))
	for _, file := range files {
		blobs[file] = "blob-" + file
	}
	return blobs, nil
}

// rigAt is a rig directory holding a shared bare repo, which is what
// specTargetTree reads.
func rigAt(t *testing.T) string {
	t.Helper()
	rigPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rigPath, ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return rigPath
}

func openTree(fake fakeTree) treeOpener {
	return func(string) treeReader { return fake }
}

// The inventory is origin's branch when origin has one, and the local branch
// only when it does not: a local main can lag what the landing worker targets
// (gt-7t1im).
func TestSpecTargetTreeReadsOriginFirst(t *testing.T) {
	t.Parallel()
	rigPath := rigAt(t)
	tests := []struct {
		name  string
		tree  fakeTree
		files []string
	}{
		{
			name:  "origin wins over the local branch",
			tree:  fakeTree{"origin/main": {"a.go"}, "main": {"b.go"}},
			files: []string{"a.go"},
		},
		{
			name:  "the local branch answers when origin has none",
			tree:  fakeTree{"main": {"b.go"}},
			files: []string{"b.go"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := specTargetTree(rigPath, openTree(tc.tree))
			if target.Err != "" {
				t.Fatalf("Err = %q, want an inventory", target.Err)
			}
			if target.Branch != "main" {
				t.Errorf("Branch = %q, want main", target.Branch)
			}
			got := make([]string, 0, len(target.Files))
			for file := range target.Files {
				got = append(got, file)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.files) {
				t.Errorf("Files = %v, want %v", got, tc.files)
			}
		})
	}
}

// Every way the inventory can fail is carried in the tree with the reason
// named: a caller that read no file must not hand the lint an empty tree.
func TestSpecTargetTreeCarriesAFailedRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T) (rigPath string, open treeOpener)
		want  string
	}{
		{
			name: "no branch on origin or locally",
			setup: func(t *testing.T) (string, treeOpener) {
				return rigAt(t), openTree(fakeTree{})
			},
			want: "cannot read main in",
		},
		{
			name: "no repo under the rig",
			setup: func(t *testing.T) (string, treeOpener) {
				return t.TempDir(), openTree(fakeTree{"origin/main": {"a.go"}})
			},
			want: "no git repo under",
		},
		{
			name: "a rig config that does not decode",
			setup: func(t *testing.T) (string, treeOpener) {
				rigPath := rigAt(t)
				if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte("{"), 0o644); err != nil {
					t.Fatal(err)
				}
				return rigPath, openTree(fakeTree{"origin/main": {"a.go"}})
			},
			want: "cannot read the rig's default branch",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rigPath, open := tc.setup(t)
			target := specTargetTree(rigPath, open)
			if target.Err == "" {
				t.Fatal("a failed inventory read as clean")
			}
			if !strings.Contains(target.Err, tc.want) {
				t.Errorf("Err = %q, want %q named", target.Err, tc.want)
			}
			if len(target.Files) != 0 {
				t.Errorf("Files = %v, want none", target.Files)
			}
		})
	}
}

// The tick hands the candidate's rig inventory to the lint: a bead whose
// Constraints name a file the target branch never got is held with the path
// named, in place of a seat (gt-7t1im).
func TestSpecDispatchHoldsABeadNamingAPathAbsentFromTheTarget(t *testing.T) {
	t.Parallel()
	body := "Change internal/cmd/bird.go."
	spec := cleanSpec("gt-absent", 2, "2026-09-29T12:00:00Z")
	spec.Description = specConstraints(&body)

	f := newFakeSpecTown(spec)
	f.targets = map[string]*specdispatch.TargetTree{
		"gastown": {Branch: "main", Files: map[string]struct{}{"internal/cmd/spec.go": {}}},
	}
	report := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 {
		t.Fatalf("slung %v, want the bead held", f.slung)
	}
	if len(report.Refused) != 1 || report.Refused[0].Bead != "gt-absent" {
		t.Fatalf("Refused = %+v, want gt-absent", report.Refused)
	}
	if line := report.Refused[0].Line; !strings.Contains(line, "internal/cmd/bird.go") || !strings.Contains(line, "absent from main") {
		t.Errorf("line = %q, want the path and the branch named", line)
	}
	if !slices.Contains(f.labels["gt-absent"], specShapeLabel) {
		t.Errorf("labels = %v, want %s", f.labels["gt-absent"], specShapeLabel)
	}
}

// A bead whose Constraints name only files the target branch holds keeps its
// seat.
func TestSpecDispatchSlingsABeadWhosePathsAreOnTheTarget(t *testing.T) {
	t.Parallel()
	body := "Change internal/cmd/spec.go."
	spec := cleanSpec("gt-present", 2, "2026-09-29T12:00:00Z")
	spec.Description = specConstraints(&body)

	f := newFakeSpecTown(spec)
	f.targets = map[string]*specdispatch.TargetTree{
		"gastown": {Branch: "main", Files: map[string]struct{}{"internal/cmd/spec.go": {}}},
	}
	report := runSpecDispatchCycle(f.env())
	if !slices.Equal(f.slung, []string{"gt-present"}) {
		t.Fatalf("slung %v, want gt-present; refused %+v", f.slung, report.Refused)
	}
}

// gt spec lint asks for the bead's rig inventory, and a town bead or a bead
// with no rig is held with the error named rather than passed as clean
// (gt-7t1im).
func TestSpecForLintCarriesTheBeadsTargetInventory(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n" + `{"prefix":"hq-","path":"."}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	show := func(id string) (specdispatch.Spec, error) { return cleanSpec(id, 2, "2026-09-29T12:00:00Z"), nil }
	tests := []struct {
		bead string
		want string
	}{
		{bead: "gt-bead", want: "no git repo under"},
		{bead: "hq-town", want: "no rig holds"},
		{bead: "zz-nowhere", want: "no rig holds"},
	}
	for _, tc := range tests {
		spec, err := specForLint(townRoot, tc.bead, show, openRigTree)
		if err != nil {
			t.Fatalf("specForLint(%s): %v", tc.bead, err)
		}
		if spec.Target == nil {
			t.Fatalf("specForLint(%s) asked for no inventory", tc.bead)
		}
		if !strings.Contains(spec.Target.Err, tc.want) {
			t.Errorf("specForLint(%s) target Err = %q, want %q named", tc.bead, spec.Target.Err, tc.want)
		}
	}
	// A bead that did not read is linted on that failure alone.
	readErr := errors.New("bead gt-gone not found")
	failed, err := specForLint(townRoot, "gt-gone", func(string) (specdispatch.Spec, error) { return specdispatch.Spec{}, readErr }, openRigTree)
	if !errors.Is(err, readErr) || failed.Target != nil {
		t.Errorf("specForLint of an unreadable bead = %+v, %v; want the read error and no inventory", failed, err)
	}
}

// A branch name from the rig's config reaches git only when it is one branch
// revision: a leading "-" is an ls-tree option and ".." a range, so both are
// held with the name reported and no git read made (gt-7t1im).
func TestSpecTargetTreeHoldsABranchNameGitWouldReadOtherwise(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		branch string
	}{
		{name: "a leading dash", branch: "-x"},
		{name: "a revision range", branch: "main..next"},
		{name: "a shell metacharacter", branch: "main;rm"},
		{name: "a revision suffix", branch: "main^"},
		{name: "a lock suffix", branch: "main.lock"},
		{name: "a trailing slash", branch: "release/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rigPath := rigAt(t)
			cfg := `{"type": "rig", "name": "rig", "default_branch": ` + strconv.Quote(tc.branch) + `}`
			if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			opened := false
			target := specTargetTree(rigPath, func(string) treeReader {
				opened = true
				return fakeTree{"origin/" + tc.branch: {"a.go"}}
			})
			if target.Err == "" || !strings.Contains(target.Err, tc.branch) || !strings.Contains(target.Err, "not a branch name") {
				t.Errorf("Err = %q, want %q named as not a branch name", target.Err, tc.branch)
			}
			if opened {
				t.Error("the repo was read with a branch name git would read as something else")
			}
			if len(target.Files) != 0 {
				t.Errorf("Files = %v, want none", target.Files)
			}
		})
	}
}

// A branch name a rig config legitimately uses still reads: the check refuses
// the forms git reads as something else, not every name with punctuation.
func TestSpecTargetTreeReadsAnOrdinaryBranchName(t *testing.T) {
	t.Parallel()
	rigPath := rigAt(t)
	cfg := `{"type": "rig", "name": "rig", "default_branch": "release/2026.10"}`
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	repoDir := ""
	target := specTargetTree(rigPath, func(dir string) treeReader {
		repoDir = dir
		return fakeTree{"origin/release/2026.10": {"internal/cmd/spec.go"}}
	})
	if target.Err != "" {
		t.Fatalf("Err = %q, want the inventory", target.Err)
	}
	if target.Branch != "release/2026.10" {
		t.Errorf("Branch = %q", target.Branch)
	}
	if _, ok := target.Files["internal/cmd/spec.go"]; !ok {
		t.Errorf("Files = %v, want internal/cmd/spec.go", target.Files)
	}
	if want := filepath.Join(rigPath, ".repo.git"); repoDir != want {
		t.Errorf("opened %q, want the rig's shared repo %q", repoDir, want)
	}
}

// specLintTarget maps a bead's prefix to its rig and hands the rig's shared
// repo to the opener: the shape gt spec lint reads a bead's paths against
// (gt-7t1im).
func TestSpecLintTargetResolvesTheBeadsRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	rig := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(filepath.Join(rig, ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	repoDir := ""
	target := specLintTarget(townRoot, "gt-bead", func(dir string) treeReader {
		repoDir = dir
		return fakeTree{"origin/main": {"internal/cmd/spec.go"}}
	})
	if target.Err != "" {
		t.Fatalf("Err = %q, want the rig's inventory", target.Err)
	}
	if want := filepath.Join(rig, ".repo.git"); repoDir != want {
		t.Errorf("opened %q, want %q", repoDir, want)
	}
	if _, ok := target.Files["internal/cmd/spec.go"]; !ok || target.Branch != "main" {
		t.Errorf("target = %+v, want origin/main's files under main", target)
	}
}

// The peer-hold answer asks the same path question the tick asks: a peer whose
// Constraints name a file its rig's branch never got is not "next", so the
// guard holds the cluster behind it (gt-7t1im).
func TestSpecPeerHeldFromHoldsAPeerNamingAPathAbsentFromTheTarget(t *testing.T) {
	t.Parallel()
	body := "Change internal/cmd/bird.go."
	spec := cleanSpec("gt-peer", 1, "2026-09-29T12:00:00Z")
	spec.Description = specConstraints(&body)
	peer := sling.Duplicate{
		ID: "gt-peer", Title: spec.Title, Status: "open", Priority: 1, IssueType: "task",
		Ready: sling.Readiness{Description: spec.Description, Acceptance: spec.Acceptance},
	}
	knobs := specHoldKnobs{
		MaxPriority: 2,
		ShapeGate:   specdispatch.ShapeGateRefuse,
		Template:    specdispatch.LoadTemplate(""),
		Warn:        io.Discard,
		Target: func(string) *specdispatch.TargetTree {
			return &specdispatch.TargetTree{Branch: "main", Files: map[string]struct{}{"internal/cmd/spec.go": {}}}
		},
	}
	knobs.Backoff = func(string) string { return "" }

	got := specPeerHeldFrom(beadsfake.New(), knobs, peer, nil)
	if !strings.Contains(got, "## Constraints") || !strings.Contains(got, "internal/cmd/bird.go") {
		t.Fatalf("peer held reason = %q, want the missing path named", got)
	}
	// The inventory answered: a peer held for a read the guard never wired
	// would name no branch, and the cluster would wait on the wrong question.
	if !strings.Contains(got, "absent from main") {
		t.Errorf("peer held reason = %q, want the branch the path is missing from", got)
	}
}
