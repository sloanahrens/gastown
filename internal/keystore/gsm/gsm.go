// Package gsm is the Secret Manager implementation of keystore.Store (plan row
// K2b). It reaches Secret Manager only through the small Client interface
// below, so this package carries no cloud SDK and every test runs against a fake
// client. A real client, written when the cloud steps are approved, implements
// Client over the SDK; nothing here makes a cloud call.
//
// No code calls this package yet. Its caller is the gateway, which loads a
// workspace's keys at start, once the cloud town exists (claude-gkl).
//
// Isolation: a Store is built for one workspace. Every secret it touches is
// named with that workspace id first, and a call that names any other workspace
// is refused before the client is called, so a Store cannot read, write, list
// or destroy another workspace's secrets. The key's value never appears in an
// error or a log line this package writes.
package gsm

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/steveyegge/gastown/internal/keystore"
)

// State is a secret version's state in Secret Manager.
type State int

// The states a version moves through; Destroyed is final.
const (
	Enabled State = iota + 1
	Disabled
	Destroyed
)

// Version is one version of a secret as the client reports it.
type Version struct {
	Number int64
	State  State
}

// ErrClientNotFound is what a Client returns for a secret or version that does
// not exist, or a version that is destroyed.
var ErrClientNotFound = errors.New("gsm: client: not found")

// Client is the part of Secret Manager the adapter uses. Version numbers start
// at 1, rise by one per AddVersion and are never reused, as in Secret Manager.
type Client interface {
	// AddVersion stores payload as a new version of secretID, creating the
	// secret first when it has none, and returns the version number.
	AddVersion(ctx context.Context, secretID string, payload []byte) (int64, error)
	// AccessVersion returns the payload of an enabled version.
	AccessVersion(ctx context.Context, secretID string, version int64) ([]byte, error)
	// DisableVersion disables an enabled version; disabling a disabled one
	// succeeds.
	DisableVersion(ctx context.Context, secretID string, version int64) error
	// DestroyVersion destroys a version, enabled or disabled.
	DestroyVersion(ctx context.Context, secretID string, version int64) error
	// ListVersions lists every version of secretID, destroyed ones included, in
	// any order. A secret that does not exist yields an empty list, no error.
	ListVersions(ctx context.Context, secretID string) ([]Version, error)
}

// ErrWrongWorkspace is the refusal of a call naming a workspace other than the
// one the Store was built for.
var ErrWrongWorkspace = errors.New("gsm: workspace is not the one this store serves")

// ErrBadName is the refusal of a workspace id or key name that cannot be a
// secret id: Secret Manager ids hold letters, digits, hyphen and underscore, and
// the adapter keeps underscore for itself as the separator, so a name with one
// could collide with another key's.
var ErrBadName = errors.New("gsm: name must be 1 to 100 characters of letters, digits and hyphen")

// Store is a keystore.Store for one workspace over a Client.
type Store struct {
	client    Client
	workspace string
}

var _ keystore.Store = (*Store)(nil)

// New returns the Store of workspace over client.
func New(client Client, workspace string) (*Store, error) {
	if client == nil {
		return nil, errors.New("gsm: a client is required")
	}
	if !validName(workspace) {
		return nil, fmt.Errorf("%w: workspace %q", ErrBadName, workspace)
	}
	return &Store{client: client, workspace: workspace}, nil
}

// SecretID is the Secret Manager id of workspace/name: the workspace id, an
// underscore, the key name. Neither part can hold an underscore, so two keys
// never share an id.
func SecretID(workspace, name string) string { return workspace + "_" + name }

// secretID checks the call is for this Store's workspace and key name, and
// returns the id. It makes no client call.
func (s *Store) secretID(ctx context.Context, workspace, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if workspace == "" || name == "" {
		return "", fmt.Errorf("%w: workspace or key name is empty", keystore.ErrInvalidKey)
	}
	if workspace != s.workspace {
		return "", fmt.Errorf("%w: got %q, store serves %q", ErrWrongWorkspace, workspace, s.workspace)
	}
	if !validName(name) {
		return "", fmt.Errorf("%w: key name %q", ErrBadName, name)
	}
	return SecretID(workspace, name), nil
}

func validName(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// Put stores value as the key's next version.
func (s *Store) Put(ctx context.Context, workspace, name string, value []byte) (keystore.Revision, error) {
	id, err := s.secretID(ctx, workspace, name)
	if err != nil {
		return 0, err
	}
	if len(value) == 0 {
		return 0, keystore.ErrEmptyValue
	}
	n, err := s.client.AddVersion(ctx, id, append([]byte(nil), value...))
	if err != nil {
		return 0, fmt.Errorf("gsm: add version of %s: %w", id, err)
	}
	return keystore.Revision(n), nil
}

// Get returns the highest enabled version.
func (s *Store) Get(ctx context.Context, workspace, name string) (keystore.Secret, error) {
	id, err := s.secretID(ctx, workspace, name)
	if err != nil {
		return keystore.Secret{}, err
	}
	versions, err := s.client.ListVersions(ctx, id)
	if err != nil {
		return keystore.Secret{}, fmt.Errorf("gsm: list versions of %s: %w", id, err)
	}
	var best int64
	for _, v := range versions {
		if v.State == Enabled && v.Number > best {
			best = v.Number
		}
	}
	if best == 0 {
		return keystore.Secret{}, fmt.Errorf("%w: workspace %q key %q", keystore.ErrNotFound, workspace, name)
	}
	payload, err := s.client.AccessVersion(ctx, id, best)
	if errors.Is(err, ErrClientNotFound) {
		// The version was disabled or destroyed between the list and the read.
		return keystore.Secret{}, fmt.Errorf("%w: workspace %q key %q", keystore.ErrNotFound, workspace, name)
	}
	if err != nil {
		return keystore.Secret{}, fmt.Errorf("gsm: access version %d of %s: %w", best, id, err)
	}
	secret := keystore.NewSecret(payload)
	clear(payload)
	return secret, nil
}

// Disable stops new reads of rev.
func (s *Store) Disable(ctx context.Context, workspace, name string, rev keystore.Revision) error {
	id, err := s.secretID(ctx, workspace, name)
	if err != nil {
		return err
	}
	err = s.client.DisableVersion(ctx, id, int64(rev))
	if errors.Is(err, ErrClientNotFound) {
		return fmt.Errorf("%w: workspace %q key %q revision %d", keystore.ErrRevisionNotFound, workspace, name, rev)
	}
	if err != nil {
		return fmt.Errorf("gsm: disable version %d of %s: %w", rev, id, err)
	}
	return nil
}

// Destroy deletes rev for good.
func (s *Store) Destroy(ctx context.Context, workspace, name string, rev keystore.Revision) error {
	id, err := s.secretID(ctx, workspace, name)
	if err != nil {
		return err
	}
	err = s.client.DestroyVersion(ctx, id, int64(rev))
	if errors.Is(err, ErrClientNotFound) {
		return fmt.Errorf("%w: workspace %q key %q revision %d", keystore.ErrRevisionNotFound, workspace, name, rev)
	}
	if err != nil {
		return fmt.Errorf("gsm: destroy version %d of %s: %w", rev, id, err)
	}
	return nil
}

// Revisions lists the versions Destroy has not deleted, lowest first.
func (s *Store) Revisions(ctx context.Context, workspace, name string) ([]keystore.RevisionInfo, error) {
	id, err := s.secretID(ctx, workspace, name)
	if err != nil {
		return nil, err
	}
	versions, err := s.client.ListVersions(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("gsm: list versions of %s: %w", id, err)
	}
	infos := make([]keystore.RevisionInfo, 0, len(versions))
	for _, v := range versions {
		if v.State == Destroyed {
			continue
		}
		infos = append(infos, keystore.RevisionInfo{Revision: keystore.Revision(v.Number), Enabled: v.State == Enabled})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Revision < infos[j].Revision })
	return infos, nil
}
