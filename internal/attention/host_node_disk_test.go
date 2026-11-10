package attention

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The control node's disk rules are the town root's rules over the node's
// image filesystem: each item is keyed by its rule, so a reader and an ack find
// the rule by the name the daemon raised it under. The warning is for the next
// look and the critical wakes, as the town root's are.
func TestHostNodeDiskRules(t *testing.T) {
	t.Parallel()

	for _, rule := range []string{RuleHostNodeDiskWarn, RuleHostNodeDiskCrit} {
		if got := RuleFor(Item{Kind: KindHostNodeDisk, Key: rule}); got != rule {
			t.Errorf("RuleFor(node disk item keyed %q) = %q, want the key", rule, got)
		}
		if got := RuleOwner(rule); got != OwnerOperator {
			t.Errorf("RuleOwner(%q) = %q, want %q", rule, got, OwnerOperator)
		}
	}
	if f, ok := Flags(RuleHostNodeDiskCrit); !ok || !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("%s = %+v, want operator_action and wake", RuleHostNodeDiskCrit, f)
	}
	if f, ok := Flags(RuleHostNodeDiskWarn); !ok || f.Wake || f.Handled != HandledInfo {
		t.Errorf("%s = %+v, want info and no wake", RuleHostNodeDiskWarn, f)
	}
	found := false
	for _, k := range Kinds() {
		if k == KindHostNodeDisk {
			found = true
		}
	}
	if !found {
		t.Errorf("Kinds() does not list %q, so readers and the rule check skip it", KindHostNodeDisk)
	}
}

// The reference documents the control node's disk rules beside the town root's:
// the block that names host.disk_warn also names both node rules, so a reader
// finds the two disks' rules together.
func TestReferenceDocumentsHostNodeDiskRules(t *testing.T) {
	t.Parallel()

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference.md"))
	if err != nil {
		t.Fatalf("reading the reference: %v", err)
	}
	var block string
	for _, b := range strings.Split(string(doc), "\n\n") {
		if strings.Contains(b, "`"+RuleHostDiskWarn+"`") {
			block = b
			break
		}
	}
	if block == "" {
		t.Fatalf("the reference does not name %s", RuleHostDiskWarn)
	}
	for _, rule := range []string{RuleHostNodeDiskWarn, RuleHostNodeDiskCrit} {
		if !strings.Contains(block, "`"+rule+"`") {
			t.Errorf("the block that names %s does not name %s", RuleHostDiskWarn, rule)
		}
	}
}
