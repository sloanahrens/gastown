package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListUserRepos(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: golden(t, "user_repos.json")}
	repos, err := newTestClient(t, rec).ListUserRepos(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "/api/v1/user/repos", rec.req.URL.Path)
	assert.Equal(t, "100", rec.req.URL.Query().Get("limit"))
	assert.Equal(t, []Repository{{FullName: "sloan/beads"}, {FullName: "sloan/mango"}}, repos)
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
