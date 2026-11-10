package stateapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

const testToken = "tok-0123456789"

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// fixedToken returns a token func that always answers v.
func fixedToken(v string) func() string {
	return func() string { return v }
}

// serve sends one request through s in-process and returns the recorded
// response. auth is the full Authorization header value, or empty for none.
func serve(s *Server, method, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// okHandler answers 200 with the body "ok", for routes a test registers.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok"))
})

func TestServerRefusesNonGet(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, path := range []string{"/v1/info", "/healthz"} {
			t.Run(method+" "+path, func(t *testing.T) {
				s := NewServer("alpha", fixedToken(testToken))
				rec := serve(s, method, path, "")
				assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
				assert.Equal(t, http.MethodGet, rec.Header().Get("Allow"))
			})
		}
	}
}

func TestServerAuth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token func() string
		path  string
		auth  string
		want  int
	}{
		{"missing header refused on info", fixedToken(testToken), "/v1/info", "", http.StatusUnauthorized},
		{"wrong token refused on info", fixedToken(testToken), "/v1/info", "Bearer tok-nope", http.StatusUnauthorized},
		{"other scheme refused on info", fixedToken(testToken), "/v1/info", "Basic " + testToken, http.StatusUnauthorized},
		{"bearer without a token refused", fixedToken(testToken), "/v1/info", "Bearer ", http.StatusUnauthorized},
		{"empty configured token refuses a presented token", fixedToken(""), "/v1/info", "Bearer " + testToken, http.StatusUnauthorized},
		{"empty configured token refuses an empty bearer", fixedToken(""), "/v1/info", "Bearer ", http.StatusUnauthorized},
		{"nil token func refuses a presented token", nil, "/v1/info", "Bearer " + testToken, http.StatusUnauthorized},
		{"right token admitted on info", fixedToken(testToken), "/v1/info", "Bearer " + testToken, http.StatusOK},
		{"healthz needs no header", fixedToken(testToken), "/healthz", "", http.StatusOK},
		{"healthz open with an empty configured token", fixedToken(""), "/healthz", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewServer("alpha", tt.token)
			rec := serve(s, http.MethodGet, tt.path, tt.auth)
			assert.Equal(t, tt.want, rec.Code)
		})
	}
}

func TestServerRefusalBodyHasNoDetail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token func() string
		auth  string
	}{
		{"missing header", fixedToken(testToken), ""},
		{"wrong token", fixedToken(testToken), "Bearer tok-nope"},
		{"empty configured token", fixedToken(""), "Bearer " + testToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewServer("alpha", tt.token)
			rec := serve(s, http.MethodGet, "/v1/info", tt.auth)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
		})
	}
}

func TestServerReadsTokenPerRequest(t *testing.T) {
	t.Parallel()
	current := "tok-first-0001"
	s := NewServer("alpha", func() string { return current })
	require.Equal(t, http.StatusOK, serve(s, http.MethodGet, "/v1/info", "Bearer tok-first-0001").Code)

	current = "tok-second-0002"

	assert.Equal(t, http.StatusUnauthorized, serve(s, http.MethodGet, "/v1/info", "Bearer tok-first-0001").Code)
	assert.Equal(t, http.StatusOK, serve(s, http.MethodGet, "/v1/info", "Bearer tok-second-0002").Code)
}

func TestServerInfo(t *testing.T) {
	t.Parallel()
	s := NewServer("alpha", fixedToken(testToken))
	rec := serve(s, http.MethodGet, "/v1/info", "Bearer "+testToken)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"town":"alpha","api":1,"cursor":"gen:offset"}`, rec.Body.String())
}

func TestServerHealthz(t *testing.T) {
	t.Parallel()
	s := NewServer("alpha", fixedToken(testToken))
	rec := serve(s, http.MethodGet, "/healthz", "")
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

func TestServerUnknownRouteIs404(t *testing.T) {
	t.Parallel()
	s := NewServer("alpha", fixedToken(testToken))
	rec := serve(s, http.MethodGet, "/v1/nope", "Bearer "+testToken)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHandleRoutesBehindToken(t *testing.T) {
	t.Parallel()
	s := NewServer("alpha", fixedToken(testToken))
	s.Handle("/v1/zeta", okHandler)

	assert.Equal(t, http.StatusUnauthorized, serve(s, http.MethodGet, "/v1/zeta", "").Code)

	rec := serve(s, http.MethodGet, "/v1/zeta", "Bearer "+testToken)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}

func TestServerRoutesSorted(t *testing.T) {
	t.Parallel()
	s := NewServer("alpha", fixedToken(testToken))
	s.Handle("/v1/zeta", okHandler)
	s.Handle("/v1/alpha", okHandler)
	assert.Equal(t, []string{"/healthz", "/v1/alpha", "/v1/info", "/v1/zeta"}, s.Routes())
}

func TestHandleRejectsDuplicatePath(t *testing.T) {
	t.Parallel()
	s := NewServer("alpha", fixedToken(testToken))
	s.Handle("/v1/zeta", okHandler)
	assert.Panics(t, func() { s.Handle("/v1/zeta", okHandler) })
}
