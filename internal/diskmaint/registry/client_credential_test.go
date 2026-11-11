package registry

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"
)

// TestCredentialFollowsTheClientsOrigin: a client with a Credential sends it to
// the origin Base names and to no other, the way an upload location on another
// origin is reached. A client with no Credential sends none.
func TestCredentialFollowsTheClientsOrigin(t *testing.T) {
	t.Parallel()
	const user, token = "viewer", "viewer-token"
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
	cred := &Credential{User: user, Token: token}
	tests := []struct {
		name   string
		cred   *Credential
		target string
		want   string
	}{
		{name: "own origin carries the login", cred: cred, target: "http://registry.test/v2/", want: basic},
		{name: "other origin goes without it", cred: cred, target: "http://elsewhere.test/v2/", want: ""},
		{name: "other scheme goes without it", cred: cred, target: "https://registry.test/v2/", want: ""},
		{name: "no credential sends none", cred: nil, target: "http://registry.test/v2/", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got string
			handler := func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusOK)
			}
			c := &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: roundTrip(handler)}, Credential: tc.cred}
			u, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			resp, done, err := c.exchange(context.Background(), http.MethodGet, u, nil, "", 0)
			if err != nil {
				t.Fatalf("exchange: %v", err)
			}
			_ = resp.Body.Close()
			done()
			if got != tc.want {
				t.Fatalf("Authorization = %q, want %q", got, tc.want)
			}
		})
	}
}
