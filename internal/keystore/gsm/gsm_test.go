package gsm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/keystore"
	"github.com/steveyegge/gastown/internal/keystore/storetest"
)

func newStore(t *testing.T, c Client) *Store {
	t.Helper()
	s, err := New(c, "ws-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// mustPut stores value under ws-1/api-key and fails the test when the store
// refuses it, so a setup that did not happen cannot pass for one that did.
func mustPut(t *testing.T, s *Store, value string) keystore.Revision {
	t.Helper()
	rev, err := s.Put(context.Background(), "ws-1", "api-key", []byte(value))
	if err != nil {
		t.Fatalf("setup Put(%q): %v", value, err)
	}
	return rev
}

func TestContractOverFakeClient(t *testing.T) {
	t.Parallel()
	storetest.Run(t, "ws-1", func(t *testing.T) keystore.Store { return newStore(t, newFakeClient()) })
}

func TestRefusesNamesOutsideItsWorkspace(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	s := newStore(t, c)
	ctx := context.Background()

	_, putErr := s.Put(ctx, "ws-2", "api-key", []byte("v"))
	_, getErr := s.Get(ctx, "ws-2", "api-key")
	disableErr := s.Disable(ctx, "ws-2", "api-key", 1)
	destroyErr := s.Destroy(ctx, "ws-2", "api-key", 1)
	_, revErr := s.Revisions(ctx, "ws-2", "api-key")
	for name, err := range map[string]error{"Put": putErr, "Get": getErr, "Disable": disableErr, "Destroy": destroyErr, "Revisions": revErr} {
		if !errors.Is(err, ErrWrongWorkspace) {
			t.Errorf("%s for another workspace = %v, want ErrWrongWorkspace", name, err)
		}
	}
	if n := c.callCount(); n != 0 {
		t.Errorf("client saw %d calls for another workspace, want none", n)
	}
}

func TestSecretIDsNeverCollide(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	s := newStore(t, c)
	ctx := context.Background()
	// A name holding the separator is refused, so ws-1/a_b can never share an id
	// with a key of a workspace named ws-1_a.
	if _, err := s.Put(ctx, "ws-1", "a_b", []byte("v")); !errors.Is(err, ErrBadName) {
		t.Errorf("Put with an underscore in the name = %v, want ErrBadName", err)
	}
	if _, err := s.Put(ctx, "ws-1", "../ws-2_key", []byte("v")); !errors.Is(err, ErrBadName) {
		t.Errorf("Put with a path in the name = %v, want ErrBadName", err)
	}
	if _, err := New(c, "ws_1"); !errors.Is(err, ErrBadName) {
		t.Errorf("New with an underscore in the workspace = %v, want ErrBadName", err)
	}
	if n := c.callCount(); n != 0 {
		t.Errorf("client saw %d calls for refused names, want none", n)
	}
	if got := SecretID("ws-1", "api-key"); !strings.HasPrefix(got, "ws-1_") {
		t.Errorf("SecretID = %q, want the workspace id first", got)
	}
}

func TestNoValueInErrors(t *testing.T) {
	t.Parallel()
	const value = "sk-live-NEVER-SHOWN"
	c := newFakeClient()
	s := newStore(t, c)
	ctx := context.Background()
	if _, err := s.Put(ctx, "ws-1", "api-key", []byte(value)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A failing client whose own error is value-free: whatever the adapter adds
	// around it must be value-free too, so the whole text is checked, not what
	// is left after taking the client's string out.
	c.failWith = errors.New("rpc failed")
	_, putErr := s.Put(ctx, "ws-1", "api-key", []byte(value))
	_, getErr := s.Get(ctx, "ws-1", "api-key")
	for name, err := range map[string]error{"Put": putErr, "Get": getErr} {
		if err == nil {
			t.Fatalf("%s with a failing client returned nil", name)
		}
		if !errors.Is(err, c.failWith) {
			t.Errorf("%s error %q lost the client's error", name, err)
		}
		if strings.Contains(err.Error(), value) {
			t.Errorf("%s error adds the value: %q", name, err.Error())
		}
	}
}

func TestSecretPrintsRedacted(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	s := newStore(t, c)
	ctx := context.Background()
	if _, err := s.Put(ctx, "ws-1", "api-key", []byte("sk-live-NEVER-SHOWN")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	secret, err := s.Get(ctx, "ws-1", "api-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := fmt.Sprintf("%v %+v %#v", secret, secret, secret); strings.Contains(got, "NEVER-SHOWN") {
		t.Errorf("printing a Secret leaked the value: %q", got)
	}
}

func TestDestroyedVersionNumberIsNotReusedAndLeavesRevisions(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	s := newStore(t, c)
	ctx := context.Background()
	first := mustPut(t, s, "one")
	if err := s.Destroy(ctx, "ws-1", "api-key", first); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	infos, err := s.Revisions(ctx, "ws-1", "api-key")
	if err != nil || len(infos) != 0 {
		t.Fatalf("Revisions after destroying the only version = %v, %v; want none", infos, err)
	}
	if second := mustPut(t, s, "two"); second <= first {
		t.Errorf("Put after Destroy = %d, want above %d", second, first)
	}
}

func TestGetReadsTheHighestEnabledVersionOnly(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	s := newStore(t, c)
	ctx := context.Background()
	mustPut(t, s, "one")
	second := mustPut(t, s, "two")
	if err := s.Disable(ctx, "ws-1", "api-key", second); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	c.mu.Lock()
	c.calls = nil
	c.mu.Unlock()
	secret, err := s.Get(ctx, "ws-1", "api-key")
	if err != nil || string(secret.Bytes()) != "one" {
		t.Fatalf("Get = %q, %v; want one", secret.Bytes(), err)
	}
	// One list and one access of exactly the secret asked for.
	c.mu.Lock()
	defer c.mu.Unlock()
	want := []string{"list ws-1_api-key", "access ws-1_api-key"}
	if fmt.Sprint(c.calls) != fmt.Sprint(want) {
		t.Errorf("client calls = %v, want %v", c.calls, want)
	}
}

func TestClientErrorsAreWrappedNotSwallowed(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	s := newStore(t, c)
	boom := errors.New("backend down")
	c.failWith = boom
	ctx := context.Background()
	if _, err := s.Get(ctx, "ws-1", "api-key"); !errors.Is(err, boom) || errors.Is(err, keystore.ErrNotFound) {
		t.Errorf("Get with a failing client = %v, want the client's error and not ErrNotFound", err)
	}
	if _, err := s.Revisions(ctx, "ws-1", "api-key"); !errors.Is(err, boom) {
		t.Errorf("Revisions with a failing client = %v, want the client's error", err)
	}
}

func TestGetNotFoundWhenTheVersionDisappearsBetweenListAndRead(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	s := newStore(t, c)
	ctx := context.Background()
	only := mustPut(t, s, "one")
	c.beforeAccess = func() {
		if err := c.DisableVersion(ctx, SecretID("ws-1", "api-key"), int64(only)); err != nil {
			t.Errorf("disable inside the race: %v", err)
		}
	}
	if _, err := s.Get(ctx, "ws-1", "api-key"); !errors.Is(err, keystore.ErrNotFound) {
		t.Errorf("Get when the newest version is disabled after the list = %v, want ErrNotFound", err)
	}
}

func TestEachClientFailureIsWrappedWithItsOperation(t *testing.T) {
	t.Parallel()
	boom := errors.New("backend down")
	ctx := context.Background()
	for _, tc := range []struct {
		method string
		call   func(s *Store) error
	}{
		{"access", func(s *Store) error { _, err := s.Get(ctx, "ws-1", "api-key"); return err }},
		{"disable", func(s *Store) error { return s.Disable(ctx, "ws-1", "api-key", 1) }},
		{"destroy", func(s *Store) error { return s.Destroy(ctx, "ws-1", "api-key", 1) }},
	} {
		c := newFakeClient()
		s := newStore(t, c)
		if _, err := s.Put(ctx, "ws-1", "api-key", []byte("one")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		c.failWith, c.failOnly = boom, tc.method
		err := tc.call(s)
		if !errors.Is(err, boom) || errors.Is(err, keystore.ErrNotFound) || errors.Is(err, keystore.ErrRevisionNotFound) {
			t.Errorf("%s with a failing client = %v, want the client's error and no not-found", tc.method, err)
		}
		if err != nil && !strings.Contains(err.Error(), tc.method) {
			t.Errorf("%s error %q does not name the operation", tc.method, err)
		}
	}
}

func TestNewRefusesANilClient(t *testing.T) {
	t.Parallel()
	if s, err := New(nil, "ws-1"); err == nil || s != nil {
		t.Errorf("New(nil client) = %v, %v, want an error and no store", s, err)
	}
}

// A newest version disabled between the list and the read leaves the older
// enabled one current: Get returns it rather than reporting the key missing.
func TestGetFallsBackToTheNextEnabledVersion(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	s := newStore(t, c)
	ctx := context.Background()
	mustPut(t, s, "one")
	second := mustPut(t, s, "two")
	once := true
	c.beforeAccess = func() {
		if !once {
			return
		}
		once = false
		if err := c.DisableVersion(ctx, SecretID("ws-1", "api-key"), int64(second)); err != nil {
			t.Errorf("disable inside the race: %v", err)
		}
	}
	secret, err := s.Get(ctx, "ws-1", "api-key")
	if err != nil || string(secret.Bytes()) != "one" {
		t.Fatalf("Get after the newest version was disabled mid-read = %q, %v; want one", secret.Bytes(), err)
	}
}
