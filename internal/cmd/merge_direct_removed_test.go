package cmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergeFlagRejectsDirect: the --merge direct convoy strategy landed on
// main from gt done with no gate, no om review and no merge slot (G2-02) and
// never landed anything (0 direct convoys in all hq history). It is deleted
// (gt-fcxe9.4); mr and local stay.
func TestMergeFlagRejectsDirect(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "mr", "local"} {
		if err := validateConvoyMergeFlag(ok); err != nil {
			t.Errorf("validateConvoyMergeFlag(%q) = %v", ok, err)
		}
	}
	err := validateConvoyMergeFlag("direct")
	if err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("validateConvoyMergeFlag(direct) = %v, want a refusal saying it was removed", err)
	}
	for _, path := range []string{"gt sling", "gt convoy create"} {
		c := findCommand(t, path)
		if f := c.Flags().Lookup("merge"); f == nil || strings.Contains(f.Usage, "direct") {
			t.Errorf("%s --merge usage still offers direct: %v", path, f)
		}
		if strings.Contains(c.Long, "--merge=direct") {
			t.Errorf("%s help still documents --merge=direct", path)
		}
	}
}

// TestDirectMergePlumbingIsGone: no non-test Go under internal/ keeps the
// direct-merge landing, its pre-push trust signal or its MR label. The two
// gt:owned-direct reads in internal/refinery stay: they sit in the batch
// engine's merge gates, which gt-fcxe9.4 leaves alone (the engine is deleted
// whole at un-park, gt-v4ssj.6), and nothing sets the label any more.
func TestDirectMergePlumbingIsGone(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..")
	banned := []string{"GT_DONE_DIRECT_MERGE", "EnvDoneDirectMerge", "gt:owned-direct", "IsOwnedDirect", "SkipMergeFlow", `MergeStrategy == "direct"`, "doneDirectMergeSkipReason"}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, b := range banned {
			if b == "gt:owned-direct" && strings.HasPrefix(filepath.ToSlash(path), "../refinery/") {
				continue
			}
			if strings.Contains(string(data), b) {
				t.Errorf("%s still contains %q", path, b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
