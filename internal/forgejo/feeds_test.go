package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListUserRepos(t *testing.T) {
	t.Parallel()
	// The handler runs on the server's own goroutine, so the slices it records
	// are guarded by mu and read under it on the test goroutine.
	var mu sync.Mutex
	var paths, pages, limits []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		pages = append(pages, r.URL.Query().Get("page"))
		limits = append(limits, r.URL.Query().Get("limit"))
		mu.Unlock()
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write(golden(t, "user_repos.json"))
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	repos, err := newTestClient(t, h).ListUserRepos(context.Background())
	require.NoError(t, err)

	mu.Lock()
	gotPaths, gotPages, gotLimits := paths, pages, limits
	mu.Unlock()
	assert.Equal(t, []string{"/api/v1/user/repos", "/api/v1/user/repos"}, gotPaths)
	assert.Equal(t, []string{"1", "2"}, gotPages)
	assert.Equal(t, []string{"100", "100"}, gotLimits)
	assert.Equal(t, []Repository{{FullName: "sloan/beads"}, {FullName: "sloan/mango"}}, repos)
}

// A viewer with more repositories than one page holds is read to its end: a
// report built from the first page alone would leave every later repository
// out (gt-nlrpk).
func TestListUserReposReadsEveryPage(t *testing.T) {
	t.Parallel()
	full := make([]Repository, userReposLimit)
	for i := range full {
		full[i] = Repository{FullName: fmt.Sprintf("sloan/repo-%d", i)}
	}
	// The handler runs on the server's own goroutine: pages is guarded by mu,
	// and a handler write failure is recorded for the test goroutine to assert
	// (a require there would abort only the handler's goroutine).
	var mu sync.Mutex
	var pages []string
	var writeErr error
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		mu.Lock()
		pages = append(pages, page)
		mu.Unlock()
		var err error
		switch page {
		case "1":
			err = json.NewEncoder(w).Encode(full)
		case "2":
			err = json.NewEncoder(w).Encode([]Repository{{FullName: "sloan/last"}})
		default:
			_, err = w.Write([]byte("[]"))
		}
		if err != nil {
			mu.Lock()
			if writeErr == nil {
				writeErr = err
			}
			mu.Unlock()
		}
	})
	repos, err := newTestClient(t, h).ListUserRepos(context.Background())
	require.NoError(t, err)

	mu.Lock()
	gotPages := pages
	gotWriteErr := writeErr
	mu.Unlock()
	require.NoError(t, gotWriteErr, "the stub wrote its canned pages")
	assert.Equal(t, []string{"1", "2", "3"}, gotPages, "a short page is not the end: the empty page is")
	require.Len(t, repos, userReposLimit+1)
	assert.Equal(t, "sloan/repo-0", repos[0].FullName)
	assert.Equal(t, "sloan/last", repos[userReposLimit].FullName)
}

// A page the client cannot read fails the whole list: reporting the pages it
// did read as the whole list is what the size report must not be built from
// (gt-nlrpk).
func TestListUserReposPageFailureFails(t *testing.T) {
	t.Parallel()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write(golden(t, "user_repos.json"))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})
	_, err := newTestClient(t, h).ListUserRepos(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

// A server that never runs out of repositories is a fault the read reports,
// not a list it truncates at the guard without saying so (gt-nlrpk).
func TestListUserReposEndlessPagesFails(t *testing.T) {
	t.Parallel()
	full := make([]Repository, userReposLimit)
	for i := range full {
		full[i] = Repository{FullName: fmt.Sprintf("sloan/repo-%d", i)}
	}
	// calls and writeErr are written on the handler's goroutine, so each is
	// guarded by mu and read under it on the test goroutine.
	var mu sync.Mutex
	calls := 0
	var writeErr error
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if err := json.NewEncoder(w).Encode(full); err != nil {
			mu.Lock()
			if writeErr == nil {
				writeErr = err
			}
			mu.Unlock()
		}
	})
	_, err := newTestClient(t, h).ListUserRepos(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrListNeverEnds)

	mu.Lock()
	gotCalls, gotWriteErr := calls, writeErr
	mu.Unlock()
	require.NoError(t, gotWriteErr, "the stub wrote its canned pages")
	// The guard is a bound, not a documented constant: assert the loop stops
	// well before any list a real server would serve, without pinning the test
	// to today's value of userReposMaxPages.
	assert.Greater(t, gotCalls, 1, "the loop read more than one page")
	assert.LessOrEqual(t, gotCalls, 1000, "the paging loop is bounded")
}

func TestListRepoActivities(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: golden(t, "activities.json")}
	acts, err := newTestClient(t, rec).ListRepoActivities(context.Background(), "sloan", "beads", 30)
	require.NoError(t, err)

	assert.Equal(t, "/api/v1/repos/sloan/beads/activities/feeds", rec.req.URL.Path)
	assert.Equal(t, "30", rec.req.URL.Query().Get("limit"))
	require.Len(t, acts, 3)
	assert.Equal(t, "commit_repo", acts[0].OpType)
	assert.Equal(t, "refs/heads/main", acts[0].RefName)
	require.NotNil(t, acts[0].Repo)
	assert.Equal(t, "sloan/beads", acts[0].Repo.FullName)
	require.NotNil(t, acts[0].ActUser)
	assert.Equal(t, "bot-polecat", acts[0].ActUser.Login)
}

// The panel note shows a failed call's error, so the error must not carry the
// token it was made with.
func TestListRepoActivitiesErrorHasNoToken(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, &recorder{status: http.StatusUnauthorized, body: []byte(`{"message":"unauthorized"}`)})
	_, err := c.ListRepoActivities(context.Background(), "sloan", "beads", 30)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "test-token")
}

func TestActivityHeadMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		content string
		want    string
		ok      bool
	}{
		{"head commit", `{"Commits":[],"HeadCommit":{"Sha1":"b","Message":"fix: the thing (gt-1)"},"Len":0}`, "fix: the thing (gt-1)", true},
		{"head wins over the commit list", `{"Commits":[{"Message":"older"}],"HeadCommit":{"Message":"newer"}}`, "newer", true},
		{"newest commit when no head", `{"Commits":[{"Message":"one"},{"Message":"two"}]}`, "two", true},
		{"subject only", `{"HeadCommit":{"Message":"subject\n\nbody line\nmore"}}`, "subject", true},
		{"trimmed", `{"HeadCommit":{"Message":"  spaced  "}}`, "spaced", true},
		{"no commits", `{"Commits":[],"Len":0}`, "", false},
		{"empty content", "", "", false},
		{"not json", "not a blob", "", false},
		{"blank message", `{"HeadCommit":{"Message":"\n\n"}}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Activity{Content: tc.content}.HeadMessage()
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The blob's keys are the ones Forgejo 16.0.5 writes (verified live): the
// capitalized Commits/HeadCommit/Sha1/Message, not the API's snake_case.
func TestActivityHeadMessageMatchesLiveShape(t *testing.T) {
	t.Parallel()
	var acts []Activity
	require.NoError(t, json.Unmarshal(golden(t, "activities.json"), &acts))
	got, ok := acts[0].HeadMessage()
	require.True(t, ok)
	assert.Equal(t, "ci: add the Forgejo gate workflow (be-6px)", got)

	_, ok = acts[1].HeadMessage()
	assert.False(t, ok, "the branch row carries no content blob")
}
