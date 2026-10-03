package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrimeContentMatchesTrackedPRIME pins the provisioned fallback to the
// rig's tracked PRIME.md, which are two copies of one document. They drifted
// silently once already: .beads/PRIME.md gained "Filing a work bead" in
// f214b34f and primeContent did not (gt-lq5kw). Edit both to the same text.
func TestPrimeContentMatchesTrackedPRIME(t *testing.T) {
	tracked := filepath.Join(repoRootFromCaller(t), ".beads", "PRIME.md")
	want, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatalf("read tracked %s: %v", tracked, err)
	}
	if string(want) == primeContent {
		return
	}
	wantLines := strings.Split(string(want), "\n")
	gotLines := strings.Split(primeContent, "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		w, g := lineAt(wantLines, i), lineAt(gotLines, i)
		if w != g {
			t.Fatalf(`.beads/PRIME.md line %d and primeContent (internal/beads/beads.go) disagree (gt-lq5kw):
  PRIME.md:     %q
  primeContent: %q
Edit both to the same text. ProvisionPrimeMD writes primeContent only into a rig that has no PRIME.md, so a divergence hands the two readers different instructions.`,
				i+1, w, g)
		}
	}
}

// TestPrimeContentKeepsGTDone guards the close protocol a re-sync could quietly
// drop. The fallback exists for a polecat whose SessionStart hook failed, and
// `gt done` is that worker's only way to signal completion (gt-lq5kw).
func TestPrimeContentKeepsGTDone(t *testing.T) {
	closeAt := strings.Index(primeContent, "## Session Close Protocol")
	if closeAt < 0 {
		t.Fatal("primeContent has no Session Close Protocol section (gt-lq5kw)")
	}
	if !strings.Contains(primeContent[closeAt:], "`gt done`") {
		t.Error("primeContent's Session Close Protocol does not name `gt done`; a polecat reading the fallback would not know how to signal completion (gt-lq5kw)")
	}
}

// lineAt returns the line, or a marker when the document ended first.
func lineAt(lines []string, i int) string {
	if i >= len(lines) {
		return "<end of file>"
	}
	return lines[i]
}
