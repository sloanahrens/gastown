package bdgateway

import (
	"context"
	"net/http"
	"testing"
)

// TestGatewayPusherObservesAnswer: every answer to a push is observed, refusals
// included, with the hash header it carried and whether it says the workspace is
// not present. Only the refusal that names the absent workspace is absent.
func TestGatewayPusherObservesAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		header string
		body   string
		want   WorkspaceAnswer
	}{
		{
			name:   "a stored push reports the live hash",
			status: http.StatusOK,
			header: "live",
			body:   `{"manifest_sha":"live"}`,
			want:   WorkspaceAnswer{Hash: "live"},
		},
		{
			name:   "a refusal that the workspace is not present is absent",
			status: http.StatusBadRequest,
			header: "live",
			body:   `{"code":"bad_request","message":"bdgateway: the workspace is not present: no tree"}`,
			want:   WorkspaceAnswer{Hash: "live", Absent: true},
		},
		{
			name:   "a bad request for another reason is not absent",
			status: http.StatusBadRequest,
			header: "live",
			body:   `{"code":"bad_request","message":"bdgateway: the rig is refused"}`,
			want:   WorkspaceAnswer{Hash: "live"},
		},
		{
			name:   "a refusal that is not JSON is not absent",
			status: http.StatusBadRequest,
			header: "live",
			body:   "the workspace is not present",
			want:   WorkspaceAnswer{Hash: "live"},
		},
		{
			name:   "an answer with no header reports no hash",
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
			_, _ = pusher.Push(context.Background(), clientTown(t))
			if len(seen) != 1 {
				t.Fatalf("observed %d answers, want 1", len(seen))
			}
			if seen[0] != tt.want {
				t.Errorf("observed %+v, want %+v", seen[0], tt.want)
			}
		})
	}
}
