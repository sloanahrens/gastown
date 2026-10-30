package store

import (
	"reflect"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// The Store method set is the contract between the server, the fake, the
// Postgres store and every bead that fills one in. A rename or an addition
// must show in review, so the set is pinned here.
func TestStoreMethodSetIsPinned(t *testing.T) {
	t.Parallel() // unit tier: reflection over a type
	typ := reflect.TypeFor[Store]()
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	want := []string{"AddComment", "AddDependency", "Assigned", "Batch", "Blocked", "ChildrenOf", "Claim", "CloseIssues", "Comments", "ConfigGet", "ConfigSet", "CountIssues", "CreateGraph", "CreateIssue", "DeleteIssues", "Dependencies", "DispatchBoard", "EventsAfter", "Export", "GetIssues", "Identify", "IssueTokenScoped", "ListIssues", "Ready", "Release", "RemoveDependency", "RevokeActorTokens", "RevokeTokenID", "Stats", "TownSnapshot", "Transfer", "UpdateIssue"}
	if !slices.Equal(got, want) {
		t.Errorf("Store methods = %v\nwant           %v\nchange this list deliberately, with the server, fake and contract", got, want)
	}
}

// api.SchemaLevel is what clients send and store.SchemaLevel is what the
// database was migrated to; they are the same number or every request skews.
func TestAPISchemaLevelEqualsTheStoresMigrationCount(t *testing.T) {
	t.Parallel()
	if api.SchemaLevel != SchemaLevel() {
		t.Errorf("api.SchemaLevel = %d, store.SchemaLevel() = %d; bump api.SchemaLevel with the migration", api.SchemaLevel, SchemaLevel())
	}
}
