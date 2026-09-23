package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

const restampBaseRubric = `{
  "threshold": 0.6,
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."}
  ]
}`

const restampChangedRubric = `{
  "threshold": 0.1,
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."},
    {"name": "docs-and-comments", "weight": 2, "guidance": "Docs follow the writing rules."}
  ]
}`

// initRubricRestampRepo builds a real origin+clone with a base commit
// carrying restampBaseRubric, then fast-forwards main onto a branch commit
// that either touches .om.json (touchRubric) or an unrelated file. The
// fast-forward keeps the landed commit single-parent, the simplest shape
// ResolveLandedRange resolves. rubricAt, when non-empty, additionally seeds
// the rubric file at that repo-relative path (used for the absolute-path
// resolution test, whose manifest declares an absolute rubric path).
func initRubricRestampRepo(t *testing.T, touchRubric bool) (clone, landedHead string) {
	t.Helper()
	tmp := t.TempDir()
	originPath := filepath.Join(tmp, "origin.git")
	runRubricRestampGit(t, tmp, "init", "--bare", "-b", "main", originPath)

	clone = filepath.Join(tmp, "clone")
	runRubricRestampGit(t, tmp, "clone", originPath, clone)
	runRubricRestampGit(t, clone, "config", "user.email", "polecat@example.com")
	runRubricRestampGit(t, clone, "config", "user.name", "Polecat Test")

	writeRubricRestampFile(t, clone, ".om.json", restampBaseRubric)
	runRubricRestampGit(t, clone, "add", "-A")
	runRubricRestampGit(t, clone, "commit", "-m", "seed rubric")
	runRubricRestampGit(t, clone, "push", "-u", "origin", "main")

	runRubricRestampGit(t, clone, "checkout", "-b", "polecat/marble/gt-restamp")
	if touchRubric {
		writeRubricRestampFile(t, clone, ".om.json", restampChangedRubric)
	} else {
		writeRubricRestampFile(t, clone, "feature.txt", "unrelated\n")
	}
	runRubricRestampGit(t, clone, "add", "-A")
	runRubricRestampGit(t, clone, "commit", "-m", "MR work")
	runRubricRestampGit(t, clone, "push", "-u", "origin", "polecat/marble/gt-restamp")

	runRubricRestampGit(t, clone, "checkout", "main")
	runRubricRestampGit(t, clone, "merge", "--ff-only", "polecat/marble/gt-restamp")
	runRubricRestampGit(t, clone, "push", "origin", "main")

	landedHead = runRubricRestampGit(t, clone, "rev-parse", "origin/main")
	return clone, landedHead
}

func runRubricRestampGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeRubricRestampFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestDetectRubricChangeAfterMerge_TouchedReportsLandedSHA is the gt-7bvf
// post-merge repro: an MR that changes .om.json lands, and the caller learns
// the landed content's sha256 so it can escalate to the operator — but the
// manifest on disk must be left exactly as LoadManifest found it (mayor
// design decision: a rubric change never self-deploys).
func TestDetectRubricChangeAfterMerge_TouchedReportsLandedSHA(t *testing.T) {
	clone, head := initRubricRestampRepo(t, true)
	rigDir := t.TempDir()
	baseSum := sha256.Sum256([]byte(restampBaseRubric))
	baseSHA := hex.EncodeToString(baseSum[:])
	manifest := &editorial.Manifest{}
	manifest.Rubric.Path = ".om.json"
	manifest.Rubric.SHA256 = baseSHA
	if err := editorial.SaveManifest(rigDir, manifest); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	mr := &refinery.MergeRequest{ID: "gt-mr-1", TargetBranch: "main"}
	touched, rel, sha, err := detectRubricChangeAfterMerge(rigDir, git.NewGit(clone), mr, head)
	if err != nil {
		t.Fatalf("detectRubricChangeAfterMerge: %v", err)
	}
	if !touched {
		t.Fatal("touched = false, want true for a rubric-touching merge")
	}
	if rel != ".om.json" {
		t.Errorf("rel = %q, want .om.json", rel)
	}

	landedRubric := runRubricRestampGit(t, clone, "show", head+":.om.json")
	wantSum := sha256.Sum256([]byte(landedRubric))
	want := hex.EncodeToString(wantSum[:])
	if sha != want {
		t.Errorf("sha = %s, want %s (the landed content's own hash)", sha, want)
	}

	// The manifest must be untouched — this function never writes.
	reloaded, err := editorial.LoadManifest(rigDir)
	if err != nil {
		t.Fatalf("LoadManifest after detect: %v", err)
	}
	if reloaded.Rubric.SHA256 != baseSHA {
		t.Errorf("manifest on disk carries rubric sha %s, want unchanged %s (detection must never restamp)", reloaded.Rubric.SHA256, baseSHA)
	}
}

// TestDetectRubricChangeAfterMerge_UntouchedIsNoOp: an MR that never touches
// .om.json reports no change.
func TestDetectRubricChangeAfterMerge_UntouchedIsNoOp(t *testing.T) {
	clone, head := initRubricRestampRepo(t, false)
	rigDir := t.TempDir()
	baseSum := sha256.Sum256([]byte(restampBaseRubric))
	wantSHA := hex.EncodeToString(baseSum[:])
	manifest := &editorial.Manifest{}
	manifest.Rubric.Path = ".om.json"
	manifest.Rubric.SHA256 = wantSHA
	if err := editorial.SaveManifest(rigDir, manifest); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	mr := &refinery.MergeRequest{ID: "gt-mr-1", TargetBranch: "main"}
	touched, _, sha, err := detectRubricChangeAfterMerge(rigDir, git.NewGit(clone), mr, head)
	if err != nil {
		t.Fatalf("detectRubricChangeAfterMerge: %v", err)
	}
	if touched || sha != "" {
		t.Errorf("touched=%v sha=%q, want false/empty: this merge never touched the rubric", touched, sha)
	}
}

// TestDetectRubricChangeAfterMerge_NoRubricConfiguredIsNoOp: a rig with no
// rubric in its manifest has nothing to detect.
func TestDetectRubricChangeAfterMerge_NoRubricConfiguredIsNoOp(t *testing.T) {
	clone, head := initRubricRestampRepo(t, true)
	rigDir := t.TempDir()
	if err := editorial.SaveManifest(rigDir, &editorial.Manifest{}); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	mr := &refinery.MergeRequest{ID: "gt-mr-1", TargetBranch: "main"}
	touched, _, sha, err := detectRubricChangeAfterMerge(rigDir, git.NewGit(clone), mr, head)
	if err != nil {
		t.Fatalf("detectRubricChangeAfterMerge: %v", err)
	}
	if touched || sha != "" {
		t.Errorf("touched=%v sha=%q, want false/empty: the rig declares no rubric", touched, sha)
	}
}

// TestDetectRubricChangeAfterMerge_NoManifestIsNoOp: a rig with no harness
// manifest deployed at all (e.g. a test fixture, or a rig predating the
// harness) is not this function's failure to report.
func TestDetectRubricChangeAfterMerge_NoManifestIsNoOp(t *testing.T) {
	clone, head := initRubricRestampRepo(t, true)
	mr := &refinery.MergeRequest{ID: "gt-mr-1", TargetBranch: "main"}
	touched, _, sha, err := detectRubricChangeAfterMerge(t.TempDir(), git.NewGit(clone), mr, head)
	if err != nil {
		t.Fatalf("detectRubricChangeAfterMerge: %v", err)
	}
	if touched || sha != "" {
		t.Errorf("touched=%v sha=%q, want false/empty: no manifest is deployed", touched, sha)
	}
}

// TestDetectRubricChangeAfterMerge_AbsoluteRubricPathIsResolved is the
// path-resolution fix: deploy.sh's documented absolute usage
// (--rubric <rig>/refinery/rig/.om.json) must still be detected. rigDir
// here plays the rig root, with the git working tree at rigDir/refinery/rig
// (a copy of the landed clone) so the absolute manifest path actually
// resolves inside it.
func TestDetectRubricChangeAfterMerge_AbsoluteRubricPathIsResolved(t *testing.T) {
	clone, head := initRubricRestampRepo(t, true)
	rigDir := t.TempDir()
	repoRoot := filepath.Join(rigDir, "refinery", "rig")
	// detectRubricChangeAfterMerge reads git history via rigGit (pointed at
	// clone below), and resolves the absolute rubric path against
	// rigDir/refinery/rig purely as a path, independent of that directory's
	// own content — so a stand-in directory is enough.
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("mkdir repo root: %v", err)
	}

	baseSum := sha256.Sum256([]byte(restampBaseRubric))
	manifest := &editorial.Manifest{}
	manifest.Rubric.Path = filepath.Join(repoRoot, ".om.json")
	manifest.Rubric.SHA256 = hex.EncodeToString(baseSum[:])
	if err := editorial.SaveManifest(rigDir, manifest); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	mr := &refinery.MergeRequest{ID: "gt-mr-1", TargetBranch: "main"}
	touched, rel, sha, err := detectRubricChangeAfterMerge(rigDir, git.NewGit(clone), mr, head)
	if err != nil {
		t.Fatalf("detectRubricChangeAfterMerge: %v", err)
	}
	if !touched {
		t.Fatal("touched = false, want true: an absolute rubric path must still be detected against the repo root, not the rig root")
	}
	if rel != ".om.json" {
		t.Errorf("rel = %q, want .om.json", rel)
	}
	if sha == "" {
		t.Error("sha is empty, want the landed rubric's hash")
	}
}

// TestRunMQPostMerge_RubricChangeEscalatesInsteadOfRestamping is the
// mayor's gt-7bvf acceptance test: a merge that lowers the rubric threshold
// escalates to the operator via `gt escalate`, and the harness manifest is
// left untouched — never restamped automatically.
func TestRunMQPostMerge_RubricChangeEscalatesInsteadOfRestamping(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	clone, head := initRubricRestampRepo(t, true)
	rigDir := t.TempDir()
	baseSum := sha256.Sum256([]byte(restampBaseRubric))
	baseSHA := hex.EncodeToString(baseSum[:])
	manifest := &editorial.Manifest{}
	manifest.Rubric.Path = ".om.json"
	manifest.Rubric.SHA256 = baseSHA
	if err := editorial.SaveManifest(rigDir, manifest); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	binDir := t.TempDir()
	gtLog := filepath.Join(t.TempDir(), "gt-args.log")
	gtScript := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> \"$GT_ARGS_LOG\"\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0o755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GT_ARGS_LOG", gtLog)

	mr := &refinery.MergeRequest{ID: "gt-mr-threshold", TargetBranch: "main"}
	touched, rel, sha, err := detectRubricChangeAfterMerge(rigDir, git.NewGit(clone), mr, head)
	if err != nil {
		t.Fatalf("detectRubricChangeAfterMerge: %v", err)
	}
	if !touched {
		t.Fatal("touched = false, want true: this MR lowered the threshold and added a criterion")
	}
	escalateRubricChange("test-rig", mr.TargetBranch, rel, sha, mr.ID)

	gtCalls := readFile(t, gtLog)
	if !strings.Contains(gtCalls, "escalate") {
		t.Fatalf("gt escalate was not invoked, gt log:\n%s", gtCalls)
	}
	if !strings.Contains(gtCalls, "rubric changed on main") {
		t.Fatalf("escalation message missing rubric-change context, gt log:\n%s", gtCalls)
	}

	// The manifest must still carry the OLD (pre-merge) sha: no restamp.
	reloaded, err := editorial.LoadManifest(rigDir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if reloaded.Rubric.SHA256 != baseSHA {
		t.Errorf("manifest rubric sha = %s, want unchanged %s (no auto-restamp)", reloaded.Rubric.SHA256, baseSHA)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
