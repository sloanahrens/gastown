//go:build integration

package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/conflict"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/land"
)

// jevGit runs git in dir with a fixed identity and returns its trimmed output.
func jevGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out))
}

// jevCommitFile writes a.go with body, commits it, and returns the commit.
func jevCommitFile(t *testing.T, dir, body, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	jevGit(t, dir, "add", "-A")
	jevGit(t, dir, "commit", "-q", "-m", msg)
	return jevGit(t, dir, "rev-parse", "HEAD")
}

// The report reads the real adapter through the production opener, on a real
// bare clone. A clone that holds no landing's commits leaves the pair
// unlabeled, and the report itself never fetches. Once the clone holds the
// commits, the same report labels the pair from their changed files.
func TestIntegrationJevConflictReportReadsTheCloneWithoutFetching(t *testing.T) {
	town := t.TempDir()
	origin := t.TempDir()
	jevGit(t, origin, "init", "-q", "-b", "main")
	base := jevCommitFile(t, origin, "base\n", "base")
	jevGit(t, origin, "checkout", "-q", "-b", "bead-x", base)
	headX := jevCommitFile(t, origin, "x\n", "x changes a.go")
	jevGit(t, origin, "checkout", "-q", "-b", "bead-y", base)
	headY := jevCommitFile(t, origin, "y\n", "y changes a.go")

	mayorDir := filepath.Dir(constants.MayorRigsPath(town))
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{submission.RigName: {GitURL: origin}}}
	raw, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(constants.MayorRigsPath(town), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	clone := submission.ClonePath(town)
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	jevGit(t, clone, "init", "-q", "--bare")

	landedAt := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	writeJevConflictLandings(t, town,
		land.LandingRecord{BeadID: "x", Rig: conflict.RigName, Base: base, Head: headX, LandedCommit: headX, LandedAt: landedAt},
		land.LandingRecord{BeadID: "y", Rig: conflict.RigName, Base: base, Head: headY, LandedCommit: headY, LandedAt: landedAt},
	)
	writeJevConflictLog(t, town, jevConflictLogLine(t, "x", "y", time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), 0.9))

	// The clone holds no branch yet, so the pair's commits are absent.
	rep := runJevConflictJSON(t, town, scorecard.OpenReadOnly, jevConflictLandings)
	cut := jevConflictCutAt(t, rep, 0.6)
	if cut.Total.Pairs != 1 || cut.Total.Unlabeled != 1 || cut.Total.Labeled != 0 {
		t.Fatalf("with the commits absent, cut 0.6 total = pairs %d, unlabeled %d, labeled %d; want the pair unlabeled",
			cut.Total.Pairs, cut.Total.Unlabeled, cut.Total.Labeled)
	}
	if refs := jevGit(t, clone, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatalf("the report fetched branches into the clone:\n%s", refs)
	}

	// The test puts the commits in the clone itself, as the landing worker's
	// fetch would. The report then labels the pair from their files.
	jevGit(t, clone, "fetch", "-q", origin, "+refs/heads/*:refs/heads/*")
	rep = runJevConflictJSON(t, town, scorecard.OpenReadOnly, jevConflictLandings)
	cut = jevConflictCutAt(t, rep, 0.6)
	if cut.Total.Pairs != 1 || cut.Total.Labeled != 1 || cut.Total.Overlapped != 1 {
		t.Fatalf("with the commits present, cut 0.6 total = pairs %d, labeled %d, overlapped %d; want the pair labeled as overlapping",
			cut.Total.Pairs, cut.Total.Labeled, cut.Total.Overlapped)
	}
}
