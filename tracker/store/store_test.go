package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
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

// An unfinished store must say so by name; an empty answer would read as "no
// issues".
func TestPostgresStubFailsLoudly(t *testing.T) {
	t.Parallel()
	_, err := NewPostgres(nil).GetIssues(context.Background(), []string{"gt-1"})
	if !errors.Is(err, ErrUnimplemented) {
		t.Fatalf("GetIssues error = %v, want ErrUnimplemented", err)
	}
	if got := err.Error(); !strings.Contains(got, "Postgres.GetIssues") {
		t.Errorf("error %q does not name the method", got)
	}
}
