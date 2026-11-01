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
	// A failing client whose error carries the payload, as a careless SDK could.
	c.failWith = fmt.Errorf("rpc failed for payload %s", value)
	_, putErr := s.Put(ctx, "ws-1", "api-key", []byte(value))
	_, getErr := s.Get(ctx, "ws-1", "api-key")
	// The adapter wraps the client's error; it must not add the value itself.
	// What the client chose to put in its own error is the client's to avoid, so
	// the check is that the adapter's own text is value-free.
	for name, err := range map[string]error{"Put": putErr, "Get": getErr} {
		if err == nil {
			t.Fatalf("%s with a failing client returned nil", name)
		}
		own := strings.ReplaceAll(err.Error(), c.failWith.Error(), "")
		if strings.Contains(own, value) {
			t.Errorf("%s error adds the value: %q", name, own)
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
	first, _ := s.Put(ctx, "ws-1", "api-key", []byte("one"))
	if err := s.Destroy(ctx, "ws-1", "api-key", first); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	infos, err := s.Revisions(ctx, "ws-1", "api-key")
	if err != nil || len(infos) != 0 {
		t.Fatalf("Revisions after destroying the only version = %v, %v; want none", infos, err)
	}
	if second, _ := s.Put(ctx, "ws-1", "api-key", []byte("two")); second <= first {
		t.Errorf("Put after Destroy = %d, want above %d", second, first)
	}
}

func TestGetReadsTheHighestEnabledVersionOnly(t *testing.T) {
	t.Parallel()
	c := newFakeClient()
	s := newStore(t, c)
	ctx := context.Background()
	s.Put(ctx, "ws-1", "api-key", []byte("one"))
	second, _ := s.Put(ctx, "ws-1", "api-key", []byte("two"))
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
	only, _ := s.Put(ctx, "ws-1", "api-key", []byte("one"))
	c.beforeAccess = func() {
		if err := c.DisableVersion(ctx, SecretID("ws-1", "api-key"), int64(only)); err != nil {
			t.Errorf("disable inside the race: %v", err)
		}
	}
	if _, err := s.Get(ctx, "ws-1", "api-key"); !errors.Is(err, keystore.ErrNotFound) {
		t.Errorf("Get when the newest version is disabled after the list = %v, want ErrNotFound", err)
	}
}
