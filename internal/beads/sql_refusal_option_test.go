package beads

import (
	"context"
	"reflect"
	"testing"
)

// sqlRefusalBeads returns a wrapper for dir whose bd calls go to r, with opts.
func sqlRefusalBeads(dir string, r *recorder, opts ...Option) *Beads {
	return newBeads(applyOptions(beadsFields{workDir: dir, isolated: true, noRoute: true, exec: r.exec}, opts))
}

// refusedRecorder refuses bd sql and answers the wisp list fallback.
func refusedRecorder() *recorder {
	return mergeRequestFallbackBD(
		reply{stderr: gatewaySQLRefusal, err: exitError{1}},
		reply{stdout: mergeRequestFallbackWisps},
	)
}

// verbCount counts the bd calls r recorded whose verb is verb.
func verbCount(r *recorder, verb string) int {
	n := 0
	for _, c := range r.calls() {
		if c.args[0] == verb {
			n++
		}
	}
	return n
}

// fallbackMergeRequests checks ListMergeRequests answers with the fallback wisps.
func fallbackMergeRequests(t *testing.T, b *Beads) {
	t.Helper()
	mrs, err := b.ListMergeRequests(ListOptions{Label: "gt:merge-request", Status: "all", Priority: -1})
	ids := make([]string, 0, len(mrs))
	for _, mr := range mrs {
		ids = append(ids, mr.ID)
	}
	if want := []string{"gt-wisp-mr-open", "gt-wisp-mr-closed"}; err != nil || !reflect.DeepEqual(ids, want) {
		t.Fatalf("ListMergeRequests() = %v, %v; want %v", ids, err, want)
	}
}

// TestSQLRefusalOptionSendsRefusedSQLOnce: two reads send the refused sql once.
func TestSQLRefusalOptionSendsRefusedSQLOnce(t *testing.T) {
	t.Parallel()
	r := refusedRecorder()
	b := sqlRefusalBeads(t.TempDir(), r, WithSQLRefusalMemory(&SQLRefusalMemory{}))
	fallbackMergeRequests(t, b)
	fallbackMergeRequests(t, b)
	if sql, mol := verbCount(r, "sql"), verbCount(r, "mol"); sql != 1 || mol != 2 {
		t.Errorf("bd sql calls = %d, bd mol calls = %d, want 1 and 2", sql, mol)
	}
}

// TestSQLRefusalOptionWithoutMemorySendsSQLEveryCall: no option, sql every read.
func TestSQLRefusalOptionWithoutMemorySendsSQLEveryCall(t *testing.T) {
	t.Parallel()
	r := refusedRecorder()
	b := sqlRefusalBeads(t.TempDir(), r)
	fallbackMergeRequests(t, b)
	fallbackMergeRequests(t, b)
	if got := verbCount(r, "sql"); got != 2 {
		t.Errorf("bd sql calls = %d, want 2", got)
	}
}

// TestSQLRefusalOptionActingAsSharesMemory: ActingAs keeps the parent's memory.
func TestSQLRefusalOptionActingAsSharesMemory(t *testing.T) {
	t.Parallel()
	r := refusedRecorder()
	b := sqlRefusalBeads(t.TempDir(), r, WithSQLRefusalMemory(&SQLRefusalMemory{}))
	fallbackMergeRequests(t, b)
	fallbackMergeRequests(t, b.ActingAs("polecat"))
	if got := verbCount(r, "sql"); got != 1 {
		t.Errorf("bd sql calls = %d, want 1", got)
	}
}

// TestSQLRefusalOptionOpenSharesMemoryAcrossStores: Open stores share one memory.
func TestSQLRefusalOptionOpenSharesMemoryAcrossStores(t *testing.T) {
	t.Parallel()
	r := refusedRecorder()
	var mem SQLRefusalMemory
	runner := WithDirRunner(func(ctx context.Context, dir string, args []string) ([]byte, []byte, error) {
		return r.exec(ctx, bdCall{dir: dir, args: args})
	})
	fallbackMergeRequests(t, mem.Open(t.TempDir(), runner))
	fallbackMergeRequests(t, mem.Open(t.TempDir(), runner))
	if got := verbCount(r, "sql"); got != 1 {
		t.Errorf("bd sql calls = %d, want 1", got)
	}
}
