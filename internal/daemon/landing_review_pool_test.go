package daemon

import (
	"bytes"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
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

// reviewPoolTestEnv is an environment the off paths must not read: a read
// fails the test, so a wiring that resolved om's config with the pool off
// would be caught here (gt-aovgi3).
func reviewPoolTestEnv(t *testing.T) reviewPoolEnv {
	t.Helper()
	return reviewPoolEnv{
		lookup: func(key string) (string, bool) {
			t.Fatalf("the pool-off path read the environment variable %q", key)
			return "", false
		},
		home: func() (string, error) {
			t.Fatal("the pool-off path read the home directory")
			return "", nil
		},
	}
}

// reviewPoolTestOMConfig puts om's config file under a fresh XDG directory and
// returns the environment naming it plus the file's path, which the on path
// requires to exist (gt-lfykjq).
func reviewPoolTestOMConfig(t *testing.T, home func() (string, error)) (reviewPoolEnv, string) {
	t.Helper()
	configHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configHome, "om"), 0o700); err != nil {
		t.Fatalf("make om's config directory: %v", err)
	}
	omConfig := filepath.Join(configHome, "om", "config.json")
	if err := os.WriteFile(omConfig, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write om's config file: %v", err)
	}
	return reviewPoolEnv{
		lookup: func(key string) (string, bool) {
			if key == "XDG_CONFIG_HOME" {
				return configHome, true
			}
			return "", false
		},
		home: home,
	}, omConfig
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

			if err := d.wireReviewPool("gastown", tc.cfg, lander, worker, "/sentinel/reviews", reviewPoolTestEnv(t)); err != nil {
				t.Fatalf("pool off for the rig returned %v; want nil", err)
			}

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

// TestReviewPoolWiringOffLeavesTheWorker pins that the off path gives the
// Worker no review pool at all: Pool and Stored stay nil, so a landing for a
// rig without the pool runs no queue-time review and sweeps no stored verdict.
// Every way the pool reads as off is a row, and none of them reads om's config,
// which reviewPoolTestEnv fails the test on (gt-1xc93t).
func TestReviewPoolWiringOffLeavesTheWorker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *config.LandingWorkerConfig
	}{
		{name: "no review_pool key", cfg: &config.LandingWorkerConfig{}},
		{name: "rig not listed", cfg: &config.LandingWorkerConfig{
			ReviewPool: &config.ReviewPoolConfig{Rigs: []string{"other"}},
		}},
		{name: "unparseable reuse_expiry", cfg: &config.LandingWorkerConfig{
			ReviewPool: &config.ReviewPoolConfig{Rigs: []string{"gastown"}, ReuseExpiryStr: "soon"},
		}},
		{name: "max_concurrent out of range", cfg: &config.LandingWorkerConfig{
			ReviewPool: &config.ReviewPoolConfig{Rigs: []string{"gastown"}, MaxConcurrent: 9},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0)}
			worker := reviewPoolTestWorker()

			if err := d.wireReviewPool("gastown", tc.cfg, reviewPoolTestLander(), worker, "/sentinel/reviews", reviewPoolTestEnv(t)); err != nil {
				t.Fatalf("pool off for the rig returned %v; want nil, the pool off", err)
			}

			if worker.Pool != nil {
				t.Fatalf("pool off for the rig gave the Worker Pool %+v; want nil so no review runs before the landing", worker.Pool)
			}
			if worker.Stored != nil {
				t.Fatalf("pool off for the rig gave the Worker Stored %+v; want nil so no stored verdict is swept", worker.Stored)
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

			if err := d.wireReviewPool("gastown", &config.LandingWorkerConfig{ReviewPool: tc.pool}, lander, worker, "/sentinel/reviews", reviewPoolTestEnv(t)); err != nil {
				t.Fatalf("bad config returned %v; want nil, the pool off", err)
			}

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

func TestReviewPoolWiringOnSetsTheLander(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		pool       *config.ReviewPoolConfig
		wantOn     bool
		wantExpiry time.Duration
	}{
		{
			name:       "listed rig gets the default expiry",
			pool:       &config.ReviewPoolConfig{Rigs: []string{"gastown"}},
			wantOn:     true,
			wantExpiry: 45 * time.Minute,
		},
		{
			name:       "listed rig gets its configured expiry",
			pool:       &config.ReviewPoolConfig{Rigs: []string{"other", "gastown"}, ReuseExpiryStr: "20m"},
			wantOn:     true,
			wantExpiry: 20 * time.Minute,
		},
		{
			name: "rig not listed gets neither",
			pool: &config.ReviewPoolConfig{Rigs: []string{"other"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0)}
			lander := reviewPoolTestLander()
			worker := reviewPoolTestWorker()

			// An unlisted rig must not read the environment at all, so its
			// rows use the env that fails the test on a read.
			env := reviewPoolTestEnv(t)
			wantOMConfig := ""
			if tc.wantOn {
				env, wantOMConfig = reviewPoolTestOMConfig(t, func() (string, error) { return "/sentinel/home", nil })
			}
			if err := d.wireReviewPool("gastown", &config.LandingWorkerConfig{ReviewPool: tc.pool}, lander, worker, "/sentinel/reviews", env); err != nil {
				t.Fatalf("wireReviewPool: %v", err)
			}

			if got := logs.String(); got != "" {
				t.Fatalf("wiring logged %q; want nothing", got)
			}
			if !tc.wantOn {
				if lander.Ahead != nil {
					t.Fatalf("rig not listed got Ahead %+v; want nil", lander.Ahead)
				}
				if lander.ReuseExpiry != 0 {
					t.Fatalf("rig not listed got ReuseExpiry %v; want 0", lander.ReuseExpiry)
				}
				if lander.OMConfigPath != "" {
					t.Fatalf("rig not listed got OMConfigPath %q; want empty", lander.OMConfigPath)
				}
			} else {
				wantAhead := land.TownAheadStore("/sentinel/town", "gastown")
				if !reflect.DeepEqual(lander.Ahead, wantAhead) {
					t.Fatalf("Ahead = %+v; want the gastown store under the Lander's town %+v", lander.Ahead, wantAhead)
				}
				if lander.ReuseExpiry != tc.wantExpiry {
					t.Fatalf("ReuseExpiry = %v; want %v", lander.ReuseExpiry, tc.wantExpiry)
				}
				if lander.OMConfigPath != wantOMConfig {
					t.Fatalf("OMConfigPath = %q; want %q", lander.OMConfigPath, wantOMConfig)
				}
				// Only the pool fields may move: with them cleared, the
				// Lander must be the one the daemon built. Lander holds a mutex,
				// so the reset happens in place rather than on a copy.
				lander.Ahead = nil
				lander.ReuseExpiry = 0
				lander.OMConfigPath = ""
				if want := reviewPoolTestLander(); !reflect.DeepEqual(lander, want) {
					t.Fatalf("wiring changed more than the pool fields:\ngot  %+v\nwant %+v", lander, want)
				}
			}
			// A listed rig's Worker gets the pool and the store behind it
			// (gt-sywmws); an unlisted rig leaves the Worker as it was built.
			if !tc.wantOn {
				if want := reviewPoolTestWorker(); !reflect.DeepEqual(worker, want) {
					t.Fatalf("rig not listed changed the Worker:\ngot  %+v\nwant %+v", worker, want)
				}
			} else if worker.Pool == nil || worker.Stored == nil {
				t.Fatalf("listed rig got Pool %v, Stored %v; want both set", worker.Pool, worker.Stored)
			}
		})
	}
}

// TestReviewPoolWiringOnSetsTheWorker pins the pool a listed rig's Worker
// gets: it reviews through the Lander, reads its hints from the same store the
// landing reads, runs the config's number of reviews at once, and logs where
// the landing logs (gt-sywmws).
func TestReviewPoolWiringOnSetsTheWorker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pool    *config.ReviewPoolConfig
		wantMax int
	}{
		{
			name:    "default max",
			pool:    &config.ReviewPoolConfig{Rigs: []string{"gastown"}},
			wantMax: 3,
		},
		{
			name:    "configured max",
			pool:    &config.ReviewPoolConfig{Rigs: []string{"gastown"}, MaxConcurrent: 5},
			wantMax: 5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0)}
			lander := reviewPoolTestLander()
			worker := reviewPoolTestWorker()
			var landingLog bytes.Buffer
			worker.Logf = log.New(&landingLog, "", 0).Printf
			env, _ := reviewPoolTestOMConfig(t, func() (string, error) { return "/sentinel/home", nil })

			if err := d.wireReviewPool("gastown", &config.LandingWorkerConfig{ReviewPool: tc.pool}, lander, worker, "/sentinel/reviews", env); err != nil {
				t.Fatalf("wireReviewPool: %v", err)
			}

			if want := land.TownAheadStore("/sentinel/town", "gastown"); !reflect.DeepEqual(worker.Stored, want) {
				t.Fatalf("Stored = %+v; want the Lander's ahead store %+v", worker.Stored, want)
			}
			if worker.Pool == nil {
				t.Fatal("the pool is on for the rig but the Worker got no pool")
			}
			if worker.Pool.Reviewer != lander {
				t.Fatalf("Pool.Reviewer = %+v; want the rig's Lander %+v", worker.Pool.Reviewer, lander)
			}
			if !reflect.DeepEqual(worker.Pool.Hints, worker.Stored) {
				t.Fatalf("Pool.Hints = %+v; want the store the Worker records into %+v", worker.Pool.Hints, worker.Stored)
			}
			if worker.Pool.Max != tc.wantMax {
				t.Fatalf("Pool.Max = %d; want the config's %d", worker.Pool.Max, tc.wantMax)
			}
			if worker.Pool.Logf == nil {
				t.Fatal("Pool.Logf is nil; want the landing log so a review's lines are seen")
			}
			worker.Pool.Logf("probe: %d", 1)
			if got, want := landingLog.String(), "probe: 1\n"; got != want {
				t.Fatalf("Pool.Logf wrote %q to the landing log; want %q", got, want)
			}
			// Only the two pool fields may move: with them and the injected
			// log cleared, the Worker must be the one the daemon built.
			worker.Stored, worker.Pool, worker.Logf = nil, nil, nil
			if want := reviewPoolTestWorker(); !reflect.DeepEqual(worker, want) {
				t.Fatalf("wiring changed more than the pool fields:\ngot  %+v\nwant %+v", worker, want)
			}
			if got := logs.String(); got != "" {
				t.Fatalf("wiring logged %q; want nothing", got)
			}
		})
	}
}

// TestReviewPoolWiringSetsOMConfigPath pins that a listed rig's Lander is
// given om's own config file under the XDG directory the environment names,
// which is where the review key reads its config from. A home directory om
// would not need is not required when XDG_CONFIG_HOME names the file
// (gt-aovgi3).
func TestReviewPoolWiringSetsOMConfigPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		home func() (string, error)
	}{
		{
			name: "home directory available",
			home: func() (string, error) { return "/fixture/home", nil },
		},
		{
			// om reads XDG_CONFIG_HOME first and never needs the home
			// directory when it is set (gt-3fr65j).
			name: "home directory unreadable, XDG names the file",
			home: func() (string, error) { return "", errors.New("no home for this process") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			configHome := t.TempDir()
			if err := os.MkdirAll(filepath.Join(configHome, "om"), 0o700); err != nil {
				t.Fatalf("make the om config directory: %v", err)
			}
			omConfig := filepath.Join(configHome, "om", "config.json")
			if err := os.WriteFile(omConfig, []byte("{}\n"), 0o600); err != nil {
				t.Fatalf("write om's config file: %v", err)
			}
			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0)}
			lander := reviewPoolTestLander()
			worker := reviewPoolTestWorker()
			env := reviewPoolEnv{
				lookup: func(key string) (string, bool) {
					if key == "XDG_CONFIG_HOME" {
						return configHome, true
					}
					return "", false
				},
				home: tc.home,
			}

			if err := d.wireReviewPool("gastown", &config.LandingWorkerConfig{
				ReviewPool: &config.ReviewPoolConfig{Rigs: []string{"gastown"}},
			}, lander, worker, "/sentinel/reviews", env); err != nil {
				t.Fatalf("wireReviewPool: %v", err)
			}

			if lander.OMConfigPath != omConfig {
				t.Fatalf("OMConfigPath = %q; want om's config file %q under the injected XDG directory", lander.OMConfigPath, omConfig)
			}
			if lander.Ahead == nil {
				t.Fatal("the pool is on but the Lander got no Ahead store")
			}
			if got := logs.String(); got != "" {
				t.Fatalf("wiring logged %q; want nothing", got)
			}
		})
	}
}

// TestReviewPoolWiringNoOMConfigStaysOff pins that a pool the config lists
// stays off when om's own config file cannot be used: a missing file and a
// location this process cannot name each undo the wiring (Ahead back to nil,
// so the rig lands with om inline), return the error, and leave exactly one
// log line, from the production wrapper that logs the returned error
// (gt-lfykjq, gt-lmflya).
func TestReviewPoolWiringNoOMConfigStaysOff(t *testing.T) {
	t.Parallel()
	noXDG := func(string) (string, bool) { return "", false }
	cases := []struct {
		name    string
		env     reviewPoolEnv
		wantErr string
	}{
		{
			name: "file missing",
			env: reviewPoolEnv{
				lookup: func(key string) (string, bool) {
					if key == "XDG_CONFIG_HOME" {
						return "/sentinel/xdg", true
					}
					return "", false
				},
				home: func() (string, error) { return "/sentinel/home", nil },
				stat: func(name string) (os.FileInfo, error) {
					return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
				},
			},
			wantErr: "/sentinel/xdg/om/config.json",
		},
		{
			name: "no home and no XDG",
			env: reviewPoolEnv{
				lookup: noXDG,
				home:   func() (string, error) { return "", nil },
			},
			wantErr: "XDG_CONFIG_HOME",
		},
		{
			name: "home directory read fails",
			env: reviewPoolEnv{
				lookup: noXDG,
				home:   func() (string, error) { return "", errors.New("no home for this process") },
			},
			wantErr: "no home for this process",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0)}
			lander := reviewPoolTestLander()
			worker := reviewPoolTestWorker()

			cfg := &config.LandingWorkerConfig{
				ReviewPool: &config.ReviewPoolConfig{Rigs: []string{"gastown"}},
			}
			err := d.wireReviewPool("gastown", cfg, lander, worker, "/sentinel/reviews", tc.env)

			if err == nil {
				t.Fatal("wireReviewPool returned nil without a usable om config file; want the error")
			}
			if !strings.Contains(err.Error(), "gastown") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name the rig and %q", err, tc.wantErr)
			}
			if lander.Ahead != nil {
				t.Fatalf("Lander.Ahead = %+v with no om config file; want nil, the pool off", lander.Ahead)
			}
			if want := reviewPoolTestLander(); !reflect.DeepEqual(lander, want) {
				t.Fatalf("no om config file changed the Lander:\ngot  %+v\nwant %+v", lander, want)
			}
			if want := reviewPoolTestWorker(); !reflect.DeepEqual(worker, want) {
				t.Fatalf("no om config file changed the Worker:\ngot  %+v\nwant %+v", worker, want)
			}
			// The production entry point logs the error wireReviewPool returns,
			// once and only there (gt-lmflya): the direct call above logged
			// nothing, so the one line below is the wrapper's.
			d.reviewPoolEnv = tc.env
			d.wireRigReviewPool("gastown", cfg, reviewPoolTestLander(), reviewPoolTestWorker(), "/sentinel/reviews")
			got := logs.String()
			if n := strings.Count(got, "\n"); n != 1 {
				t.Fatalf("no om config file logged %d lines, want one naming the rig and the cause; log:\n%s", n, got)
			}
			if !strings.Contains(got, "gastown") || !strings.Contains(got, tc.wantErr) {
				t.Fatalf("log line %q does not name the rig and %q", got, tc.wantErr)
			}
		})
	}
}

// TestReviewPoolWiringHoldStopsReviews pins that a listed rig's pool starts no
// ahead review while the daemon holds new landing passes off: the pool's Begin
// is the daemon's own landing-pass gate, so each hold a job takes answers false,
// and the released daemon answers true (gt-zajcg2).
func TestReviewPoolWiringHoldStopsReviews(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		hold string
	}{
		{name: "upgrade restart hold", hold: landingHoldUpgradeRestart},
		{name: "shutdown drain hold", hold: landingHoldShutdownDrain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0)}
			lander := reviewPoolTestLander()
			worker := reviewPoolTestWorker()
			env, _ := reviewPoolTestOMConfig(t, func() (string, error) { return "/sentinel/home", nil })

			if err := d.wireReviewPool("gastown", &config.LandingWorkerConfig{
				ReviewPool: &config.ReviewPoolConfig{Rigs: []string{"gastown"}},
			}, lander, worker, "/sentinel/reviews", env); err != nil {
				t.Fatalf("wireReviewPool: %v", err)
			}
			if worker.Pool == nil {
				t.Fatal("the pool is on for the rig but the Worker got no pool")
			}
			if worker.Pool.Begin == nil {
				t.Fatal("Pool.Begin is nil; want the daemon's landing-pass gate so a hold stops new ahead reviews")
			}
			if worker.Pool.End == nil {
				t.Fatal("Pool.End is nil; want the daemon's landing-pass end so the slot a true Begin takes is given back")
			}

			d.landingHolds.hold(tc.hold)
			if worker.Pool.Begin() {
				t.Fatalf("Begin returned true with the %s taken; want false so no ahead review starts", tc.hold)
			}

			d.landingHolds.release(tc.hold)
			if !worker.Pool.Begin() {
				t.Fatalf("Begin returned false after the %s was released; want true", tc.hold)
			}
			if got := d.landingPasses.Load(); got != 1 {
				t.Fatalf("in-flight count = %d after a true Begin, want 1: the review holds a landing pass", got)
			}
			worker.Pool.End()
			if got := d.landingPasses.Load(); got != 0 {
				t.Fatalf("in-flight count = %d after Pool.End, want 0: the review gave its landing pass back", got)
			}
			if got := logs.String(); got != "" {
				t.Fatalf("wiring logged %q; want nothing", got)
			}
		})
	}
}

// TestReviewPoolWiringNoOMConfigLeavesTheWorker pins that a listed rig whose
// om config file is missing or cannot be named leaves the Worker exactly as it
// was built: Pool and Stored stay nil, so a landing whose queue-time review
// could never be reused spends no om run on one (gt-jbirx5).
func TestReviewPoolWiringNoOMConfigLeavesTheWorker(t *testing.T) {
	t.Parallel()
	noXDG := func(string) (string, bool) { return "", false }
	cases := []struct {
		name string
		env  func(t *testing.T) reviewPoolEnv
	}{
		{
			// The production os.Stat runs here: a fresh XDG directory holds no
			// om/config.json, as on a host that has never run om.
			name: "config file missing",
			env: func(t *testing.T) reviewPoolEnv {
				return reviewPoolEnv{
					lookup: func(key string) (string, bool) {
						if key == "XDG_CONFIG_HOME" {
							return t.TempDir(), true
						}
						return "", false
					},
					home: func() (string, error) { return t.TempDir(), nil },
				}
			},
		},
		{
			name: "no XDG and no home",
			env: func(t *testing.T) reviewPoolEnv {
				return reviewPoolEnv{
					lookup: noXDG,
					home:   func() (string, error) { return "", nil },
				}
			},
		},
		{
			name: "home directory read fails",
			env: func(t *testing.T) reviewPoolEnv {
				return reviewPoolEnv{
					lookup: noXDG,
					home:   func() (string, error) { return "", errors.New("no home for this process") },
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			d := &Daemon{logger: log.New(&logs, "", 0)}
			lander := reviewPoolTestLander()
			worker := reviewPoolTestWorker()

			err := d.wireReviewPool("gastown", &config.LandingWorkerConfig{
				ReviewPool: &config.ReviewPoolConfig{Rigs: []string{"gastown"}},
			}, lander, worker, "/sentinel/reviews", tc.env(t))

			if err == nil {
				t.Fatal("wireReviewPool returned nil with no usable om config file; want the error so the pool stays off")
			}
			if !strings.Contains(err.Error(), "gastown") {
				t.Fatalf("error %q does not name the rig", err)
			}
			// The same error undoes the Lander's wiring, so the rig lands
			// with om inline.
			if want := reviewPoolTestLander(); !reflect.DeepEqual(lander, want) {
				t.Fatalf("no om config file changed the Lander:\ngot  %+v\nwant %+v", lander, want)
			}
			if worker.Pool != nil {
				t.Fatalf("no om config file gave the Worker Pool %+v; want nil so no review runs before the landing", worker.Pool)
			}
			if worker.Stored != nil {
				t.Fatalf("no om config file gave the Worker Stored %+v; want nil so no stored verdict is swept", worker.Stored)
			}
			if want := reviewPoolTestWorker(); !reflect.DeepEqual(worker, want) {
				t.Fatalf("no om config file changed the Worker:\ngot  %+v\nwant %+v", worker, want)
			}
		})
	}
}

// TestReviewPoolWiringRig pins that wireRigReviewPool reads om's config
// location through the Daemon's reviewPoolEnv field, so a test's own om config
// wires the pool; an om config the environment cannot use is logged once and
// leaves the pool off (gt-lmflya).
func TestReviewPoolWiringRig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		env      func(t *testing.T) (reviewPoolEnv, string)
		wantPool bool
	}{
		{
			name: "field's om config",
			env: func(t *testing.T) (reviewPoolEnv, string) {
				return reviewPoolTestOMConfig(t, func() (string, error) { return t.TempDir(), nil })
			},
			wantPool: true,
		},
		{
			name: "missing om config",
			env: func(t *testing.T) (reviewPoolEnv, string) {
				env, omConfig := reviewPoolTestOMConfig(t, func() (string, error) { return t.TempDir(), nil })
				env.stat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
				return env, omConfig
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			env, omConfig := tc.env(t)
			d := &Daemon{logger: log.New(&logs, "", 0), reviewPoolEnv: env}
			lander, worker := reviewPoolTestLander(), reviewPoolTestWorker()
			d.wireRigReviewPool("gastown", &config.LandingWorkerConfig{
				ReviewPool: &config.ReviewPoolConfig{Rigs: []string{"gastown"}},
			}, lander, worker, "/sentinel/reviews")

			if got := worker.Pool != nil; got != tc.wantPool {
				t.Fatalf("Worker.Pool set = %v; want %v", got, tc.wantPool)
			}
			if tc.wantPool && lander.OMConfigPath != omConfig {
				t.Fatalf("OMConfigPath = %q; want the field's om config file %q", lander.OMConfigPath, omConfig)
			}
			wantLines := 0
			if !tc.wantPool {
				wantLines = 1
				if lander.Ahead != nil {
					t.Fatalf("Lander.Ahead = %+v with no om config file; want nil, the pool off", lander.Ahead)
				}
			}
			if n := strings.Count(logs.String(), "\n"); n != wantLines {
				t.Fatalf("wiring logged %d lines, want %d; log:\n%s", n, wantLines, logs.String())
			}
		})
	}
}
