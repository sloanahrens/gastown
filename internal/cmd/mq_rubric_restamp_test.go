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
// (--rubric <rig>/refinery/rig/.om.json) must still be detected.
// detectRubricChangeAfterMerge resolves the manifest path purely as a
// string against rigDir/refinery/rig, so no repo needs to exist there.
func TestDetectRubricChangeAfterMerge_AbsoluteRubricPathIsResolved(t *testing.T) {
	clone, head := initRubricRestampRepo(t, true)
	rigDir := t.TempDir()

	baseSum := sha256.Sum256([]byte(restampBaseRubric))
	manifest := &editorial.Manifest{}
	manifest.Rubric.Path = filepath.Join(rigDir, "refinery", "rig", ".om.json")
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

// TestDetectRubricChangeAfterMerge_AbsoluteRubricPathUnderSiblingCloneIsResolved
// is the om-review finding this attempt fixes: the gastown rig deploys with
// --rubric under mayor/rig, but detectRubricChangeAfterMerge always resolves
// against rigDir/refinery/rig. Both clones of the same rig track the same
// files at the same paths, so the manifest path must still resolve even
// though it names the OTHER clone (gt-7bvf).
func TestDetectRubricChangeAfterMerge_AbsoluteRubricPathUnderSiblingCloneIsResolved(t *testing.T) {
	clone, head := initRubricRestampRepo(t, true)
	rigDir := t.TempDir()

	baseSum := sha256.Sum256([]byte(restampBaseRubric))
	manifest := &editorial.Manifest{}
	manifest.Rubric.Path = filepath.Join(rigDir, "mayor", "rig", ".om.json")
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
		t.Fatal("touched = false, want true: an absolute rubric path under mayor/rig must still be detected even though this resolves against refinery/rig")
	}
	if rel != ".om.json" {
		t.Errorf("rel = %q, want .om.json", rel)
	}
	if sha == "" {
		t.Error("sha is empty, want the landed rubric's hash")
	}
}

// TestDetectRubricChangeAfterMerge_UnresolvableRubricPathFailsClosed: a
// manifest whose rubric path resolves under neither rigDir/refinery/rig nor
// a sibling rig clone must be reported as touched, never as untouched — an
// unresolvable path can silently disable every rubric protection (gt-7bvf).
func TestDetectRubricChangeAfterMerge_UnresolvableRubricPathFailsClosed(t *testing.T) {
	clone, head := initRubricRestampRepo(t, true)
	rigDir := t.TempDir()

	baseSum := sha256.Sum256([]byte(restampBaseRubric))
	manifest := &editorial.Manifest{}
	manifest.Rubric.Path = filepath.FromSlash("/etc/somewhere/.om.json")
	manifest.Rubric.SHA256 = hex.EncodeToString(baseSum[:])
	if err := editorial.SaveManifest(rigDir, manifest); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	mr := &refinery.MergeRequest{ID: "gt-mr-1", TargetBranch: "main"}
	touched, _, _, err := detectRubricChangeAfterMerge(rigDir, git.NewGit(clone), mr, head)
	if err == nil {
		t.Fatal("detectRubricChangeAfterMerge returned nil error for an unresolvable rubric path, want a fail-closed error")
	}
	if !touched {
		t.Error("touched = false, want true: an unresolvable rubric path must fail closed")
	}
}

// TestDetectRubricChangeAfterMerge_CorruptManifestReturnsError: a manifest
// file that exists but fails to parse must be reported as an error, not
// silently treated the same as no manifest deployed at all.
func TestDetectRubricChangeAfterMerge_CorruptManifestReturnsError(t *testing.T) {
	clone, head := initRubricRestampRepo(t, true)
	rigDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rigDir, ".gastown-harness-manifest.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt manifest: %v", err)
	}

	mr := &refinery.MergeRequest{ID: "gt-mr-1", TargetBranch: "main"}
	touched, _, _, err := detectRubricChangeAfterMerge(rigDir, git.NewGit(clone), mr, head)
	if err == nil {
		t.Fatal("detectRubricChangeAfterMerge returned nil error for a corrupt manifest, want an error")
	}
	if touched {
		t.Error("touched = true, want false: a corrupt manifest carries no rubric path to check")
	}
}

// fakeExecutable writes a shell script at binDir/name that appends its args
// to logPath, one call per line, and exits 0.
func fakeExecutable(t *testing.T, binDir, name, logPath string) {
	t.Helper()
	script := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> " + shellQuote(logPath) + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestHandlePostMergeRubricChange_TouchedEscalatesInsteadOfRestamping is the
// runMQPostMerge wiring this attempt adds a test for: a merge that lowers
// the rubric threshold escalates to the operator via `gt escalate`, records
// rubric_changed_escalated on the MR bead via `bd`, and never restamps the
// harness manifest.
func TestHandlePostMergeRubricChange_TouchedEscalatesInsteadOfRestamping(t *testing.T) {
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
	bdLog := filepath.Join(t.TempDir(), "bd-args.log")
	fakeExecutable(t, binDir, "gt", gtLog)
	fakeExecutable(t, binDir, "bd", bdLog)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mr := &refinery.MergeRequest{ID: "gt-mr-threshold", TargetBranch: "main"}
	handlePostMergeRubricChange(rigDir, "test-rig", git.NewGit(clone), mr, head)

	gtCalls := readFile(t, gtLog)
	if !strings.Contains(gtCalls, "escalate") {
		t.Fatalf("gt escalate was not invoked, gt log:\n%s", gtCalls)
	}
	if !strings.Contains(gtCalls, "rubric changed on main") {
		t.Fatalf("escalation message missing rubric-change context, gt log:\n%s", gtCalls)
	}
	bdCalls := readFile(t, bdLog)
	if !strings.Contains(bdCalls, "rubric_changed_escalated") {
		t.Fatalf("bd comments add did not record rubric_changed_escalated, bd log:\n%s", bdCalls)
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

// TestHandlePostMergeRubricChange_UnresolvablePathStillEscalates is the
// fail-closed wiring: a detection error from an unresolvable rubric path
// must still trigger the operator escalation, not just a warning — the
// warning alone is what silently disabled every rubric protection before
// this attempt (gt-7bvf).
func TestHandlePostMergeRubricChange_UnresolvablePathStillEscalates(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	clone, head := initRubricRestampRepo(t, true)
	rigDir := t.TempDir()
	baseSum := sha256.Sum256([]byte(restampBaseRubric))
	manifest := &editorial.Manifest{}
	manifest.Rubric.Path = filepath.FromSlash("/etc/somewhere/.om.json")
	manifest.Rubric.SHA256 = hex.EncodeToString(baseSum[:])
	if err := editorial.SaveManifest(rigDir, manifest); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	binDir := t.TempDir()
	gtLog := filepath.Join(t.TempDir(), "gt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd-args.log")
	fakeExecutable(t, binDir, "gt", gtLog)
	fakeExecutable(t, binDir, "bd", bdLog)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mr := &refinery.MergeRequest{ID: "gt-mr-unresolvable", TargetBranch: "main"}
	handlePostMergeRubricChange(rigDir, "test-rig", git.NewGit(clone), mr, head)

	gtCalls := readFile(t, gtLog)
	if !strings.Contains(gtCalls, "escalate") {
		t.Fatalf("gt escalate was not invoked for an unresolvable rubric path, gt log:\n%s", gtCalls)
	}
}

// TestHandlePostMergeRubricChange_UntouchedDoesNotEscalate: a merge that
// never touched the rubric must not invoke `gt escalate` at all.
func TestHandlePostMergeRubricChange_UntouchedDoesNotEscalate(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	clone, head := initRubricRestampRepo(t, false)
	rigDir := t.TempDir()
	baseSum := sha256.Sum256([]byte(restampBaseRubric))
	manifest := &editorial.Manifest{}
	manifest.Rubric.Path = ".om.json"
	manifest.Rubric.SHA256 = hex.EncodeToString(baseSum[:])
	if err := editorial.SaveManifest(rigDir, manifest); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	binDir := t.TempDir()
	gtLog := filepath.Join(t.TempDir(), "gt-args.log")
	fakeExecutable(t, binDir, "gt", gtLog)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mr := &refinery.MergeRequest{ID: "gt-mr-untouched", TargetBranch: "main"}
	handlePostMergeRubricChange(rigDir, "test-rig", git.NewGit(clone), mr, head)

	if gtCalls := readFile(t, gtLog); strings.Contains(gtCalls, "escalate") {
		t.Fatalf("gt escalate was invoked for a merge that never touched the rubric, gt log:\n%s", gtCalls)
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
