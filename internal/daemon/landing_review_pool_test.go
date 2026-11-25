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

func TestReviewPoolOMConfigPath(t *testing.T) {
	t.Parallel()
	const xdgConfig = "/xdg/config"
	const home = "/fixture/tester"
	cases := []struct {
		name    string
		env     map[string]string
		home    string
		want    string
		wantErr bool
	}{
		{
			name: "XDG set",
			env:  map[string]string{"XDG_CONFIG_HOME": xdgConfig},
			home: home,
			want: "/xdg/config/om/config.json",
		},
		{
			name: "XDG set with empty home",
			env:  map[string]string{"XDG_CONFIG_HOME": xdgConfig},
			want: "/xdg/config/om/config.json",
		},
		{
			name: "XDG set but empty",
			env:  map[string]string{"XDG_CONFIG_HOME": ""},
			home: home,
			want: "/fixture/tester/.config/om/config.json",
		},
		{
			name:    "XDG set but empty, no home",
			env:     map[string]string{"XDG_CONFIG_HOME": ""},
			wantErr: true,
		},
		{
			name: "XDG unset",
			home: home,
			want: "/fixture/tester/.config/om/config.json",
		},
		{
			name:    "no XDG and no home",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lookup := func(key string) (string, bool) {
				v, ok := tc.env[key]
				return v, ok
			}

			got, err := omConfigPath(lookup, tc.home)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("omConfigPath() = %q, nil; want an error naming the missing location", got)
				}
				for _, want := range []string{"XDG_CONFIG_HOME", "home"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q does not name %q", err, want)
					}
				}
				if got != "" {
					t.Fatalf("omConfigPath() returned the path %q with the error; want none", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("omConfigPath() error = %v; want %q", err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("omConfigPath() = %q; want %q", got, tc.want)
			}
		})
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
