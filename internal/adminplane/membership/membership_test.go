package membership

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/adminplane/registry"
)

// ctx is the context every call takes. Nothing cancels it: the store has no
// work to abandon.
var ctx = context.Background()

// newStore returns a Memory holding organisation "org-1" and the given members.
func newStore(t *testing.T, members ...Member) *Memory {
	t.Helper()
	m := NewMemory()
	if err := m.AddOrg(ctx, Org{ID: "org-1", Name: "Acme"}); err != nil {
		t.Fatalf("AddOrg: %v", err)
	}
	for _, member := range members {
		if err := m.AddMember(ctx, "org-1", member); err != nil {
			t.Fatalf("AddMember(%q): %v", member.Sub, err)
		}
	}
	return m
}

// mustAddWorkspace stores w and fails the test if the store refuses it.
func mustAddWorkspace(t *testing.T, m *Memory, w registry.Workspace) {
	t.Helper()
	if err := m.AddWorkspace(ctx, w); err != nil {
		t.Fatalf("AddWorkspace(%+v): %v", w, err)
	}
}

// TestEmailChangeKeepsMembership is the acceptance test for the key: a person
// who changes address keeps their membership and the workspace they own.
func TestEmailChangeKeepsMembership(t *testing.T) {
	t.Parallel()
	m := newStore(t, Member{Sub: "sub-1", Email: "old@example.com", Role: RoleOwner})
	mustAddWorkspace(t, m, registry.Workspace{ID: "ws-1", OrgID: "org-1", OwnerSub: "sub-1"})

	if err := m.SetEmail(ctx, "sub-1", "new@example.com"); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}

	members, err := m.MembersOf(ctx, "org-1")
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("members = %+v, want the one member the address change kept", members)
	}
	got := members[0]
	if got.Sub != "sub-1" || got.Role != RoleOwner {
		t.Errorf("member = %+v, want sub %q with role %q", got, "sub-1", RoleOwner)
	}
	if got.Email != "new@example.com" {
		t.Errorf("email = %q, want %q", got.Email, "new@example.com")
	}

	spaces, err := m.WorkspacesOf(ctx, "sub-1")
	if err != nil {
		t.Fatalf("WorkspacesOf: %v", err)
	}
	if len(spaces) != 1 || spaces[0].ID != "ws-1" {
		t.Errorf("workspaces = %+v, want the workspace the sub key still names", spaces)
	}
}

// TestLastOwnerCannotBeRemovedOrDemoted is the acceptance test for the owner
// rule: the last owner stays a member with the owner role, whichever way the
// caller tries to take them.
func TestLastOwnerCannotBeRemovedOrDemoted(t *testing.T) {
	t.Parallel()
	changes := map[string]func(*Memory) error{
		"removed": func(m *Memory) error { return m.RemoveMember(ctx, "org-1", "sub-1") },
		"demoted": func(m *Memory) error { return m.SetRole(ctx, "org-1", "sub-1", RoleMember) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newStore(t, Member{Sub: "sub-1", Email: "owner@example.com", Role: RoleOwner})

			if err := change(m); !errors.Is(err, ErrLastOwner) {
				t.Fatalf("change returned %v, want ErrLastOwner", err)
			}

			members, err := m.MembersOf(ctx, "org-1")
			if err != nil {
				t.Fatalf("MembersOf: %v", err)
			}
			if len(members) != 1 || members[0].Sub != "sub-1" || members[0].Role != RoleOwner {
				t.Errorf("members = %+v, want the last owner unchanged", members)
			}
		})
	}
}

// TestSameEmailDifferentSubAreTwoMembers is the acceptance test for the email's
// place in the model: it is display data, so it never merges two people.
func TestSameEmailDifferentSubAreTwoMembers(t *testing.T) {
	t.Parallel()
	m := newStore(t,
		Member{Sub: "sub-1", Email: "shared@example.com", Role: RoleOwner},
		Member{Sub: "sub-2", Email: "shared@example.com", Role: RoleMember},
	)

	members, err := m.MembersOf(ctx, "org-1")
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %+v, want two members who share an address", members)
	}
	if members[0].Sub != "sub-1" || members[1].Sub != "sub-2" {
		t.Errorf("members = %+v, want subs %q and %q in order", members, "sub-1", "sub-2")
	}

	// A change to one member's address leaves the other alone, which it could
	// not if the address were the key.
	if err := m.SetEmail(ctx, "sub-1", "one@example.com"); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}
	members, err = m.MembersOf(ctx, "org-1")
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	if members[0].Email != "one@example.com" || members[1].Email != "shared@example.com" {
		t.Errorf("emails = %q and %q, want the address change to reach only its own sub", members[0].Email, members[1].Email)
	}
}

func TestSetEmailUpdatesEveryOrganisation(t *testing.T) {
	t.Parallel()
	m := newStore(t, Member{Sub: "sub-1", Email: "old@example.com", Role: RoleOwner})
	if err := m.AddOrg(ctx, Org{ID: "org-2", Name: "Other"}); err != nil {
		t.Fatalf("AddOrg: %v", err)
	}
	if err := m.AddMember(ctx, "org-2", Member{Sub: "sub-1", Email: "old@example.com", Role: RoleMember}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	if err := m.SetEmail(ctx, "sub-1", "new@example.com"); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}

	for _, orgID := range []string{"org-1", "org-2"} {
		members, err := m.MembersOf(ctx, orgID)
		if err != nil {
			t.Fatalf("MembersOf(%q): %v", orgID, err)
		}
		if len(members) != 1 || members[0].Email != "new@example.com" {
			t.Errorf("members of %q = %+v, want the same sub with the new address", orgID, members)
		}
	}
}

func TestAddOrgRefusals(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	if err := m.AddOrg(ctx, Org{Name: "No ID"}); !errors.Is(err, ErrEmptyOrgID) {
		t.Fatalf("AddOrg without an id returned %v, want ErrEmptyOrgID", err)
	}
	if err := m.AddOrg(ctx, Org{ID: "org-1"}); err != nil {
		t.Fatalf("AddOrg: %v", err)
	}
	if err := m.AddOrg(ctx, Org{ID: "org-1"}); !errors.Is(err, ErrOrgExists) {
		t.Fatalf("AddOrg of a stored id returned %v, want ErrOrgExists", err)
	}
}

func TestAddMemberRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		orgID  string
		member Member
		want   error
	}{
		{"unknown organisation", "org-2", Member{Sub: "sub-2", Role: RoleMember}, ErrNoOrg},
		{"no sub", "org-1", Member{Email: "a@example.com", Role: RoleMember}, ErrEmptySub},
		{"no role", "org-1", Member{Sub: "sub-2"}, ErrUnknownRole},
		{"unknown role", "org-1", Member{Sub: "sub-2", Role: "admin"}, ErrUnknownRole},
		{"duplicate sub", "org-1", Member{Sub: "sub-1", Role: RoleMember}, ErrMemberExists},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newStore(t, Member{Sub: "sub-1", Role: RoleOwner})

			if err := m.AddMember(ctx, tc.orgID, tc.member); !errors.Is(err, tc.want) {
				t.Fatalf("AddMember(%q, %+v) returned %v, want %v", tc.orgID, tc.member, err, tc.want)
			}

			members, err := m.MembersOf(ctx, "org-1")
			if err != nil {
				t.Fatalf("MembersOf: %v", err)
			}
			if len(members) != 1 || members[0].Sub != "sub-1" {
				t.Errorf("members = %+v, want the refused member left out", members)
			}
		})
	}
}

func TestMembersOfUnknownOrgIsAnError(t *testing.T) {
	t.Parallel()
	m := newStore(t)
	members, err := m.MembersOf(ctx, "org-2")
	if !errors.Is(err, ErrNoOrg) {
		t.Fatalf("MembersOf(unknown) returned %v, want ErrNoOrg", err)
	}
	if members != nil {
		t.Errorf("MembersOf(unknown) returned members %+v, want none", members)
	}
}

func TestMembersOfEmptyOrg(t *testing.T) {
	t.Parallel()
	members, err := newStore(t).MembersOf(ctx, "org-1")
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("members = %+v, want none", members)
	}
}

func TestSetEmailUnknownSubIsAnError(t *testing.T) {
	t.Parallel()
	m := newStore(t, Member{Sub: "sub-1", Email: "old@example.com", Role: RoleOwner})
	if err := m.SetEmail(ctx, "sub-2", "new@example.com"); !errors.Is(err, ErrNoMember) {
		t.Fatalf("SetEmail(unknown sub) returned %v, want ErrNoMember", err)
	}
	members, err := m.MembersOf(ctx, "org-1")
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	if members[0].Email != "old@example.com" {
		t.Errorf("email = %q, want the stored address untouched", members[0].Email)
	}
}

func TestRemoveMemberRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		orgID string
		sub   string
		want  error
	}{
		{"unknown organisation", "org-2", "sub-1", ErrNoOrg},
		{"unknown member", "org-1", "sub-9", ErrNoMember},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newStore(t, Member{Sub: "sub-1", Role: RoleOwner})
			if err := m.RemoveMember(ctx, tc.orgID, tc.sub); !errors.Is(err, tc.want) {
				t.Fatalf("RemoveMember(%q, %q) returned %v, want %v", tc.orgID, tc.sub, err, tc.want)
			}
		})
	}
}

func TestRemoveMemberWithAnotherOwner(t *testing.T) {
	t.Parallel()
	m := newStore(t,
		Member{Sub: "sub-1", Role: RoleOwner},
		Member{Sub: "sub-2", Role: RoleOwner},
	)

	if err := m.RemoveMember(ctx, "org-1", "sub-2"); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	members, err := m.MembersOf(ctx, "org-1")
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	if len(members) != 1 || members[0].Sub != "sub-1" {
		t.Errorf("members = %+v, want only the owner who stayed", members)
	}
}

func TestSetRole(t *testing.T) {
	t.Parallel()
	m := newStore(t,
		Member{Sub: "sub-1", Role: RoleOwner},
		Member{Sub: "sub-2", Role: RoleMember},
	)

	if err := m.SetRole(ctx, "org-1", "sub-2", RoleOwner); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	members, err := m.MembersOf(ctx, "org-1")
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	if members[1].Sub != "sub-2" || members[1].Role != RoleOwner {
		t.Errorf("member = %+v, want sub %q promoted to %q", members[1], "sub-2", RoleOwner)
	}
}

func TestSetRoleRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		orgID string
		sub   string
		role  Role
		want  error
	}{
		{"unknown organisation", "org-2", "sub-1", RoleMember, ErrNoOrg},
		{"unknown role", "org-1", "sub-1", "admin", ErrUnknownRole},
		{"unknown member", "org-1", "sub-9", RoleMember, ErrNoMember},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newStore(t,
				Member{Sub: "sub-1", Role: RoleOwner},
				Member{Sub: "sub-2", Role: RoleOwner},
			)
			if err := m.SetRole(ctx, tc.orgID, tc.sub, tc.role); !errors.Is(err, tc.want) {
				t.Fatalf("SetRole(%q, %q, %q) returned %v, want %v", tc.orgID, tc.sub, tc.role, err, tc.want)
			}
		})
	}
}

// TestDemotingAnOwnerAWorkspaceNamesIsRefused holds a workspace to the owner it
// names even where the organisation keeps another owner.
func TestDemotingAnOwnerAWorkspaceNamesIsRefused(t *testing.T) {
	t.Parallel()
	m := newStore(t,
		Member{Sub: "sub-1", Role: RoleOwner},
		Member{Sub: "sub-2", Role: RoleOwner},
	)
	mustAddWorkspace(t, m, registry.Workspace{ID: "ws-1", OrgID: "org-1", OwnerSub: "sub-1"})

	if err := m.SetRole(ctx, "org-1", "sub-1", RoleMember); !errors.Is(err, ErrWorkspaceOwner) {
		t.Fatalf("SetRole(workspace owner) returned %v, want ErrWorkspaceOwner", err)
	}
	if err := m.RemoveMember(ctx, "org-1", "sub-1"); !errors.Is(err, ErrWorkspaceOwner) {
		t.Fatalf("RemoveMember(workspace owner) returned %v, want ErrWorkspaceOwner", err)
	}

	members, err := m.MembersOf(ctx, "org-1")
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	if len(members) != 2 || members[0].Role != RoleOwner {
		t.Errorf("members = %+v, want the workspace's owner unchanged", members)
	}

	// The owner no workspace names may still leave.
	if err := m.RemoveMember(ctx, "org-1", "sub-2"); err != nil {
		t.Fatalf("RemoveMember(second owner): %v", err)
	}
}

func TestWorkspacesOf(t *testing.T) {
	t.Parallel()
	m := newStore(t,
		Member{Sub: "sub-1", Role: RoleOwner},
		Member{Sub: "sub-2", Role: RoleMember},
	)
	mustAddWorkspace(t, m, registry.Workspace{ID: "ws-2", OrgID: "org-1", OwnerSub: "sub-1"})
	mustAddWorkspace(t, m, registry.Workspace{ID: "ws-1", OrgID: "org-1", OwnerSub: "sub-1"})

	spaces, err := m.WorkspacesOf(ctx, "sub-1")
	if err != nil {
		t.Fatalf("WorkspacesOf: %v", err)
	}
	if len(spaces) != 2 || spaces[0].ID != "ws-1" || spaces[1].ID != "ws-2" {
		t.Errorf("workspaces = %+v, want ws-1 and ws-2 in id order", spaces)
	}

	spaces, err = m.WorkspacesOf(ctx, "sub-2")
	if err != nil {
		t.Fatalf("WorkspacesOf(member): %v", err)
	}
	if len(spaces) != 0 {
		t.Errorf("workspaces = %+v, want none for a member who owns none", spaces)
	}

	for _, tc := range []struct {
		name string
		sub  string
		want error
	}{
		{"unknown sub", "sub-9", ErrNoMember},
		{"no sub", "", ErrEmptySub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spaces, err := m.WorkspacesOf(ctx, tc.sub)
			if !errors.Is(err, tc.want) {
				t.Fatalf("WorkspacesOf(%q) returned %v, want %v", tc.sub, err, tc.want)
			}
			if spaces != nil {
				t.Errorf("WorkspacesOf(%q) returned %+v, want none", tc.sub, spaces)
			}
		})
	}
}

func TestAddWorkspaceRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		store func(t *testing.T) *Memory
		space registry.Workspace
		want  error
	}{
		{
			name:  "no id",
			store: func(t *testing.T) *Memory { return newStore(t, Member{Sub: "sub-1", Role: RoleOwner}) },
			space: registry.Workspace{OrgID: "org-1", OwnerSub: "sub-1"},
			want:  ErrEmptyWorkspaceID,
		},
		{
			name: "duplicate id",
			store: func(t *testing.T) *Memory {
				m := newStore(t, Member{Sub: "sub-1", Role: RoleOwner})
				mustAddWorkspace(t, m, registry.Workspace{ID: "ws-1", OrgID: "org-1", OwnerSub: "sub-1"})
				return m
			},
			space: registry.Workspace{ID: "ws-1", OrgID: "org-1", OwnerSub: "sub-1"},
			want:  ErrWorkspaceExists,
		},
		{
			name:  "unknown organisation",
			store: func(t *testing.T) *Memory { return newStore(t, Member{Sub: "sub-1", Role: RoleOwner}) },
			space: registry.Workspace{ID: "ws-1", OrgID: "org-2", OwnerSub: "sub-1"},
			want:  ErrNoOrg,
		},
		{
			name:  "owner is not a member",
			store: func(t *testing.T) *Memory { return newStore(t, Member{Sub: "sub-1", Role: RoleOwner}) },
			space: registry.Workspace{ID: "ws-1", OrgID: "org-1", OwnerSub: "sub-2"},
			want:  ErrWorkspaceOwner,
		},
		{
			name:  "owner is a plain member",
			store: func(t *testing.T) *Memory { return newStore(t, Member{Sub: "sub-1", Role: RoleMember}) },
			space: registry.Workspace{ID: "ws-1", OrgID: "org-1", OwnerSub: "sub-1"},
			want:  ErrWorkspaceOwner,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := tc.store(t)
			before, _ := m.WorkspacesOf(ctx, tc.space.OwnerSub)

			if err := m.AddWorkspace(ctx, tc.space); !errors.Is(err, tc.want) {
				t.Fatalf("AddWorkspace(%+v) returned %v, want %v", tc.space, err, tc.want)
			}

			after, _ := m.WorkspacesOf(ctx, tc.space.OwnerSub)
			if len(after) != len(before) {
				t.Errorf("workspaces = %+v, want the refused workspace left out of the %d already stored", after, len(before))
			}
		})
	}
}

// TestZeroValueMemoryStores keeps a Memory built without NewMemory from
// panicking on its first write.
func TestZeroValueMemoryStores(t *testing.T) {
	t.Parallel()
	var m Memory
	if err := m.AddOrg(ctx, Org{ID: "org-1"}); err != nil {
		t.Fatalf("AddOrg: %v", err)
	}
	if err := m.AddMember(ctx, "org-1", Member{Sub: "sub-1", Role: RoleOwner}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	mustAddWorkspace(t, &m, registry.Workspace{ID: "ws-1", OrgID: "org-1", OwnerSub: "sub-1"})

	spaces, err := m.WorkspacesOf(ctx, "sub-1")
	if err != nil {
		t.Fatalf("WorkspacesOf: %v", err)
	}
	if len(spaces) != 1 {
		t.Errorf("workspaces = %+v, want the one stored", spaces)
	}
}
