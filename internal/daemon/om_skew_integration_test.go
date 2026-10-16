//go:build integration

package daemon

import (
	"path/filepath"
	"testing"
)

// TestIntegrationOMSkewEmbedRulesReadsRealGitGrep runs the embed probe against
// a real repository, so the output the parser reads is git's own rather than a
// capture that could have drifted from it. git prefixes every line with the
// revision it searched, and a parser that misses that recognizes no directive
// at all — the allowlist then dismisses a skew the directives say is real
// (gt-8n9hj).
func TestIntegrationOMSkewEmbedRulesReadsRealGitGrep(t *testing.T) {
	t.Parallel()
	d, _ := omSkewDaemon(t)
	clone := filepath.Join(d.config.TownRoot, rebuildOMSrcDir)
	mkdirs(t, clone, filepath.Join(clone, "docs"))
	lwGit(t, clone, "init", "-q", "-b", "main")
	lwGit(t, clone, "commit", "-q", "--allow-empty", "-m", "an om with no directive")
	empty := lwGit(t, clone, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(clone, "docs", "doc.go"), "package docs\n\n//go:embed gate.md\nvar gate string\n")
	writeFile(t, filepath.Join(clone, "docs", "gate.md"), "# gate\n")
	lwGit(t, clone, "add", ".")
	lwGit(t, clone, "commit", "-q", "-m", "a directive over an allowlisted path")
	rev := lwGit(t, clone, "rev-parse", "HEAD")

	// docs/gate.md is on the allowlist and a directive carries it into the
	// binary, so the skew stays. Reading no directive is what this catches:
	// the probe would answer that nothing reaches it.
	reaches, err := d.omSkewNoEmbedReaches(d.config.TownRoot, rev, []string{"docs/gate.md"})
	if err != nil {
		t.Fatalf("reading the embed directives of %s: %v", rev, err)
	}
	if reaches {
		t.Error("omSkewNoEmbedReaches(docs/gate.md) = true, want the directive to keep the alert")
	}
	if reaches, err := d.omSkewNoEmbedReaches(d.config.TownRoot, rev, []string{"README.md"}); err != nil || !reaches {
		t.Errorf("omSkewNoEmbedReaches(README.md) = %v, %v; want no directive over it", reaches, err)
	}

	// A revision carrying no directive is git grep's exit 1, an answer.
	rules, err := d.omSkewEmbedRules(d.config.TownRoot, empty)
	if err != nil {
		t.Fatalf("reading a tree with no directive: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("a tree with no directive gave %+v, want no rule", rules)
	}
}
