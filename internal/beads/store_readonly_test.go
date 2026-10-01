package beads

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	beadsdk "github.com/steveyegge/beads"
)

// storageMutators are the beadsdk.Storage methods that write a database, by the
// library's own reading of them. readOnlyStore must declare every one of them
// and refuse it: a write that reaches an in-process store commits with the
// pinned library's pre-fork semantics (no is_blocked maintenance, no events
// journal row, no row_lock bump) or fails on a dropped id default, and either
// way it is gastown writing beads outside bd (gt-fcxe9.11).
var storageMutators = []string{
	"AddDependency",
	"AddIssueComment",
	"AddLabel",
	"CloseIssue",
	"CreateIssue",
	"CreateIssues",
	"DeleteIssue",
	"MergeSlotCreate",
	"MergeSlotRelease",
	"RemoveDependency",
	"RemoveLabel",
	"ReopenIssue",
	"RunInTransaction",
	"SetConfig",
	"SetLocalMetadata",
	"SlotClear",
	"SlotSet",
	"UpdateIssue",
	"UpdateIssueType",
}

// storagePassThrough are the beadsdk.Storage methods readOnlyStore leaves to
// the opened store: every read, Close, and the two merge-slot writers the
// library does not re-export types for, so a facade outside the library cannot
// declare them. gastown calls neither on a store (beads_merge_slot.go reads and
// writes the slot through bd).
//
// The list is exhaustive on purpose. A method the library adds — in a version
// bump or by the wayfinder D1 work — is in neither list, and
// TestReadOnlyStore_RefusesEveryMutator fails until someone classifies it,
// rather than letting a new write reach the database unrefused.
var storagePassThrough = []string{
	"Close",
	"CountDependencies",
	"CountDependents",
	"CountEvents",
	"CountIssueComments",
	"CountIssues",
	"CountIssuesByGroup",
	"GetAllConfig",
	"GetAllEventsSince",
	"GetBlockedIssues",
	"GetConfig",
	"GetDependencies",
	"GetDependenciesWithMetadata",
	"GetDependencyTree",
	"GetDependents",
	"GetDependentsWithMetadata",
	"GetEpicsEligibleForClosure",
	"GetEvents",
	"GetIssue",
	"GetIssueByExternalRef",
	"GetIssueComments",
	"GetIssuesByIDs",
	"GetIssuesByLabel",
	"GetLabels",
	"GetLocalMetadata",
	"GetReadyWork",
	"GetReadyWorkWithCounts",
	"GetStatistics",
	"IterAllEventsSince",
	"IterBlockedIssues",
	"IterDependenciesWithMetadata",
	"IterDependentsWithMetadata",
	"IterEvents",
	"IterIssueComments",
	"IterIssues",
	"IterReadyWork",
	"IterWisps",
	"ListWisps",
	"MergeSlotAcquire",
	"MergeSlotCheck",
	"SearchIssues",
	"SearchIssuesWithCounts",
	"SlotGet",
}

// TestReadOnlyStore_RefusesEveryMutator refuses the whole write surface, and
// requires the two lists above to account for the whole Storage interface.
func TestReadOnlyStore_RefusesEveryMutator(t *testing.T) {
	t.Parallel()

	store := readOnlyStore{}
	for _, name := range storageMutators {
		method := reflect.ValueOf(store).MethodByName(name)
		if !method.IsValid() {
			t.Errorf("readOnlyStore has no method %s", name)
			continue
		}
		assertRefused(t, name, method)
	}

	classified := make(map[string]bool, len(storageMutators)+len(storagePassThrough))
	for _, name := range append(append([]string{}, storageMutators...), storagePassThrough...) {
		classified[name] = true
	}
	var unclassified, stale []string
	storage := reflect.TypeOf((*beadsdk.Storage)(nil)).Elem()
	for i := 0; i < storage.NumMethod(); i++ {
		name := storage.Method(i).Name
		if !classified[name] {
			unclassified = append(unclassified, name)
		}
		delete(classified, name)
	}
	for name := range classified {
		stale = append(stale, name)
	}
	sort.Strings(unclassified)
	sort.Strings(stale)
	if len(unclassified) > 0 {
		t.Errorf("beadsdk.Storage methods in neither list: %v — classify each as a mutator (refused) or a read (passed through)", unclassified)
	}
	if len(stale) > 0 {
		t.Errorf("listed methods that beadsdk.Storage no longer has: %v", stale)
	}
}

// assertRefused calls one method with zero-valued arguments and requires the
// read-only refusal. Arguments never matter: every refusal returns before it
// reads them. readOnlyStore embeds a nil Storage, so a method the facade does
// not declare is promoted from that interface and panics instead — which is
// the failure this asserts against, and says which write got through.
func assertRefused(t *testing.T, name string, method reflect.Value) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s reached the opened store instead of refusing (%v): readOnlyStore must declare it", name, r)
		}
	}()

	typ := method.Type()
	args := make([]reflect.Value, typ.NumIn())
	for i := range args {
		args[i] = reflect.Zero(typ.In(i))
	}
	results := method.Call(args)
	if len(results) == 0 {
		t.Errorf("%s returns nothing: it cannot report the read-only refusal", name)
		return
	}
	for _, result := range results[:len(results)-1] {
		if !result.IsZero() {
			t.Errorf("%s returned %v alongside the refusal, want the zero value", name, result.Interface())
		}
	}
	err, ok := results[len(results)-1].Interface().(error)
	if !ok {
		t.Errorf("%s's last result is not an error", name)
		return
	}
	if !errors.Is(err, ErrStoreReadOnly) {
		t.Errorf("%s = %v, want ErrStoreReadOnly", name, err)
	}
}
