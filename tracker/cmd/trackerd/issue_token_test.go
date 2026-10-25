package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
)

type fakeIssuer struct {
	token string
	err   error
	calls int
	scope api.Scope
}

func (f *fakeIssuer) IssueTokenScoped(_ context.Context, _ string, scope api.Scope, _ *time.Time) (string, error) {
	f.calls++
	f.scope = scope
	return f.token, f.err
}

// Unit tier: a fake issuer and a temp directory; microseconds.
func TestIssueTokenWritesAPrivateFileAndNeverPrintsTheToken(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tok")
	var out bytes.Buffer
	if err := issueToken(context.Background(), &fakeIssuer{token: "trk_abc"}, "town/crew/x", api.ScopeSeat, path, &out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "trk_abc\n" {
		t.Errorf("token file = %q, %v; want the token and a newline", b, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	if strings.Contains(out.String(), "trk_abc") || !strings.Contains(out.String(), path) {
		t.Errorf("output %q must name the file and never the token", out.String())
	}
}

func TestIssueTokenFailureLeavesNoFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tok")
	var out bytes.Buffer
	err := issueToken(context.Background(), &fakeIssuer{err: errors.New("database down")}, "x", api.ScopeSeat, path, &out)
	if err == nil || !strings.Contains(err.Error(), "database down") {
		t.Errorf("error = %v, want the issuer's failure", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a failed issue left %s behind (stat: %v); an empty token file looks like a good one", path, statErr)
	}
	if out.Len() != 0 {
		t.Errorf("output %q after a failure, want none", out.String())
	}
}

func TestIssueTokenNeverOverwritesAndNeverCallsTheIssuer(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{token: "trk_new"}
	if err := issueToken(context.Background(), f, "x", api.ScopeSeat, path, &bytes.Buffer{}); err == nil {
		t.Error("issuing into an existing file succeeded")
	}
	if f.calls != 0 {
		t.Errorf("the issuer was called %d times for a path that could not be written; that would mint an unused token", f.calls)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep me" {
		t.Errorf("existing file now %q, want it untouched", b)
	}
}

func TestIssueTokenPassesTheScopeAndSaysItWithoutPrintingTheToken(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "admin.token")
	var out bytes.Buffer
	f := &fakeIssuer{token: "trk_secret"}
	if err := issueToken(context.Background(), f, "town/daemon", api.ScopeAdmin, path, &out); err != nil {
		t.Fatal(err)
	}
	if f.scope != api.ScopeAdmin {
		t.Errorf("issuer got scope %q, want admin", f.scope)
	}
	if !strings.Contains(out.String(), "admin token for town/daemon") || strings.Contains(out.String(), "trk_secret") {
		t.Errorf("output %q should name the scope and never the token", out.String())
	}
}
