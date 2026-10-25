package migrate

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestDepTargetTakesTheFirstNonEmptyColumn(t *testing.T) {
	t.Parallel() // unit tier: pure
	for name, tc := range map[string]struct{ issue, wisp, ext, want string }{
		"issue":      {"gt-a", "", "", "gt-a"},
		"wisp":       {"", "gt-wisp-b", "", "gt-wisp-b"},
		"external":   {"", "", "be-9yi", "be-9yi"},
		"all empty":  {"", "", "", ""},
		"issue wins": {"gt-a", "gt-wisp-b", "be-1", "gt-a"},
	} {
		if got := depTarget(tc.issue, tc.wisp, tc.ext); got != tc.want {
			t.Errorf("%s: depTarget = %q, want %q", name, got, tc.want)
		}
	}
}

func TestParentsComeFromParentChildLinks(t *testing.T) {
	t.Parallel()
	deps := []DepRow{
		{IssueID: "gt-k1", DependsOn: "gt-p", Type: "parent-child"},
		{IssueID: "gt-k2", DependsOn: "gt-p", Type: "parent-child"},
		{IssueID: "gt-x", DependsOn: "gt-y", Type: "blocks"},
		{IssueID: "gt-wisp-s", DependsOn: "gt-wisp-m", Type: "parent-child"},
	}
	got := parentsOf(deps)
	want := map[string]string{"gt-k1": "gt-p", "gt-k2": "gt-p", "gt-wisp-s": "gt-wisp-m"}
	if len(got) != len(want) {
		t.Fatalf("parentsOf = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parent of %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestSplitConfigSendsMemoriesToKV(t *testing.T) {
	t.Parallel()
	cfg, kv := splitConfig(map[string]string{
		"issue_prefix": "gt", "compact_batch_size": "50", "kv.archive.gt.feedback.x": "text", "kv.memory.y": "mem",
	})
	if len(cfg) != 2 || cfg["issue_prefix"] != "gt" {
		t.Errorf("config = %v, want issue_prefix and compact_batch_size", cfg)
	}
	if len(kv) != 2 || kv["kv.archive.gt.feedback.x"] != "text" || kv["kv.memory.y"] != "mem" {
		t.Errorf("kv = %v, want the two kv.* keys with the prefix kept", kv)
	}
}

func TestRigsOfIDs(t *testing.T) {
	t.Parallel()
	got := rigsOf([]string{"gt-a", "gt-wisp-b", "hq-c", "gt-a.1", "weird", ""})
	if !slices.Equal(got, []string{"gt", "hq"}) {
		t.Errorf("rigsOf = %v, want [gt hq] sorted and de-duplicated, ids with no prefix skipped", got)
	}
}

func TestLegacyMetadataKeepsOnlyWhatIsSet(t *testing.T) {
	t.Parallel()
	b := legacyMetadata(map[string]any{"row_lock": "abc", "content_hash": "h", "spec_id": "", "pinned": false, "compaction_level": int64(0), "work_type": "bug"})
	var got map[string]map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	legacy := got["legacy"]
	if len(legacy) != 3 || legacy["row_lock"] != "abc" || legacy["content_hash"] != "h" || legacy["work_type"] != "bug" {
		t.Errorf("legacy = %v, want exactly row_lock, content_hash and work_type", legacy)
	}
	if string(legacyMetadata(map[string]any{"spec_id": ""})) != "{}" {
		t.Errorf("nothing set must be {}, got %s", legacyMetadata(map[string]any{"spec_id": ""}))
	}
}
