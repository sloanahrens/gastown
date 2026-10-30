package forgejoprov

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// testRepo is the repository the collaborator tests grant access on.
const testRepo = "acme/rig"

// statusTransport answers each method with the status given for it and the echoing
// refusal body, and records each method it sees, so a test can tell which requests
// reached the server.
type statusTransport struct {
	status  map[string]int
	methods []string
}

func (s *statusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.methods = append(s.methods, req.Method)
	return respond(req, s.status[req.Method], echoReply)
}

// TestCollaboratorPermissionTable checks the access each role's bot is granted: write
// for the polecat and landing bots, read for the viewer, none for the registry, and an
// error for a role no table names.
func TestCollaboratorPermissionTable(t *testing.T) {
	cases := []struct {
		name    string
		role    string
		perm    string
		ok      bool
		wantErr bool
	}{
		{name: "polecat", role: "polecat", perm: "write", ok: true},
		{name: "landing", role: "landing", perm: "write", ok: true},
		{name: "viewer", role: "viewer", perm: "read", ok: true},
		{name: "registry", role: "registry"},
		{name: "unknown", role: "admin", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			perm, ok, err := CollaboratorPermission(c.role)
			if (err != nil) != c.wantErr {
				t.Fatalf("CollaboratorPermission(%q) error = %v, want error %v", c.role, err, c.wantErr)
			}
			if perm != c.perm || ok != c.ok {
				t.Fatalf("CollaboratorPermission(%q) = (%q, %v), want (%q, %v)", c.role, perm, ok, c.perm, c.ok)
			}
		})
	}
}

// TestEnsureCollaboratorGrantsOnce grants the polecat bot write access and checks a
// second run reads the access back and sends no write.
func TestEnsureCollaboratorGrantsOnce(t *testing.T) {
	f := newFakeForgejo()
	f.addRepo(testRepo)
	var logs []string
	p := userProvisioner(f, false, &logs)
	if err := p.EnsureCollaborator("polecat", testRepo, "write"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := f.permission(testRepo, DefaultBotPrefix+"polecat"); got != "write" {
		t.Fatalf("after the first run the bot holds %q, want write", got)
	}
	if n := len(f.writes()); n != 1 {
		t.Fatalf("first run sent %d writes, want 1", n)
	}
	if err := p.EnsureCollaborator("polecat", testRepo, "write"); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n := len(f.writes()); n != 1 {
		t.Fatalf("second run sent %d writes in all, want the first run's 1 and no more", n)
	}
	if len(logs) != 2 || !strings.Contains(logs[1], "already has write access") {
		t.Fatalf("second run logged %q, want it to say the bot already has access", logs)
	}
}

// TestEnsureCollaboratorAcceptsAnyGrantStatus checks that a grant the server answers
// 204, 200 or 201 counts as granted, and that the grant is sent after a 404 read.
func TestEnsureCollaboratorAcceptsAnyGrantStatus(t *testing.T) {
	for _, put := range []int{http.StatusNoContent, http.StatusOK, http.StatusCreated} {
		t.Run(http.StatusText(put), func(t *testing.T) {
			tr := &statusTransport{status: map[string]int{"GET": http.StatusNotFound, "PUT": put}}
			var logs []string
			p := userProvisioner(tr, false, &logs)
			if err := p.EnsureCollaborator("polecat", testRepo, "write"); err != nil {
				t.Fatalf("grant answered %d: %v", put, err)
			}
			if !slices.Equal(tr.methods, []string{"GET", "PUT"}) {
				t.Fatalf("requests %v, want a read then a grant", tr.methods)
			}
			if len(logs) != 1 || !strings.Contains(logs[0], "granted") {
				t.Fatalf("logged %q, want one granted line", logs)
			}
		})
	}
}

// TestEnsureCollaboratorRefusesAnUnexpectedStatus drives each failing branch of the
// read and of the grant. Each error names the status, the reply is quoted through the
// client so the tokens the reply echoes stay out of it, and no grant is sent after a
// read that did not answer 404.
func TestEnsureCollaboratorRefusesAnUnexpectedStatus(t *testing.T) {
	cases := []struct {
		name        string
		get, put    int
		wantMethods []string
	}{
		{"server error on the read", http.StatusInternalServerError, 0, []string{"GET"}},
		{"forbidden read", http.StatusForbidden, 0, []string{"GET"}},
		{"forbidden grant", http.StatusNotFound, http.StatusForbidden, []string{"GET", "PUT"}},
		{"unprocessable grant", http.StatusNotFound, http.StatusUnprocessableEntity, []string{"GET", "PUT"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := &statusTransport{status: map[string]int{"GET": c.get, "PUT": c.put}}
			var logs []string
			p := userProvisioner(tr, false, &logs)
			err := p.EnsureCollaborator("polecat", testRepo, "write")
			if err == nil {
				t.Fatal("EnsureCollaborator returned nil for an unexpected status")
			}
			if !strings.Contains(err.Error(), "unexpected HTTP") {
				t.Fatalf("error %q does not name the unexpected status", err)
			}
			if strings.Contains(err.Error(), fixtureToken) || strings.Contains(err.Error(), mintedSecret) {
				t.Fatal("error text carries a token the reply echoed")
			}
			if !slices.Equal(tr.methods, c.wantMethods) {
				t.Fatalf("requests %v, want %v", tr.methods, c.wantMethods)
			}
		})
	}
}

// TestEnsureCollaboratorStopsOnTransportFailure checks a read that never reaches the
// server is returned as an error and no grant follows it.
func TestEnsureCollaboratorStopsOnTransportFailure(t *testing.T) {
	var logs []string
	p := userProvisioner(failingTransport{}, false, &logs)
	if err := p.EnsureCollaborator("polecat", testRepo, "write"); err == nil {
		t.Fatal("EnsureCollaborator returned nil when the server was unreachable")
	}
}

// TestEnsureCollaboratorRefusesAMalformedRepo checks a repository that is not
// owner/name is refused before any request is sent.
func TestEnsureCollaboratorRefusesAMalformedRepo(t *testing.T) {
	for _, repo := range []string{"acme", "/rig", "acme/"} {
		t.Run(repo, func(t *testing.T) {
			f := newFakeForgejo()
			var logs []string
			p := userProvisioner(f, false, &logs)
			if err := p.EnsureCollaborator("polecat", repo, "write"); err == nil {
				t.Fatalf("EnsureCollaborator accepted %q", repo)
			}
			if n := len(f.callLog()); n != 0 {
				t.Fatalf("sent %d requests for a malformed repository, want none", n)
			}
		})
	}
}

// TestEnsureCollaboratorDryRunSendsNoWrite checks a dry run reads the access, reports
// the grant it would make, and sends no write.
func TestEnsureCollaboratorDryRunSendsNoWrite(t *testing.T) {
	f := newFakeForgejo()
	f.addRepo(testRepo)
	var logs []string
	p := userProvisioner(f, true, &logs)
	if err := p.EnsureCollaborator("polecat", testRepo, "write"); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := len(f.writes()); n != 0 {
		t.Fatalf("dry run sent %d writes, want none", n)
	}
	if got := f.permission(testRepo, DefaultBotPrefix+"polecat"); got != "" {
		t.Fatalf("dry run granted %q, want no grant", got)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "would grant") {
		t.Fatalf("dry run logged %q, want one would-grant line", logs)
	}
}

// TestEnsureViewerAccessOnlyWhenTheViewerExistsAndIsNotRequested checks the viewer's
// read grant lands only when the viewer bot exists and the run does not provision the
// viewer role itself. A run that provisions it grants it in its role loop instead.
func TestEnsureViewerAccessOnlyWhenTheViewerExistsAndIsNotRequested(t *testing.T) {
	cases := []struct {
		name         string
		viewerExists bool
		requested    bool
		wantGrant    bool
	}{
		{name: "exists and not requested grants read", viewerExists: true, wantGrant: true},
		{name: "requested is left to the role loop", viewerExists: true, requested: true},
		{name: "missing user is skipped", viewerExists: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeForgejo()
			f.addRepo(testRepo)
			if c.viewerExists {
				withBot(t, f, "viewer")
			}
			before := len(f.callLog())
			var logs []string
			p := userProvisioner(f, false, &logs)
			p.ViewerRequested = c.requested
			if err := p.EnsureViewerAccess(testRepo); err != nil {
				t.Fatalf("EnsureViewerAccess: %v", err)
			}
			got := f.permission(testRepo, DefaultBotPrefix+"viewer")
			if c.wantGrant != (got == "read") {
				t.Fatalf("viewer holds %q on %s; grant wanted %v", got, testRepo, c.wantGrant)
			}
			if c.requested && len(f.callLog()) != before {
				t.Fatalf("a run that provisions the viewer sent %d requests, want none", len(f.callLog())-before)
			}
			if !c.viewerExists && !strings.Contains(strings.Join(logs, "\n"), "skipping") {
				t.Fatalf("missing viewer logged %q, want a skip notice", logs)
			}
		})
	}
}

// TestEnsureViewerAccessRefusesWhatItCannotRead checks an unexpected status on the
// viewer's user read is an error, and no grant is sent after it.
func TestEnsureViewerAccessRefusesWhatItCannotRead(t *testing.T) {
	tr := &statusTransport{status: map[string]int{"GET": http.StatusInternalServerError}}
	var logs []string
	p := userProvisioner(tr, false, &logs)
	err := p.EnsureViewerAccess(testRepo)
	if err == nil || !strings.Contains(err.Error(), "unexpected HTTP") {
		t.Fatalf("EnsureViewerAccess error = %v, want an unexpected-status error", err)
	}
	if !slices.Equal(tr.methods, []string{"GET"}) {
		t.Fatalf("requests %v, want only the read of the viewer user", tr.methods)
	}
}
