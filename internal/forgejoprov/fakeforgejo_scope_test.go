package forgejoprov

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejotest"
)

// scopeDrift reports how a role table and a known scope list disagree: a role with no
// scope, a scope a role uses that the list lacks, and a listed scope no role uses. An
// empty table is a disagreement too, so the check cannot pass on nothing. These checks stay
// in forgejoprov, not forgejotest, because they read roleTable, which is unexported here;
// forgejotest keeps its own scope list and cannot see the table.
func scopeDrift(table []roleScopes, known []string) []string {
	if len(table) == 0 {
		return []string{"the role table is empty"}
	}
	var problems []string
	used := map[string]bool{}
	for _, r := range table {
		if len(r.scopes) == 0 {
			problems = append(problems, fmt.Sprintf("role %s has no scopes", r.name))
		}
		for _, s := range r.scopes {
			used[s] = true
			if !slices.Contains(known, s) {
				problems = append(problems, fmt.Sprintf("role %s uses scope %s, which the fake does not accept", r.name, s))
			}
		}
	}
	for _, s := range known {
		if !used[s] {
			problems = append(problems, fmt.Sprintf("the fake accepts scope %s, which no role uses", s))
		}
	}
	return problems
}

// TestFakeKnowsEveryRoleScope checks that the fake's scope list and the production role
// table agree both ways: every scope a role uses is one the fake accepts, and every scope
// the fake accepts is one a role uses. The table must not be empty.
func TestFakeKnowsEveryRoleScope(t *testing.T) {
	t.Parallel()
	table := roleTable()
	if len(table) == 0 {
		t.Fatalf("the production role table is empty")
	}
	if problems := scopeDrift(table, forgejotest.KnownScopes()); len(problems) != 0 {
		t.Errorf("the fake and the role table disagree: %v", problems)
	}
}

// TestScopeDriftReports drives each way the role table and the known list can disagree,
// and the agreeing case, so the check is seen to report a problem and to pass when there
// is none.
func TestScopeDriftReports(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		table []roleScopes
		known []string
		want  string
	}{
		{"agreeing table and list", []roleScopes{{"polecat", []string{"write:repository"}}}, []string{"write:repository"}, ""},
		{"a role uses a scope the list lacks", []roleScopes{{"admin", []string{"write:admin"}}}, []string{"write:repository"}, "role admin uses scope write:admin"},
		{"the list holds a scope no role uses", []roleScopes{{"polecat", []string{"write:repository"}}}, []string{"write:repository", "read:user"}, "scope read:user, which no role uses"},
		{"a role with no scopes", []roleScopes{{"empty", nil}}, []string{"write:repository"}, "role empty has no scopes"},
		{"an empty role table", nil, []string{"write:repository"}, "the role table is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := scopeDrift(tc.table, tc.known)
			if tc.want == "" {
				if len(problems) != 0 {
					t.Fatalf("agreeing table reported problems: %v", problems)
				}
				return
			}
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.want) }) {
				t.Fatalf("problems %v do not name %q", problems, tc.want)
			}
		})
	}
}
