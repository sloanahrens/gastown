package forgejotest

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// RepoFake answers the three routes a run that creates and seeds a repository calls,
// as an in-process http.RoundTripper that opens no socket.
type RepoFake struct {
	// RepoStatus answers the read of the repository; the default 404 means it does not exist.
	RepoStatus int
	// CreateStatus answers the admin create of the repository.
	CreateStatus int
	// SeedStatus answers the contents commit.
	SeedStatus int
	// SeedReply is the body the contents commit answers.
	SeedReply string
	// Down fails every request at the transport, as an unreachable Forgejo does.
	Down bool
	// WritesDown fails every request but a GET at the transport.
	WritesDown bool

	owner, name string
	mu          sync.Mutex
	reqs        []RepoRequest
}

// RepoRequest is one call the fake received: its method, request path, Authorization
// header and body.
type RepoRequest struct {
	Method string
	Path   string
	Auth   string
	Body   []byte
}

// NewRepoFake starts a fake for owner/name whose routes answer as a healthy Forgejo does,
// with the overrides a test sets before the run.
func NewRepoFake(owner, name string) *RepoFake {
	return &RepoFake{
		RepoStatus:   http.StatusNotFound,
		CreateStatus: http.StatusCreated,
		SeedStatus:   http.StatusCreated,
		SeedReply:    `{"commit":{"sha":"abc123"}}`,
		owner:        owner,
		name:         name,
	}
}

// API is the root a client reaches the fake at.
func (f *RepoFake) API() string { return APIRoot + APIPath }

// RoundTrip answers one request in-process, recording it for the test.
func (f *RepoFake) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.Down || (f.WritesDown && r.Method != http.MethodGet) {
		return nil, errors.New("connection refused")
	}
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, RepoRequest{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: body})
	f.mu.Unlock()
	rec := httptest.NewRecorder()
	f.serve(rec, r)
	return rec.Result(), nil
}

// serve answers the read, the admin create and the contents commit for owner/name; any
// other route is a 404, so a call the run should not make fails loudly.
func (f *RepoFake) serve(w http.ResponseWriter, r *http.Request) {
	repoPath := APIPath + "/repos/" + f.owner + "/" + f.name
	switch {
	case r.Method == http.MethodGet && r.URL.Path == repoPath:
		w.WriteHeader(f.RepoStatus)
	case r.Method == http.MethodPost && r.URL.Path == APIPath+"/admin/users/"+f.owner+"/repos":
		w.WriteHeader(f.CreateStatus)
		_, _ = io.WriteString(w, `{"name":"`+f.name+`"}`)
	case r.Method == http.MethodPost && r.URL.Path == repoPath+"/contents":
		w.WriteHeader(f.SeedStatus)
		_, _ = io.WriteString(w, f.SeedReply)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// Requests returns a copy of every call received, in order.
func (f *RepoFake) Requests() []RepoRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RepoRequest(nil), f.reqs...)
}

// Writes returns the calls that are not GET.
func (f *RepoFake) Writes() []RepoRequest {
	var out []RepoRequest
	for _, r := range f.Requests() {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}
