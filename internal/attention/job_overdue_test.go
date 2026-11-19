package attention

import "testing"

// TestJobOverdueRules is the job-overdue kind's two rules, one table row per key
// shape the collector and the wake collector raise: the wake key resolves to
// job.overdue_wake and every other key to job.overdue, each carries its flags
// row, and both belong to the overseer to restart or repair the job
// (gt-toiff6, gt-obe5qp).
func TestJobOverdueRules(t *testing.T) {
	t.Parallel()

	listed := make(map[Kind]bool, len(Kinds()))
	for _, k := range Kinds() {
		listed[k] = true
	}
	if !listed[KindJobOverdue] {
		t.Errorf("Kinds() does not list %q, so readers and the rule checks skip it", KindJobOverdue)
	}

	for _, tc := range []struct {
		name string
		key  string
		rule string
		flag RuleFlags
	}{
		{"wake key", "job-overdue:wake:dolt_gc", RuleJobOverdueWake, flagsWake},
		{"second wake key", "job-overdue:wake:dolt_backup", RuleJobOverdueWake, flagsWake},
		{"overdue key", "job-overdue:dolt_gc", RuleJobOverdue, flagsInfo},
		{"second overdue key", "job-overdue:cache_trim", RuleJobOverdue, flagsInfo},
		// The wake prefix ends at the colon: a name that only starts with the
		// letters is still the job's info item, not a second job's wake.
		{"name without the colon", "job-overdue:waker", RuleJobOverdue, flagsInfo},
		{"no key", "", RuleJobOverdue, flagsInfo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			it := Item{Kind: KindJobOverdue, Key: tc.key}
			if got := RuleFor(it); got != tc.rule {
				t.Fatalf("RuleFor(%q) = %q, want %q", tc.key, got, tc.rule)
			}
			f, ok := Flags(tc.rule)
			if !ok {
				t.Fatalf("Flags(%q) has no row", tc.rule)
			}
			if f != tc.flag {
				t.Errorf("Flags(%q) = %+v, want %+v", tc.rule, f, tc.flag)
			}
			if owner := RuleOwner(tc.rule); owner != OwnerOverseer {
				t.Errorf("RuleOwner(%q) = %q, want %q", tc.rule, owner, OwnerOverseer)
			}
		})
	}
}
