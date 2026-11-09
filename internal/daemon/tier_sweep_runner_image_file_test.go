package daemon

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// tierSweepImageRef is a digest reference in repo: the shape the pod's file
// holds. The digest is 64 lowercase hex characters.
func tierSweepImageRef(repo string) string {
	return repo + "@sha256:" + strings.Repeat("a", 64)
}

// tierSweepImageFile writes content to a file under a fresh temp directory and
// returns its path.
func tierSweepImageFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "polecat-image")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the runner image file: %v", err)
	}
	return path
}

// runnerImageFileDaemon is a runner-image daemon whose image file is path, with
// image_registry set to registry and the daemon's log captured in the buffer
// it returns.
func runnerImageFileDaemon(t *testing.T, image, registry, path string) (*Daemon, *tierSweepImageRunner, *bytes.Buffer) {
	t.Helper()
	d, fake := runnerImageDaemon(t, atHour(time.Now(), 15), image)
	d.patrolConfig.Patrols.TierSweep.Runner.ImageRegistry = registry
	d.tierSweepSeams.runnerImageFile = path
	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)
	return d, fake, &logs
}

// TestTierSweepRunnerImageFileWins: a valid digest file is the runner image
// whatever the settings pin is, and image_registry does not rewrite it.
func TestTierSweepRunnerImageFileWins(t *testing.T) {
	t.Parallel()

	file := tierSweepImageRef("registry.example.com/gt/gastown-polecat")
	for _, pin := range []string{"", config.TierSweepRunnerInstalledImage, "gastown-polecat:23017b4dd"} {
		for _, registry := range []string{"", "registry.example.com/gt"} {
			t.Run("pin "+pin+" registry "+registry, func(t *testing.T) {
				t.Parallel()
				path := tierSweepImageFile(t, file+"\n")
				d, _, logs := runnerImageFileDaemon(t, pin, registry, path)

				got, err := d.tierSweepRunnerImage(d.patrolConfig.Patrols.TierSweep.Runner)
				if err != nil {
					t.Fatalf("tierSweepRunnerImage: %v", err)
				}
				if got != file {
					t.Errorf("runner image = %q, want the file's content %q", got, file)
				}
				if logs.Len() != 0 {
					t.Errorf("a valid file logged %q, want nothing", logs.String())
				}
			})
		}
	}
}

// TestTierSweepRunnerImageFileFallsBackLoudly: a file that cannot be read, is
// empty, or holds a tag resolves as the settings do, logs one line naming the
// path and the reason, and logs nothing more for the same reason.
func TestTierSweepRunnerImageFileFallsBackLoudly(t *testing.T) {
	t.Parallel()

	const today = "gastown-polecat:0123456789ab"
	for _, tc := range []struct {
		name   string
		path   func(t *testing.T) string
		reason string
	}{
		// A directory is a path that exists and cannot be read as a file.
		{name: "unreadable", path: func(t *testing.T) string { return t.TempDir() }, reason: "cannot be read"},
		{name: "empty", path: func(t *testing.T) string { return tierSweepImageFile(t, " \n\t\n") }, reason: "is empty"},
		{name: "a tag", path: func(t *testing.T) string {
			return tierSweepImageFile(t, "registry.example.com/gt/gastown-polecat:v1\n")
		}, reason: "does not hold a digest reference"},
		// A repository that starts with a dash would read as a docker flag.
		{name: "a dashed repository", path: func(t *testing.T) string {
			return tierSweepImageFile(t, "-registry.example.com/gt/gastown-polecat@sha256:"+strings.Repeat("a", 64)+"\n")
		}, reason: "does not hold a digest reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := tc.path(t)
			d, _, logs := runnerImageFileDaemon(t, "", "", path)
			cfg := d.patrolConfig.Patrols.TierSweep.Runner

			for range 2 {
				got, err := d.tierSweepRunnerImage(cfg)
				if err != nil {
					t.Fatalf("tierSweepRunnerImage: %v", err)
				}
				if got != today {
					t.Errorf("runner image = %q, want today's %q", got, today)
				}
			}

			lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n")
			if len(lines) != 1 {
				t.Fatalf("log = %q, want one line: the reason logs once, not per resolution", logs.String())
			}
			if !strings.Contains(lines[0], path) || !strings.Contains(lines[0], tc.reason) {
				t.Errorf("log line = %q, want it to name %q and %q", lines[0], path, tc.reason)
			}
		})
	}
}

// TestTierSweepRunnerImageFileAbsentChangesNothing: with no file, the image and
// the log are what they were before the file existed, for each settings case.
func TestTierSweepRunnerImageFileAbsentChangesNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		pin      string
		registry string
		want     string
	}{
		{name: "empty follows the installed build", pin: "", want: "gastown-polecat:0123456789ab"},
		{name: "installed follows the installed build", pin: config.TierSweepRunnerInstalledImage, want: "gastown-polecat:0123456789ab"},
		{name: "an explicit pin stands", pin: "gastown-polecat:23017b4dd", want: "gastown-polecat:23017b4dd"},
		{name: "the installed build inside the registry", pin: "", registry: "reg.example.com/gt", want: "reg.example.com/gt/gastown-polecat:0123456789ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			absent := filepath.Join(t.TempDir(), "absent")
			d, _, logs := runnerImageFileDaemon(t, tc.pin, tc.registry, absent)

			got, err := d.tierSweepRunnerImage(d.patrolConfig.Patrols.TierSweep.Runner)
			if err != nil {
				t.Fatalf("tierSweepRunnerImage: %v", err)
			}
			if got != tc.want {
				t.Errorf("runner image = %q, want %q", got, tc.want)
			}
			if logs.Len() != 0 {
				t.Errorf("an absent file logged %q, want nothing", logs.String())
			}
		})
	}
}

// TestTierSweepRunnerImageFileIsNotEnsuredAsInstalled: a valid file is a pin,
// so the ensure step neither checks the sweep engine for it nor loads it. The
// fallback case shows the same ensure step does check the installed build.
func TestTierSweepRunnerImageFileIsNotEnsuredAsInstalled(t *testing.T) {
	t.Parallel()

	file := tierSweepImageRef("registry.example.com/gt/gastown-polecat")

	t.Run("a valid file is not checked or loaded", func(t *testing.T) {
		t.Parallel()
		d, fake, _ := runnerImageFileDaemon(t, "", "", tierSweepImageFile(t, file))
		spec := sweeprunner.Spec{Rig: "gastown", Image: file, Context: "colima-gt-sbx"}
		if out, err := d.tierSweepEnsureRunnerImage(context.Background(), spec); err != nil || out != "" {
			t.Fatalf("tierSweepEnsureRunnerImage = %q, %v; want nothing and no error", out, err)
		}
		if calls := fake.callList(); len(calls) != 0 {
			t.Errorf("ensure ran %q, want no engine command for a pinned file", calls)
		}
	})

	t.Run("an empty file is still ensured as the installed build", func(t *testing.T) {
		t.Parallel()
		d, fake, _ := runnerImageFileDaemon(t, "", "", tierSweepImageFile(t, ""))
		fake.checkOut = "sha256:abc\n"
		spec := sweeprunner.Spec{Rig: "gastown", Image: "gastown-polecat:0123456789ab", Context: "colima-gt-sbx"}
		if _, err := d.tierSweepEnsureRunnerImage(context.Background(), spec); err != nil {
			t.Fatalf("tierSweepEnsureRunnerImage: %v", err)
		}
		if looks := fake.callsMatching("ls"); len(looks) == 0 {
			t.Errorf("ensure ran no presence check for the installed build; calls = %q", fake.callList())
		}
	})
}

// TestTierSweepRunnerImageFilePathMatchesTheRender: the path the daemon reads
// by default is the path the pod template mounts the polecat image at.
func TestTierSweepRunnerImageFilePathMatchesTheRender(t *testing.T) {
	t.Parallel()

	if tierSweepPolecatImageFile != k8sdaemon.PolecatImagePath {
		t.Errorf("tierSweepPolecatImageFile = %q, want the render's mount path %q", tierSweepPolecatImageFile, k8sdaemon.PolecatImagePath)
	}
}
