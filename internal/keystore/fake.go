package keystore

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory Store: it makes no cloud call, so tests and the custody
// logic's early wiring run against it. A mutex serializes its methods, and its
// zero value is ready to use.
type Fake struct {
	mu   sync.Mutex
	keys map[keyRef]*keyEntry
}

var _ Store = (*Fake)(nil)

// keyRef addresses one key: a name within a workspace.
type keyRef struct {
	workspace string
	name      string
}

// keyEntry is every revision of one key, and the number Put assigns next. The
// entry outlives its revisions, so a Destroy never reuses a number.
type keyEntry struct {
	next Revision
	revs map[Revision]*revEntry
}

// revEntry is one revision's value and whether Get may still read it.
type revEntry struct {
	value   []byte
	enabled bool
}

// NewFake returns an empty in-memory key store.
func NewFake() *Fake { return &Fake{keys: make(map[keyRef]*keyEntry)} }

// Put stores value as the key's next revision.
func (f *Fake) Put(ctx context.Context, workspace, name string, value []byte) (Revision, error) {
	if err := checkKey(ctx, workspace, name); err != nil {
		return 0, err
	}
	if len(value) == 0 {
		return 0, ErrEmptyValue
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys == nil {
		f.keys = make(map[keyRef]*keyEntry)
	}
	ref := keyRef{workspace: workspace, name: name}
	entry := f.keys[ref]
	if entry == nil {
		entry = &keyEntry{revs: make(map[Revision]*revEntry)}
		f.keys[ref] = entry
	}
	entry.next++
	entry.revs[entry.next] = &revEntry{value: append([]byte(nil), value...), enabled: true}
	return entry.next, nil
}

// Get returns the highest enabled revision of the key.
func (f *Fake) Get(ctx context.Context, workspace, name string) (Secret, error) {
	if err := checkKey(ctx, workspace, name); err != nil {
		return Secret{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.keys[keyRef{workspace: workspace, name: name}]
	var latest Revision
	found := false
	if entry != nil {
		for rev, r := range entry.revs {
			if r.enabled && (!found || rev > latest) {
				latest, found = rev, true
			}
		}
	}
	if !found {
		return Secret{}, fmt.Errorf("%w: workspace %q key %q", ErrNotFound, workspace, name)
	}
	return newSecret(entry.revs[latest].value), nil
}

// Disable stops new reads of rev. Disabling a revision that is already disabled
// succeeds, since the requested state already holds.
func (f *Fake) Disable(ctx context.Context, workspace, name string, rev Revision) error {
	if err := checkKey(ctx, workspace, name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.revision(workspace, name, rev)
	if err != nil {
		return err
	}
	r.enabled = false
	return nil
}

// Destroy deletes rev, so no read returns it again.
func (f *Fake) Destroy(ctx context.Context, workspace, name string, rev Revision) error {
	if err := checkKey(ctx, workspace, name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.revision(workspace, name, rev); err != nil {
		return err
	}
	delete(f.keys[keyRef{workspace: workspace, name: name}].revs, rev)
	return nil
}

// revision looks up one revision. The caller holds the lock.
func (f *Fake) revision(workspace, name string, rev Revision) (*revEntry, error) {
	if entry := f.keys[keyRef{workspace: workspace, name: name}]; entry != nil {
		if r := entry.revs[rev]; r != nil {
			return r, nil
		}
	}
	return nil, fmt.Errorf("%w: workspace %q key %q revision %d", ErrRevisionNotFound, workspace, name, rev)
}
