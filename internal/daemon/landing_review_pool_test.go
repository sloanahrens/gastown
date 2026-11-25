package daemon

import (
	"bytes"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
)

// reviewPoolTestLander and reviewPoolTestWorker are fresh collaborators for
// each call: a second construction is the value the off paths must leave in
// place, so any field the wiring touches makes the compare unequal.
func reviewPoolTestLander() *land.Lander {
	return &land.Lander{
		Repo:     "/sentinel/repo",
		Remote:   "sentinel",
		WorkRoot: "/sentinel/work",
		Route:    "sentinel",
		TownRoot: "/sentinel/town",
		BeadsDir: "/sentinel/beads",
	}
}

func reviewPoolTestWorker() *landworker.Worker {
	return &landworker.Worker{
		Rig:         "sentinel",
		TownRoot:    "/sentinel/town",
		WatchTarget: "main",
		LandTimeout: 7 * time.Minute,
	}
}

func TestReviewPoolWiringOff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *config.LandingWorkerConfig
	}{
		{name: "no review_pool key", cfg: &config.LandingWorkerConfig{}},
		{name: "rig not listed", cfg: &config.LandingWorkerConfig{
			ReviewPool: &config.ReviewPoolConfig{Rigs: []string{"other"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0)}
			lander := reviewPoolTestLander()
			worker := reviewPoolTestWorker()

			d.wireReviewPool("gastown", tc.cfg, lander, worker, "/sentinel/reviews")

			if got := logs.String(); got != "" {
				t.Fatalf("pool off for the rig logged %q; want nothing", got)
			}
			if want := reviewPoolTestLander(); !reflect.DeepEqual(lander, want) {
				t.Fatalf("pool off for the rig changed the Lander:\ngot  %+v\nwant %+v", lander, want)
			}
			if want := reviewPoolTestWorker(); !reflect.DeepEqual(worker, want) {
				t.Fatalf("pool off for the rig changed the Worker:\ngot  %+v\nwant %+v", worker, want)
			}
		})
	}
}

func TestReviewPoolWiringBadConfigStaysOff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pool    *config.ReviewPoolConfig
		wantKey string
	}{
		{name: "unparseable expiry", wantKey: "reuse_expiry",
			pool: &config.ReviewPoolConfig{Rigs: []string{"gastown"}, ReuseExpiryStr: "soon"}},
		{name: "non-positive expiry", wantKey: "reuse_expiry",
			pool: &config.ReviewPoolConfig{Rigs: []string{"gastown"}, ReuseExpiryStr: "0s"}},
		{name: "expiry over the maximum", wantKey: "reuse_expiry",
			pool: &config.ReviewPoolConfig{Rigs: []string{"gastown"}, ReuseExpiryStr: "48h"}},
		{name: "max_concurrent out of range", wantKey: "max_concurrent",
			pool: &config.ReviewPoolConfig{Rigs: []string{"gastown"}, MaxConcurrent: 9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0)}
			lander := reviewPoolTestLander()
			worker := reviewPoolTestWorker()

			d.wireReviewPool("gastown", &config.LandingWorkerConfig{ReviewPool: tc.pool}, lander, worker, "/sentinel/reviews")

			got := logs.String()
			if n := strings.Count(got, "\n"); n != 1 {
				t.Fatalf("bad config logged %d lines, want one naming the rig and the error; log:\n%s", n, got)
			}
			if !strings.Contains(got, "gastown") || !strings.Contains(got, tc.wantKey) {
				t.Fatalf("log line %q does not name the rig and the %s key", got, tc.wantKey)
			}
			if want := reviewPoolTestLander(); !reflect.DeepEqual(lander, want) {
				t.Fatalf("bad config changed the Lander:\ngot  %+v\nwant %+v", lander, want)
			}
			if want := reviewPoolTestWorker(); !reflect.DeepEqual(worker, want) {
				t.Fatalf("bad config changed the Worker:\ngot  %+v\nwant %+v", worker, want)
			}
		})
	}
}
