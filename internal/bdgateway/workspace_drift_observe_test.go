package bdgateway

import (
	"context"
	"net/http"
	"testing"
)

// TestWorkspaceDriftObservesAnswer: every answer to a push is observed, refusals
// included, with the hash header it carried, whether it says the workspace is not
// present, and whether the push was the reconcile loop's own. Only the refusal
// that names the absent workspace is absent.
func TestWorkspaceDriftObservesAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		ctx    context.Context
		status int
		header string
		body   string
		want   WorkspaceAnswer
	}{
		{
			name:   "a stored push reports the live hash",
			ctx:    context.Background(),
			status: http.StatusOK,
			header: "live",
			body:   `{"manifest_sha":"live"}`,
			want:   WorkspaceAnswer{Hash: "live"},
		},
		{
			name:   "the reconcile loop's own push is observed as own",
			ctx:    AsOwnPush(context.Background()),
			status: http.StatusOK,
			header: "live",
			body:   `{"manifest_sha":"live"}`,
			want:   WorkspaceAnswer{Hash: "live", Own: true},
		},
		{
			name:   "a refusal that the workspace is not present is absent",
			ctx:    context.Background(),
			status: http.StatusBadRequest,
			header: "live",
			body:   `{"code":"bad_request","message":"bdgateway: the workspace is not present: no tree"}`,
			want:   WorkspaceAnswer{Hash: "live", Absent: true},
		},
		{
			name:   "a bad request for another reason is not absent",
			ctx:    context.Background(),
			status: http.StatusBadRequest,
			header: "live",
			body:   `{"code":"bad_request","message":"bdgateway: the rig is refused"}`,
			want:   WorkspaceAnswer{Hash: "live"},
		},
		{
			name:   "a refusal that is not JSON is not absent",
			ctx:    context.Background(),
			status: http.StatusBadRequest,
			header: "live",
			body:   "the workspace is not present",
			want:   WorkspaceAnswer{Hash: "live"},
		},
		{
			name:   "an answer with no header reports no hash",
			ctx:    context.Background(),
			status: http.StatusOK,
			header: "",
			body:   `{"manifest_sha":"x"}`,
			want:   WorkspaceAnswer{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var seen []WorkspaceAnswer
			pusher := GatewayPusher{
				URL:       "http://127.0.0.1:7440",
				TokenFile: clientToken(t, "s3cret"),
				HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
					resp := jsonResponse(tt.status, tt.body)
					if tt.header != "" {
						resp.Header.Set(WorkspaceHashHeader, tt.header)
					}
					return resp, nil
				}),
				Observe: func(a WorkspaceAnswer) { seen = append(seen, a) },
			}
			_, _ = pusher.Push(tt.ctx, clientTown(t))
			if len(seen) != 1 {
				t.Fatalf("observed %d answers, want 1", len(seen))
			}
			if seen[0] != tt.want {
				t.Errorf("observed %+v, want %+v", seen[0], tt.want)
			}
		})
	}
}
