package consoleapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/adminplane/registry"
	"github.com/steveyegge/gastown/internal/adminplane/views"
)

// at is the time every record is created, far enough from the wall clock that a
// response that read time.Now instead is visible.
var at = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

const (
	staffToken  = "staff-token"
	staffEmail  = "operator@example.com"
	verifierErr = "token revoked at 16:02"
	sourceErr   = "record store is down"
)

// The four kinds of field a view refuses. Each value is distinctive so a test
// can search a response body for it.
const (
	codeValue       = "print('deploy')"
	apiKeyValue     = "sk-live-NEVER-SHOWN"
	transcriptValue = "user: show me the workspace"
	beadBodyValue   = "bead body: the plan nobody outside may read"
)

// fakeVerifier is Verifier in memory, refusing while err is set.
type fakeVerifier struct {
	err   error
	calls int
	token string
}

func (v *fakeVerifier) Verify(_ context.Context, token string) (string, error) {
	v.calls++
	v.token = token
	if v.err != nil {
		return "", v.err
	}
	return staffEmail, nil
}

// fakeSource is Source in memory. detailErr fails every Workspace call;
// wrapNotFound reports a missing id as a wrapped ErrNotFound rather than a
// bare one.
type fakeSource struct {
	list         []views.Record
	records      map[string]views.Record
	listErr      error
	detailErr    error
	wrapNotFound bool
}

func (s *fakeSource) Workspaces() ([]views.Record, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.list, nil
}

func (s *fakeSource) Workspace(id string) (views.Record, error) {
	if s.detailErr != nil {
		return views.Record{}, s.detailErr
	}
	rec, ok := s.records[id]
	if !ok {
		if s.wrapNotFound {
			return views.Record{}, fmt.Errorf("store: no workspace %q: %w", id, ErrNotFound)
		}
		return views.Record{}, ErrNotFound
	}
	return rec, nil
}

// record returns a workspace record carrying every field a view may show and
// the four it refuses.
func record(id string) views.Record {
	return views.Record{
		ID:           id,
		Organization: "org-1",
		Owner:        "owner-1",
		State:        registry.Active,
		Created:      at,
		Health:       "healthy",
		Counts:       views.Counts{Beads: 2, Members: 3},
		Beads:        []views.Bead{{ID: "bd-1", Title: "first bead", Body: beadBodyValue}},
		Metering: views.MeteringSummary{
			SeatHours:       []views.ProfileHours{{Profile: "standard", Hours: 12.5}},
			StorageGBMonths: 1.5,
			Tokens:          views.Tokens{Input: 10, Output: 20, CacheCreation: 30, CacheRead: 40},
		},
		Code:       codeValue,
		APIKey:     apiKeyValue,
		Transcript: transcriptValue,
	}
}

// oneSource returns a Source holding rec under its own id.
func oneSource(rec views.Record) *fakeSource {
	return &fakeSource{
		list:    []views.Record{rec},
		records: map[string]views.Record{rec.ID: rec},
	}
}

// request serves one request through h. auth, when set, is sent verbatim as the
// Authorization header.
func request(h http.Handler, method, target, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, want, rec.Body.String())
	}
}

func wantEmptyBody(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

func wantInternalError(t *testing.T, rec *httptest.ResponseRecorder, secret string) {
	t.Helper()
	wantStatus(t, rec, http.StatusInternalServerError)
	if got := rec.Body.String(); got != "internal error" {
		t.Errorf("body = %q, want %q", got, "internal error")
	}
	if secret != "" && strings.Contains(rec.Body.String(), secret) {
		t.Errorf("body holds the internal detail %q: %q", secret, rec.Body.String())
	}
}

func TestHandlerRequiresStaffBearer(t *testing.T) {
	t.Parallel()

	t.Run("no header", func(t *testing.T) {
		t.Parallel()
		v := &fakeVerifier{}
		rec := request(New(oneSource(record("ws-1")), v), http.MethodGet, "/workspaces", "")
		wantStatus(t, rec, http.StatusUnauthorized)
		wantEmptyBody(t, rec)
		if v.calls != 0 {
			t.Errorf("verifier called %d times with no token, want 0", v.calls)
		}
	})

	for name, auth := range map[string]string{
		"another scheme": "Basic " + staffToken,
		"no token":       "Bearer",
		"empty token":    "Bearer ",
		"two tokens":     "Bearer " + staffToken + " extra",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v := &fakeVerifier{}
			rec := request(New(oneSource(record("ws-1")), v), http.MethodGet, "/workspaces", auth)
			wantStatus(t, rec, http.StatusUnauthorized)
			wantEmptyBody(t, rec)
			if v.calls != 0 {
				t.Errorf("verifier called %d times with a malformed header, want 0", v.calls)
			}
		})
	}

	t.Run("verifier refusal", func(t *testing.T) {
		t.Parallel()
		v := &fakeVerifier{err: errors.New(verifierErr)}
		rec := request(New(oneSource(record("ws-1")), v), http.MethodGet, "/workspaces", "Bearer "+staffToken)
		wantStatus(t, rec, http.StatusUnauthorized)
		wantEmptyBody(t, rec)
		if v.calls != 1 || v.token != staffToken {
			t.Errorf("verifier got %d calls with token %q, want 1 call with %q", v.calls, v.token, staffToken)
		}
	})

	t.Run("no verifier configured", func(t *testing.T) {
		t.Parallel()
		rec := request(New(oneSource(record("ws-1")), nil), http.MethodGet, "/workspaces", "Bearer "+staffToken)
		wantStatus(t, rec, http.StatusUnauthorized)
		wantEmptyBody(t, rec)
	})
}

func TestHandlerListAndDetail(t *testing.T) {
	t.Parallel()

	t.Run("list returns the list view", func(t *testing.T) {
		t.Parallel()
		src := oneSource(record("ws-1"))
		v := &fakeVerifier{}
		rec := request(New(src, v), http.MethodGet, "/workspaces", "Bearer "+staffToken)
		wantStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if v.token != staffToken {
			t.Errorf("verifier got token %q, want %q", v.token, staffToken)
		}
		var rows []views.WorkspaceRow
		if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
			t.Fatalf("decode list: %v (body %q)", err, rec.Body.String())
		}
		if len(rows) != 1 {
			t.Fatalf("list returned %d rows, want 1", len(rows))
		}
		if !rows[0].Created.Equal(at) {
			t.Errorf("list created = %v, want %v", rows[0].Created, at)
		}
		want := []views.WorkspaceRow{{
			ID:           "ws-1",
			Organization: "org-1",
			State:        registry.Active,
			Owner:        "owner-1",
			Created:      rows[0].Created,
			Counts:       views.Counts{Beads: 2, Members: 3},
		}}
		if !reflect.DeepEqual(rows, want) {
			t.Errorf("rows = %+v, want %+v", rows, want)
		}
	})

	t.Run("detail returns the detail view", func(t *testing.T) {
		t.Parallel()
		src := oneSource(record("ws-1"))
		rec := request(New(src, &fakeVerifier{}), http.MethodGet, "/workspaces/ws-1", "Bearer "+staffToken)
		wantStatus(t, rec, http.StatusOK)
		var got views.WorkspaceDetail
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode detail: %v (body %q)", err, rec.Body.String())
		}
		if !got.Created.Equal(at) {
			t.Errorf("detail created = %v, want %v", got.Created, at)
		}
		want := views.WorkspaceDetail{
			WorkspaceRow: views.WorkspaceRow{
				ID:           "ws-1",
				Organization: "org-1",
				State:        registry.Active,
				Owner:        "owner-1",
				Created:      got.Created,
				Counts:       views.Counts{Beads: 2, Members: 3},
			},
			Health:   "healthy",
			Beads:    []views.BeadRef{{ID: "bd-1", Title: "first bead"}},
			Metering: src.records["ws-1"].Metering,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("detail = %+v, want %+v", got, want)
		}
	})

	for name, target := range map[string]string{
		"empty id":    "/workspaces/",
		"nested path": "/workspaces/ws-1/extra",
	} {
		t.Run(name+" is not found", func(t *testing.T) {
			t.Parallel()
			rec := request(New(oneSource(record("ws-1")), &fakeVerifier{}), http.MethodGet, target, "Bearer "+staffToken)
			wantStatus(t, rec, http.StatusNotFound)
		})
	}

	for name, wrap := range map[string]bool{"bare": false, "wrapped": true} {
		t.Run("unknown id ("+name+") is not found", func(t *testing.T) {
			t.Parallel()
			src := &fakeSource{records: map[string]views.Record{}, wrapNotFound: wrap}
			rec := request(New(src, &fakeVerifier{}), http.MethodGet, "/workspaces/ws-404", "Bearer "+staffToken)
			wantStatus(t, rec, http.StatusNotFound)
		})
	}

	t.Run("list source error is internal", func(t *testing.T) {
		t.Parallel()
		src := &fakeSource{listErr: errors.New(sourceErr)}
		rec := request(New(src, &fakeVerifier{}), http.MethodGet, "/workspaces", "Bearer "+staffToken)
		wantInternalError(t, rec, sourceErr)
	})

	t.Run("detail source error is internal", func(t *testing.T) {
		t.Parallel()
		src := &fakeSource{detailErr: errors.New(sourceErr)}
		rec := request(New(src, &fakeVerifier{}), http.MethodGet, "/workspaces/ws-1", "Bearer "+staffToken)
		wantInternalError(t, rec, sourceErr)
	})

	t.Run("wrong workspace is internal", func(t *testing.T) {
		t.Parallel()
		// The source answers under the requested key with another workspace's
		// record; showing it as this workspace would be a cross-workspace leak.
		src := &fakeSource{records: map[string]views.Record{"ws-1": record("ws-2")}}
		rec := request(New(src, &fakeVerifier{}), http.MethodGet, "/workspaces/ws-1", "Bearer "+staffToken)
		wantInternalError(t, rec, "ws-2")
	})

	t.Run("no source is internal", func(t *testing.T) {
		t.Parallel()
		rec := request(New(nil, &fakeVerifier{}), http.MethodGet, "/workspaces", "Bearer "+staffToken)
		wantInternalError(t, rec, "")
	})
}

func TestHandlerNeverLeaksRefusedFields(t *testing.T) {
	t.Parallel()
	src := oneSource(record("ws-1"))
	for name, target := range map[string]string{
		"list":   "/workspaces",
		"detail": "/workspaces/ws-1",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := request(New(src, &fakeVerifier{}), http.MethodGet, target, "Bearer "+staffToken)
			wantStatus(t, rec, http.StatusOK)
			body := rec.Body.String()
			for field, value := range map[string]string{
				"code":       codeValue,
				"API key":    apiKeyValue,
				"transcript": transcriptValue,
				"bead body":  beadBodyValue,
			} {
				if strings.Contains(body, value) {
					t.Errorf("%s response holds the %s: %s", name, field, body)
				}
			}
		})
	}
}

func TestHandlerOnlyGet(t *testing.T) {
	t.Parallel()
	for _, method := range []string{
		http.MethodPost,
		http.MethodPut,
		http.MethodDelete,
		http.MethodPatch,
		http.MethodHead,
	} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			h := New(oneSource(record("ws-1")), &fakeVerifier{})
			rec := request(h, method, "/workspaces", "Bearer "+staffToken)
			wantStatus(t, rec, http.StatusMethodNotAllowed)
			if got := rec.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow = %q, want %q", got, http.MethodGet)
			}
			wantEmptyBody(t, rec)
		})
	}
}

// TestWriteJSONRefusesUnmarshalableValue drives the refusal branch a view can
// never reach, since every view field marshals.
func TestWriteJSONRefusesUnmarshalableValue(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writeJSON(rec, make(chan int))
	wantInternalError(t, rec, "")
}
