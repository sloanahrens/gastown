package store

import (
	"errors"
	"reflect"
	"slices"
	"strings"
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
	want := []string{"AddComment", "AddDependency", "Blocked", "ChildrenOf", "CloseIssues", "Comments", "CreateIssue", "DeleteIssues", "Dependencies", "EventsAfter", "GetIssues", "ListIssues", "Ready", "RemoveDependency", "UpdateIssue"}
	if !slices.Equal(got, want) {
		t.Errorf("Store methods = %v\nwant           %v\nchange this list deliberately, with the server, fake and contract", got, want)
	}
}

// An unfinished store method must say so by name; an empty answer would read
// as "no issues". The helper builds that error for every stubbed method, so
// it is tested directly: no particular method stays a stub.
func TestUnimplementedNamesTheMethod(t *testing.T) {
	t.Parallel()
	err := unimplemented("SomeMethod")
	if !errors.Is(err, ErrUnimplemented) {
		t.Fatalf("unimplemented() = %v, want ErrUnimplemented", err)
	}
	if got := err.Error(); !strings.Contains(got, "Postgres.SomeMethod") {
		t.Errorf("error %q does not name the method", got)
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
