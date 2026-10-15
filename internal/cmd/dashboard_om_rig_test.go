package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// omTestRead is the rig-file read a test hands the reader.
func omTestRead(data string) func() ([]byte, bool, error) {
	return func() ([]byte, bool, error) { return []byte(data), true, nil }
}

func omTestPtr(f float64) *float64 { return &f }

// The panel reports the bar the landing worker holds a landing to: the rig's
// own .om.json, not the user's om config (gt-ndbwf). The user config is the
// fallback only for a rig with none, and the caption says which one spoke.
func TestOMPanelThresholdComesFromTheRigsOMJSON(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)
	userCfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(userCfg, []byte(`{"backend":["/x/claude-deepseek-flash","-p"],"threshold":0.6,"depth":"deep","timeout":300}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		rigOM      func() ([]byte, bool, error)
		wantThr    *float64
		wantDepth  string
		wantTO     int
		wantSource string
		wantModel  string
	}{
		{"rig bar over the user config", omTestRead(`{"threshold":0.7,"depth":"standard","timeout":900}`),
			omTestPtr(0.7), "standard", 900, "gastown .om.json", "flash"},
		{"rig with no .om.json falls back to the user config", func() ([]byte, bool, error) { return nil, false, nil },
			omTestPtr(0.6), "deep", 300, "~/.config/om/config.json, gastown has no .om.json", "flash"},
		{"rig file without a threshold falls back for it", omTestRead(`{"depth":"standard"}`),
			omTestPtr(0.6), "standard", 300, "~/.config/om/config.json, gastown .om.json sets no threshold", "flash"},
		// om ignores a backend key in the reviewed tree's .om.json, so the
		// model stays the user config's.
		{"rig backend key is ignored", omTestRead(`{"threshold":0.7,"backend":["/x/claude-deepseek-pro","-p"]}`),
			omTestPtr(0.7), "deep", 300, "gastown .om.json", "flash"},
		// A rig file the worker cannot parse fails every review: the panel
		// shows no bar, and says why, rather than the user's 0.6.
		{"unparseable rig file", omTestRead(`{"threshold":`),
			nil, "", 0, "gastown .om.json unreadable: unexpected end of JSON input", "flash"},
		{"rig file that cannot be read", func() ([]byte, bool, error) { return nil, false, errors.New("git: bad object") },
			nil, "", 0, "gastown .om.json unreadable: git: bad object", "flash"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &omReader{logPath: omTestLogFile(t, omTestMergeLine), landings: &dashLandings{}, cfgPath: userCfg, rigOM: tt.rigOM}
			om := r.read(now)
			if om == nil {
				t.Fatal("the read came back empty")
			}
			if (om.Threshold == nil) != (tt.wantThr == nil) || (tt.wantThr != nil && *om.Threshold != *tt.wantThr) {
				t.Errorf("threshold = %v, want %v", om.Threshold, tt.wantThr)
			}
			if om.Depth != tt.wantDepth || om.TimeoutS != tt.wantTO || om.ThresholdSource != tt.wantSource || om.Model != tt.wantModel {
				t.Errorf("depth %q timeout %d source %q model %q; want %q %d %q %q",
					om.Depth, om.TimeoutS, om.ThresholdSource, om.Model, tt.wantDepth, tt.wantTO, tt.wantSource, tt.wantModel)
			}
		})
	}
}

// The rig's file is read through git, so the reader keeps a read for
// omRigConfigTTL and re-reads after it: a landing that changes the bar shows
// within a minute, and a poll does not start git each time.
func TestOMPanelRigReadIsCachedForItsTTL(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)
	calls := 0
	r := &omReader{rigOM: func() ([]byte, bool, error) {
		calls++
		return []byte(`{"threshold":0.7}`), true, nil
	}}
	r.rigRead(now)
	r.rigRead(now.Add(omRigConfigTTL - time.Second))
	if calls != 1 {
		t.Fatalf("read %d times inside the TTL, want 1", calls)
	}
	r.rigRead(now.Add(omRigConfigTTL))
	if calls != 2 {
		t.Fatalf("read %d times after the TTL, want 2", calls)
	}
}

// readRigOMJSON is driven in the shape production takes: a bare .repo.git
// under the town's rig directory whose origin/main carries (or lacks) the
// .om.json. A target with no file is "none"; a repository git cannot read is
// an error and never reads as "none".
func TestOMPanelReadRigOMJSONReadsTheLandingTargetsFile(t *testing.T) {
	t.Parallel()
	gitRun := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mkTown := func(t *testing.T, omJSON string) string {
		t.Helper()
		town := t.TempDir()
		rigDir := filepath.Join(town, "gastown")
		work := filepath.Join(t.TempDir(), "work")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		gitRun(work, "init", "-q", "-b", "main")
		file, body := ".om.json", omJSON
		if omJSON == "" {
			file, body = "README", "x"
		}
		if err := os.WriteFile(filepath.Join(work, file), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		gitRun(work, "add", "-A")
		gitRun(work, "commit", "-q", "-m", "c")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		gitRun(rigDir, "clone", "-q", "--bare", work, ".repo.git")
		sha := gitRun(rigDir, "--git-dir=.repo.git", "rev-parse", "main")
		gitRun(rigDir, "--git-dir=.repo.git", "update-ref", "refs/remotes/origin/main", sha)
		return town
	}

	t.Run("target has the file", func(t *testing.T) {
		t.Parallel()
		data, found, err := readRigOMJSON(mkTown(t, `{"threshold":0.7}`), "gastown")
		if err != nil || !found || string(data) != `{"threshold":0.7}` {
			t.Fatalf("got %q found=%v err=%v", data, found, err)
		}
	})
	t.Run("target has no file", func(t *testing.T) {
		t.Parallel()
		data, found, err := readRigOMJSON(mkTown(t, ""), "gastown")
		if err != nil || found || data != nil {
			t.Fatalf("got %q found=%v err=%v; a target with no .om.json is none, not an error", data, found, err)
		}
	})
	t.Run("repository cannot be read", func(t *testing.T) {
		t.Parallel()
		if _, found, err := readRigOMJSON(t.TempDir(), "gastown"); err == nil || found {
			t.Fatalf("found=%v err=%v; a rig with no repository must be an error, not a rig without .om.json", found, err)
		}
	})
}
