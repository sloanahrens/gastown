package store

import (
	"slices"
	"strings"
	"testing"
)

// Unit tier: pure. The lists below are what every blocking query is built from.
func TestBlockingQueriesShareOneDefinition(t *testing.T) {
	t.Parallel()
	if blocking != "('blocks', 'conditional-blocks')" {
		t.Errorf("blocking = %s, want blocks and conditional-blocks", blocking)
	}
	for _, status := range []string{"closed", "pinned"} {
		if !strings.Contains(unresolved, "'"+status+"'") {
			t.Errorf("unresolved = %q does not exempt %s", unresolved, status)
		}
	}
	for _, typ := range append(slices.Clone(blockingTypes), "parent-child") {
		if !isCycleChecked(typ) {
			t.Errorf("isCycleChecked(%q) = false: blocking links and parent-child must stay acyclic", typ)
		}
	}
	for _, typ := range []string{"tracks", "relates-to", "discovered-from", ""} {
		if isCycleChecked(typ) {
			t.Errorf("isCycleChecked(%q) = true, but a non-blocking link may form a cycle", typ)
		}
	}
	for _, q := range []string{blockedCTE, moleculeBlockedCTE} {
		if !strings.Contains(q, blocking) || !strings.Contains(q, unresolved) {
			t.Errorf("a blocked CTE does not use the shared predicate: %s", q)
		}
	}
}

func TestSQLListQuotesEachValue(t *testing.T) {
	t.Parallel()
	if got := sqlList([]string{"a", "b"}); got != "('a', 'b')" {
		t.Errorf("sqlList = %q", got)
	}
}
