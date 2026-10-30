package forgejoprov

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejotest"
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

// levelTransport answers the collaborator read with collab and the permission read with
// level and levelBody. A permission read that the transport drops returns the transport
// error instead, when levelDrops is set. Every write is answered 204. It records each
// method it sees.
type levelTransport struct {
	collab     int
	level      int
	levelBody  string
	levelDrops bool
	methods    []string
}

func (l *levelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	l.methods = append(l.methods, req.Method)
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/permission"):
		if l.levelDrops {
			return failingTransport{}.RoundTrip(req)
		}
		return respond(req, l.level, l.levelBody)
	case req.Method == http.MethodGet:
		return respond(req, l.collab, "")
	}
	return respond(req, http.StatusNoContent, "")
}

// seedCollaborator gives user access to repo at level on the fake, as a run that
// already granted it would have left it.
func seedCollaborator(f *forgejotest.Fake, repo, user, level string) {
	f.SeedCollaborator(repo, user, level)
}

// TestCollaboratorPermissionTable checks the access each role's bot is granted: write
// for the polecat and landing bots, read for the viewer, none for the registry, and an
// error for a role no table names.
func TestCollaboratorPermissionTable(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(testRepo)
	var logs []string
	p := userProvisioner(f, false, &logs)
	if err := p.EnsureCollaborator("polecat", testRepo, "write"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := f.Permission(testRepo, DefaultBotPrefix+"polecat"); got != "write" {
		t.Fatalf("after the first run the bot holds %q, want write", got)
	}
	if n := len(f.Writes()); n != 1 {
		t.Fatalf("first run sent %d writes, want 1", n)
	}
	if err := p.EnsureCollaborator("polecat", testRepo, "write"); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n := len(f.Writes()); n != 1 {
		t.Fatalf("second run sent %d writes in all, want the first run's 1 and no more", n)
	}
	if len(logs) != 2 || !strings.Contains(logs[1], "already has write access") {
		t.Fatalf("second run logged %q, want it to say the bot already has access", logs)
	}
}

// TestEnsureCollaboratorSettlesAtTheRequestedLevel checks the bot ends at the requested
// level whatever it held before. A bot already at the level is left alone, one at a
// higher or lower level is changed to it, and the log states the level read, never the
// requested one.
func TestEnsureCollaboratorSettlesAtTheRequestedLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		held       string
		requested  string
		wantWrites int
		wantLog    string
		notLog     string
	}{
		{name: "absent is granted", requested: "write", wantWrites: 1, wantLog: "granted bot-polecat write access"},
		{name: "present at the requested level sends no write", held: "write", requested: "write",
			wantWrites: 0, wantLog: "already has write access"},
		{name: "present at a higher level is changed down", held: "write", requested: "read",
			wantWrites: 1, wantLog: "changed", notLog: "already has"},
		{name: "present at a lower level is changed up", held: "read", requested: "write",
			wantWrites: 1, wantLog: "changed", notLog: "already has"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := forgejotest.New()
			f.AddRepo(testRepo)
			user := DefaultBotPrefix + "polecat"
			if c.held != "" {
				seedCollaborator(f, testRepo, user, c.held)
			}
			var logs []string
			p := userProvisioner(f, false, &logs)
			if err := p.EnsureCollaborator("polecat", testRepo, c.requested); err != nil {
				t.Fatalf("EnsureCollaborator: %v", err)
			}
			if got := f.Permission(testRepo, user); got != c.requested {
				t.Fatalf("the bot holds %q after the run, want %q", got, c.requested)
			}
			if n := len(f.Writes()); n != c.wantWrites {
				t.Fatalf("sent %d writes, want %d", n, c.wantWrites)
			}
			line := strings.Join(logs, "\n")
			if !strings.Contains(line, c.wantLog) {
				t.Fatalf("logged %q, want a line containing %q", logs, c.wantLog)
			}
			if c.notLog != "" && strings.Contains(line, c.notLog) {
				t.Fatalf("logged %q, which claims the access the bot did not have", logs)
			}
		})
	}
}

// TestEnsureCollaboratorDryRunReportsAChange checks a dry run that finds the bot at
// another level reports the change it would make and sends no write.
func TestEnsureCollaboratorDryRunReportsAChange(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(testRepo)
	user := DefaultBotPrefix + "viewer"
	seedCollaborator(f, testRepo, user, "write")
	var logs []string
	p := userProvisioner(f, true, &logs)
	if err := p.EnsureCollaborator("viewer", testRepo, "read"); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := len(f.Writes()); n != 0 {
		t.Fatalf("dry run sent %d writes, want none", n)
	}
	if got := f.Permission(testRepo, user); got != "write" {
		t.Fatalf("dry run left the bot at %q, want write", got)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "would change") || !strings.Contains(logs[0], "from write to read") {
		t.Fatalf("dry run logged %q, want one would-change line naming the level read", logs)
	}
}

// TestEnsureCollaboratorAcceptsAnyGrantStatus checks that a grant the server answers
// 204, 200 or 201 counts as granted, and that the grant is sent after a 404 read.
func TestEnsureCollaboratorAcceptsAnyGrantStatus(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestEnsureCollaboratorRefusesALevelItCannotRead drives each failing branch of the
// level read that follows a 204. Each refusal stops the run before any write, and none
// of the tokens the reply echoes reaches the error text.
func TestEnsureCollaboratorRefusesALevelItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		level      int
		levelBody  string
		levelDrops bool
		wantErr    string
	}{
		{name: "permission read refused", level: http.StatusInternalServerError, levelBody: echoReply, wantErr: "unexpected HTTP"},
		{name: "reply names no level", level: http.StatusOK, levelBody: echoReply, wantErr: "carries no permission level"},
		{name: "reply is not JSON", level: http.StatusOK, levelBody: "not json", wantErr: "carries no permission level"},
		{name: "permission read gets no reply", levelDrops: true, wantErr: "request failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := &levelTransport{collab: http.StatusNoContent, level: c.level, levelBody: c.levelBody, levelDrops: c.levelDrops}
			var logs []string
			p := userProvisioner(tr, false, &logs)
			err := p.EnsureCollaborator("polecat", testRepo, "write")
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("EnsureCollaborator error = %v, want one containing %q", err, c.wantErr)
			}
			if strings.Contains(err.Error(), fixtureToken) || strings.Contains(err.Error(), mintedSecret) {
				t.Fatal("error text carries a token the reply echoed")
			}
			if !slices.Equal(tr.methods, []string{"GET", "GET"}) {
				t.Fatalf("requests %v, want the access read and the level read only", tr.methods)
			}
			if len(logs) != 0 {
				t.Fatalf("logged %q for a level that was not read, want no line", logs)
			}
		})
	}
}

// TestEnsureCollaboratorStopsOnTransportFailure checks a read that never reaches the
// server is returned as the client's transport error, and no grant follows it.
func TestEnsureCollaboratorStopsOnTransportFailure(t *testing.T) {
	t.Parallel()
	var logs []string
	p := userProvisioner(failingTransport{}, false, &logs)
	err := p.EnsureCollaborator("polecat", testRepo, "write")
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("EnsureCollaborator error = %v, want the transport failure", err)
	}
}

// TestEnsureCollaboratorStopsOnAFailedGrant checks a grant whose request gets no reply
// is returned as the client's transport error and logs no grant.
func TestEnsureCollaboratorStopsOnAFailedGrant(t *testing.T) {
	t.Parallel()
	tr := &statusTransport{status: map[string]int{"GET": http.StatusNotFound}}
	var logs []string
	p := userProvisioner(createDropsTransport{next: tr}, false, &logs)
	err := p.EnsureCollaborator("polecat", testRepo, "write")
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("EnsureCollaborator error = %v, want the transport failure", err)
	}
	if len(logs) != 0 {
		t.Fatalf("logged %q for a grant that failed, want no line", logs)
	}
}

// TestEnsureCollaboratorRefusesAMalformedRepo checks a repository that is not two plain
// names is refused before any request is sent. Each error names the part at fault.
func TestEnsureCollaboratorRefusesAMalformedRepo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		repo    string
		wantErr string
	}{
		{"acme", "owner/name"},
		{"", "owner/name"},
		{"/rig", "owner is not"},
		{"acme/", "name is not"},
		{"../x", "owner is not"},
		{"x/..", "name is not"},
		{"x/.", "name is not"},
		{"a/b/c", "name is not"},
		{"%2f", "owner/name"},
		{"x/%2f", "name is not"},
		{"x y/rig", "owner is not"},
	}
	for _, c := range cases {
		t.Run(c.repo, func(t *testing.T) {
			f := forgejotest.New()
			var logs []string
			p := userProvisioner(f, false, &logs)
			err := p.EnsureCollaborator("polecat", c.repo, "write")
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("EnsureCollaborator error = %v, want one containing %q", err, c.wantErr)
			}
			if n := len(f.CallLog()); n != 0 {
				t.Fatalf("sent %d requests for a malformed repository, want none", n)
			}
		})
	}
}

// TestEnsureCollaboratorAcceptsPlainRepoNames checks the plain names the request path
// carries are granted, the dot and underscore and dash among them.
func TestEnsureCollaboratorAcceptsPlainRepoNames(t *testing.T) {
	t.Parallel()
	for _, repo := range []string{"a/b", "good-owner.v2/rig_1-x"} {
		t.Run(repo, func(t *testing.T) {
			f := forgejotest.New()
			f.AddRepo(repo)
			var logs []string
			p := userProvisioner(f, false, &logs)
			if err := p.EnsureCollaborator("polecat", repo, "write"); err != nil {
				t.Fatalf("EnsureCollaborator(%q): %v", repo, err)
			}
			if got := f.Permission(repo, DefaultBotPrefix+"polecat"); got != "write" {
				t.Fatalf("the bot holds %q on %s, want write", got, repo)
			}
		})
	}
}

// TestEnsureCollaboratorDryRunSendsNoWrite checks a dry run reads the access, reports
// the grant it would make, and sends no write.
func TestEnsureCollaboratorDryRunSendsNoWrite(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(testRepo)
	var logs []string
	p := userProvisioner(f, true, &logs)
	if err := p.EnsureCollaborator("polecat", testRepo, "write"); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := len(f.Writes()); n != 0 {
		t.Fatalf("dry run sent %d writes, want none", n)
	}
	if got := f.Permission(testRepo, DefaultBotPrefix+"polecat"); got != "" {
		t.Fatalf("dry run granted %q, want no grant", got)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "would grant") {
		t.Fatalf("dry run logged %q, want one would-grant line", logs)
	}
}

// TestEnsureViewerAccessOnlyWhenTheViewerExistsAndIsNotRequested checks the viewer's
// read grant lands only when the viewer bot exists and the run does not provision the
// viewer role itself, and that the result says which of those happened. A viewer that
// already holds write is changed to read.
func TestEnsureViewerAccessOnlyWhenTheViewerExistsAndIsNotRequested(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		viewerExists bool
		held         string
		requested    bool
		want         ViewerAccess
		wantLevel    string
	}{
		{name: "exists and not requested grants read", viewerExists: true, want: ViewerGranted, wantLevel: "read"},
		{name: "exists holding write is changed to read", viewerExists: true, held: "write", want: ViewerGranted, wantLevel: "read"},
		{name: "requested is left to the role loop", viewerExists: true, requested: true, want: ViewerNotNeeded},
		{name: "missing user is skipped", want: ViewerSkipped},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := forgejotest.New()
			f.AddRepo(testRepo)
			if c.viewerExists {
				withBot(t, f, "viewer")
			}
			if c.held != "" {
				seedCollaborator(f, testRepo, DefaultBotPrefix+"viewer", c.held)
			}
			before := len(f.CallLog())
			var logs []string
			p := userProvisioner(f, false, &logs)
			p.ViewerRequested = c.requested
			got, err := p.EnsureViewerAccess(testRepo)
			if err != nil {
				t.Fatalf("EnsureViewerAccess: %v", err)
			}
			if got != c.want {
				t.Fatalf("EnsureViewerAccess = %v, want %v", got, c.want)
			}
			if level := f.Permission(testRepo, DefaultBotPrefix+"viewer"); level != c.wantLevel {
				t.Fatalf("viewer holds %q on %s, want %q", level, testRepo, c.wantLevel)
			}
			if c.requested && len(f.CallLog()) != before {
				t.Fatalf("a run that provisions the viewer sent %d requests, want none", len(f.CallLog())-before)
			}
			if got == ViewerSkipped && !strings.Contains(strings.Join(logs, "\n"), "skipping") {
				t.Fatalf("missing viewer logged %q, want a skip notice", logs)
			}
		})
	}
}

// TestEnsureViewerAccessRefusesWhatItCannotRead checks an unexpected status on the
// viewer's user read is an error, and no grant is sent after it.
func TestEnsureViewerAccessRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	tr := &statusTransport{status: map[string]int{"GET": http.StatusInternalServerError}}
	var logs []string
	p := userProvisioner(tr, false, &logs)
	_, err := p.EnsureViewerAccess(testRepo)
	if err == nil || !strings.Contains(err.Error(), "unexpected HTTP") {
		t.Fatalf("EnsureViewerAccess error = %v, want an unexpected-status error", err)
	}
	if !slices.Equal(tr.methods, []string{"GET"}) {
		t.Fatalf("requests %v, want only the read of the viewer user", tr.methods)
	}
}

// TestEnsureViewerAccessStopsOnTransportFailure checks a viewer user read that never
// reaches the server is returned as the client's transport error.
func TestEnsureViewerAccessStopsOnTransportFailure(t *testing.T) {
	t.Parallel()
	var logs []string
	p := userProvisioner(failingTransport{}, false, &logs)
	_, err := p.EnsureViewerAccess(testRepo)
	if err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("EnsureViewerAccess error = %v, want the transport failure", err)
	}
}

// TestEnsureViewerAccessReturnsAFailedGrant checks a read grant the server refuses is
// returned as its error, so the run cannot carry on as though the viewer could read.
func TestEnsureViewerAccessReturnsAFailedGrant(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(testRepo)
	withBot(t, f, "viewer")
	f.RepoMissing = true
	var logs []string
	p := userProvisioner(f, false, &logs)
	if _, err := p.EnsureViewerAccess(testRepo); err == nil || !strings.Contains(err.Error(), "unexpected HTTP") {
		t.Fatalf("EnsureViewerAccess error = %v, want the refused grant", err)
	}
}
