// Package membership is the admin plane's organization and membership model
// (plan row O1, gt-hqmhf): the organizations a hosted workspace belongs to, the
// people in them, and which workspaces each person owns.
//
// A member is keyed on the Google subject, never on the email. The email is
// display data, so a person whose address changes keeps their membership and
// their workspaces, and two people who share an address are two members.
//
// Nothing calls it yet. Its callers are sign-in, invitations and onboarding
// (plan rows O2, O3 and O5), which supply a Store over the admin plane's
// Postgres tables (a later bead); Memory is the in-memory fake those callers
// and these tests use. The package holds no database and no HTTP.
package membership

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/steveyegge/gastown/internal/adminplane/registry"
)

// Role is what a member may do in an organization: RoleOwner administers it and
// RoleMember takes part in it.
type Role string

// The two roles. Every organization keeps at least one RoleOwner.
const (
	RoleOwner  Role = "owner"
	RoleMember Role = "member"
)

// known reports whether r is a role this package defines, so a caller's typo is
// refused rather than stored and read back later as if it meant something.
func (r Role) known() bool {
	return r == RoleOwner || r == RoleMember
}

// Org is an organization: the tenant whose workspaces and membership are
// managed together.
type Org struct {
	ID   string
	Name string
}

// Member is one person's place in an organization. Sub is the Google subject
// and the key; Email is stored for display and identifies nobody.
type Member struct {
	Sub   string
	Email string
	Role  Role
}

var (
	// ErrEmptyOrgID refuses an organization with no id, which nothing could name.
	ErrEmptyOrgID = errors.New("membership: an organization needs an id")
	// ErrOrgExists refuses a second organization with an id already stored.
	ErrOrgExists = errors.New("membership: organization already exists")
	// ErrNoOrg reports a read or change naming an organization the store does not hold.
	ErrNoOrg = errors.New("membership: unknown organization")
	// ErrEmptySub refuses a member with no subject, since the subject is the key.
	ErrEmptySub = errors.New("membership: a member needs a sub")
	// ErrMemberExists refuses a second membership for one subject in one organization.
	ErrMemberExists = errors.New("membership: that sub is already a member")
	// ErrNoMember reports a change naming a subject that is not a member.
	ErrNoMember = errors.New("membership: unknown member")
	// ErrUnknownRole refuses a role this package does not define.
	ErrUnknownRole = errors.New("membership: unknown role")
	// ErrLastOwner refuses removing or demoting an organization's last owner.
	ErrLastOwner = errors.New("membership: an organization keeps at least one owner")
	// ErrWorkspaceOwner refuses a change that would leave a workspace without
	// the owner-role member it names as its owner.
	ErrWorkspaceOwner = errors.New("membership: a workspace keeps the owner it names")
	// ErrEmptyWorkspaceID refuses a workspace with no id, which nothing could name.
	ErrEmptyWorkspaceID = errors.New("membership: a workspace needs an id")
	// ErrWorkspaceExists refuses a second workspace with an id already stored.
	ErrWorkspaceExists = errors.New("membership: workspace already exists")
)

// Model is the store's methods as plan row O1 lists them (gt-hqmhf).
type Model interface {
	// AddOrg stores o. An error means it was not stored.
	AddOrg(ctx context.Context, o Org) error
	// AddMember adds m to the organization orgID. An error means it was not added.
	AddMember(ctx context.Context, orgID string, m Member) error
	// MembersOf returns the organization's members ordered by sub. A missing
	// organization is an error, never an empty list.
	MembersOf(ctx context.Context, orgID string) ([]Member, error)
	// WorkspacesOf returns the workspaces owned by sub ordered by id. A subject
	// that is not a member is an error, never an empty list.
	WorkspacesOf(ctx context.Context, sub string) ([]registry.Workspace, error)
	// SetEmail stores email as the display address of every membership held by
	// sub. The subject, and so the membership, does not change.
	SetEmail(ctx context.Context, sub, email string) error
}

// Store is where organizations, their members and their workspaces live: Model,
// plus the three membership changes the owner rule is checked on, since it is a
// change that could take an owner away and a stored workspace that has one to
// lose (gt-hqmhf). The admin plane's implementation is its Postgres tables (a
// later bead); Memory is the in-memory fake.
type Store interface {
	Model
	// SetRole changes the role of the member sub in orgID. A change that would
	// take the organization's last owner, or the owner a workspace names, is an
	// error and leaves the role as it was.
	SetRole(ctx context.Context, orgID, sub string, role Role) error
	// RemoveMember removes sub from orgID. A change that would take the
	// organization's last owner, or the owner a workspace names, is an error and
	// leaves the member in place.
	RemoveMember(ctx context.Context, orgID, sub string) error
	// AddWorkspace stores w. Its owner must be an owner-role member of the
	// organization it names, so a stored workspace always has exactly one owner.
	AddWorkspace(ctx context.Context, w registry.Workspace) error
}

// Memory is an in-memory Store for tests and for callers before the Postgres
// tables exist. Its zero value is ready to use. It is not safe for concurrent
// use.
type Memory struct {
	orgs    map[string]Org
	members map[string]map[string]Member // organization id -> sub -> member
	spaces  map[string]registry.Workspace
}

// The fake the callers use must be a Store, so a method either interface gains
// without it fails the build here rather than at the first caller.
var _ Store = (*Memory)(nil)

// NewMemory returns an empty Memory.
func NewMemory() *Memory {
	return &Memory{
		orgs:    map[string]Org{},
		members: map[string]map[string]Member{},
		spaces:  map[string]registry.Workspace{},
	}
}

// ensure fills in the maps the zero value lacks, so a Memory built without
// NewMemory stores rather than panics on its first write.
func (m *Memory) ensure() {
	if m.orgs == nil {
		m.orgs = map[string]Org{}
	}
	if m.members == nil {
		m.members = map[string]map[string]Member{}
	}
	if m.spaces == nil {
		m.spaces = map[string]registry.Workspace{}
	}
}

// AddOrg stores o.
func (m *Memory) AddOrg(_ context.Context, o Org) error {
	m.ensure()
	if o.ID == "" {
		return ErrEmptyOrgID
	}
	if _, ok := m.orgs[o.ID]; ok {
		return ErrOrgExists
	}
	m.orgs[o.ID] = o
	m.members[o.ID] = map[string]Member{}
	return nil
}

// AddMember adds m to the organization orgID.
func (m *Memory) AddMember(_ context.Context, orgID string, member Member) error {
	bySub, ok := m.members[orgID]
	if !ok {
		return ErrNoOrg
	}
	if member.Sub == "" {
		return ErrEmptySub
	}
	if !member.Role.known() {
		return fmt.Errorf("%w: %q", ErrUnknownRole, member.Role)
	}
	if _, ok := bySub[member.Sub]; ok {
		return ErrMemberExists
	}
	bySub[member.Sub] = member
	return nil
}

// MembersOf returns the organization's members ordered by sub.
func (m *Memory) MembersOf(_ context.Context, orgID string) ([]Member, error) {
	bySub, ok := m.members[orgID]
	if !ok {
		return nil, ErrNoOrg
	}
	out := make([]Member, 0, len(bySub))
	for _, member := range bySub {
		out = append(out, member)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sub < out[j].Sub })
	return out, nil
}

// WorkspacesOf returns the workspaces owned by sub ordered by id.
func (m *Memory) WorkspacesOf(_ context.Context, sub string) ([]registry.Workspace, error) {
	if sub == "" {
		return nil, ErrEmptySub
	}
	if !m.isMember(sub) {
		return nil, ErrNoMember
	}
	out := []registry.Workspace{}
	for _, w := range m.spaces {
		if w.OwnerSub == sub {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// isMember reports whether sub holds a membership in any organization, which is
// what makes WorkspacesOf's empty answer a real answer rather than a typo.
func (m *Memory) isMember(sub string) bool {
	for _, bySub := range m.members {
		if _, ok := bySub[sub]; ok {
			return true
		}
	}
	return false
}

// SetEmail stores email on every membership held by sub.
func (m *Memory) SetEmail(_ context.Context, sub, email string) error {
	if sub == "" {
		return ErrEmptySub
	}
	found := false
	for _, bySub := range m.members {
		member, ok := bySub[sub]
		if !ok {
			continue
		}
		member.Email = email
		bySub[sub] = member
		found = true
	}
	if !found {
		return ErrNoMember
	}
	return nil
}

// SetRole changes the role of the member sub in orgID.
func (m *Memory) SetRole(_ context.Context, orgID, sub string, role Role) error {
	bySub, ok := m.members[orgID]
	if !ok {
		return ErrNoOrg
	}
	if !role.known() {
		return fmt.Errorf("%w: %q", ErrUnknownRole, role)
	}
	member, ok := bySub[sub]
	if !ok {
		return ErrNoMember
	}
	if member.Role == RoleOwner && role != RoleOwner {
		if err := m.refuseLossOfOwner(orgID, sub); err != nil {
			return err
		}
	}
	member.Role = role
	bySub[sub] = member
	return nil
}

// RemoveMember removes sub from orgID.
func (m *Memory) RemoveMember(_ context.Context, orgID, sub string) error {
	bySub, ok := m.members[orgID]
	if !ok {
		return ErrNoOrg
	}
	member, ok := bySub[sub]
	if !ok {
		return ErrNoMember
	}
	if member.Role == RoleOwner {
		if err := m.refuseLossOfOwner(orgID, sub); err != nil {
			return err
		}
	}
	delete(bySub, sub)
	return nil
}

// refuseLossOfOwner is the owner rule for one departing owner: the organization
// keeps at least one owner, and a workspace keeps the owner it names.
func (m *Memory) refuseLossOfOwner(orgID, sub string) error {
	owners := 0
	for _, member := range m.members[orgID] {
		if member.Role == RoleOwner {
			owners++
		}
	}
	if owners <= 1 {
		return ErrLastOwner
	}
	for _, w := range m.spaces {
		if w.OrgID == orgID && w.OwnerSub == sub {
			return fmt.Errorf("%w: workspace %q names it", ErrWorkspaceOwner, w.ID)
		}
	}
	return nil
}

// AddWorkspace stores w.
func (m *Memory) AddWorkspace(_ context.Context, w registry.Workspace) error {
	if w.ID == "" {
		return ErrEmptyWorkspaceID
	}
	if _, ok := m.spaces[w.ID]; ok {
		return ErrWorkspaceExists
	}
	bySub, ok := m.members[w.OrgID]
	if !ok {
		return ErrNoOrg
	}
	owner, ok := bySub[w.OwnerSub]
	if !ok || owner.Role != RoleOwner {
		return fmt.Errorf("%w: workspace %q names %q, who is not an owner-role member of organization %q",
			ErrWorkspaceOwner, w.ID, w.OwnerSub, w.OrgID)
	}
	m.spaces[w.ID] = w
	return nil
}
