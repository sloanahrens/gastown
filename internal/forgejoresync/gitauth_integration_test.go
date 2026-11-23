//go:build integration

package forgejoresync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// emptyRepoAdvert is a smart-HTTP upload-pack advertisement of a repository with
// no refs: git ls-remote prints nothing and exits 0.
func emptyRepoAdvert() string {
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	return pkt("# service=git-upload-pack\n") + "0000" +
		pkt(strings.Repeat("0", 40)+" capabilities^{}\x00side-band-64k\n") + "0000"
}

// gitLsRemoteBound bounds each git ls-remote call so a hung remote fails its
// sub-test in about a minute instead of spending the gate's ten, and a bound is
// never raised to land a change (test-engineering standard, points 3 and 10).
const gitLsRemoteBound = 30 * time.Second

// callContext returns how long one call may run: bound, or the time the test has
// left when its deadline is sooner. A zero deadline means the test set none.
func callContext(t *testing.T, bound time.Duration, deadline time.Time) time.Duration {
	t.Helper()
	if deadline.IsZero() {
		return bound
	}
	if remaining := time.Until(deadline); remaining < bound {
		return remaining
	}
	return bound
}

// TestIntegrationGitAuthenticatesWithTheTokenVariablesStripped runs real git
// through gitCommandIn against a server that answers 401 without the right basic
// auth. The environment holds the admin token under both of its names and a
// GITHUB_TOKEN, all of which gitCommandIn strips. Authentication has to survive
// that: git gets its credential from the URL or from a credential helper, never
// from a *_TOKEN variable (the helper, gt git-credential-forgejo, reads a token
// file named by flag). Mutation: a scrub that also drops the GIT_CONFIG_*
// variables turns the helper case red.
func TestIntegrationGitAuthenticatesWithTheTokenVariablesStripped(t *testing.T) {
	t.Parallel()
	var denied, served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "bot" || p != "s3cret-fixture" {
			denied.Add(1)
			w.Header().Set("WWW-Authenticate", `Basic realm="forgejo"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		served.Add(1)
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = w.Write([]byte(emptyRepoAdvert()))
	}))
	defer srv.Close()

	stripped := []string{
		"GT_AUTH_FORGEJO_TOKEN=tok-marker",
		"FORGEJO_ADMIN_TOKEN=tok-marker",
		"GITHUB_TOKEN=tok-marker",
	}
	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1"}

	cases := []struct {
		name     string
		url      string
		extraEnv []string
	}{
		{"credential in the URL", strings.Replace(srv.URL, "http://", "http://bot:s3cret-fixture@", 1) + "/r.git", nil},
		{"credential helper through git's config environment", srv.URL + "/r.git", []string{
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
			"GIT_CONFIG_KEY_1=credential.helper",
			"GIT_CONFIG_VALUE_1=!f() { echo username=bot; echo password=s3cret-fixture; }; f",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := served.Load()
			// A hung call fails this sub-test at the bound, not at the package timeout.
			deadline, _ := t.Deadline() // zero when the test set none
			ctx, cancel := context.WithTimeout(t.Context(), callContext(t, gitLsRemoteBound, deadline))
			defer cancel()
			environ := append(append(append([]string{}, base...), stripped...), tc.extraEnv...)
			cmd := gitCommandIn(ctx, environ, "git", "ls-remote", "--", tc.url)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git ls-remote failed with the token variables stripped: %v\n%s", err, out)
			}
			if served.Load() == before {
				t.Fatalf("the server never saw the credential (denied %d)", denied.Load())
			}
		})
	}
}

// TestIntegrationCallContextCutsTheBoundToATestDeadline pins callContext's cut: the bound
// when the test set no deadline, the remaining time when the deadline is sooner,
// and the bound when it is later. The distances are fractions of the bound, so
// the constant holds the file's only whole-second literal.
func TestIntegrationCallContextCutsTheBoundToATestDeadline(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		away time.Duration // zero: the test set no deadline
		want time.Duration
	}{
		{"no test deadline", 0, gitLsRemoteBound},
		{"deadline sooner than the bound", gitLsRemoteBound / 6, gitLsRemoteBound / 6}, // five seconds
		{"deadline later than the bound", gitLsRemoteBound * 20, gitLsRemoteBound},     // ten minutes
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var deadline time.Time
			if tc.away != 0 {
				deadline = time.Now().Add(tc.away)
			}
			got := callContext(t, gitLsRemoteBound, deadline)
			// Setting the deadline costs a little, so the remaining time is a hair
			// under the distance; the tenth leaves room for a delayed goroutine.
			if got > tc.want || got < tc.want-tc.want/10 {
				t.Fatalf("callContext(bound, deadline %v away) = %v, want about %v", tc.away, got, tc.want)
			}
		})
	}
}
