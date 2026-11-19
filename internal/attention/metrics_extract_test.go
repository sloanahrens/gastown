package attention

import "testing"

// TestMetricsExtractRules is the metrics extract's place in the tables, one row
// per key the collector raises: the kind is listed for readers, every key
// resolves to metrics.extract, the rule is a record that wakes no one, and the
// overseer is the one who looks at it (gt-mi4cem).
func TestMetricsExtractRules(t *testing.T) {
	t.Parallel()

	listed := make(map[Kind]bool, len(Kinds()))
	for _, k := range Kinds() {
		listed[k] = true
	}
	if !listed[KindMetricsExtract] {
		t.Errorf("Kinds() does not list %q, so readers and the rule check skip it", KindMetricsExtract)
	}

	for _, key := range []string{
		"metrics-extract:stale",
		"metrics-extract:gaps",
		"metrics-extract:unreadable",
	} {
		t.Run(key, func(t *testing.T) {
			it := Item{Kind: KindMetricsExtract, Key: key}
			if got := RuleFor(it); got != RuleMetricsExtract {
				t.Fatalf("RuleFor(%q) = %q, want %q", key, got, RuleMetricsExtract)
			}
			f, ok := Flags(RuleMetricsExtract)
			if !ok {
				t.Fatalf("Flags(%q) has no row", RuleMetricsExtract)
			}
			if f.Wake || f.Handled != HandledInfo {
				t.Errorf("Flags(%q) = %+v, want info and no wake", RuleMetricsExtract, f)
			}
			if owner := RuleOwner(RuleMetricsExtract); owner != OwnerOverseer {
				t.Errorf("RuleOwner(%q) = %q, want %q", RuleMetricsExtract, owner, OwnerOverseer)
			}
		})
	}
}
