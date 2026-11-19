package daemonimage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/templates"
)

// pinnedInputMtimeWant is the instant every pinned input must carry after
// staging, taken from the exported constant rather than written again here.
func pinnedInputMtimeWant() time.Time { return time.Unix(PinnedInputMtimeUnix, 0).UTC() }

// checkPinnedMtime fails the test unless path was staged with the fixed
// modification time.
func checkPinnedMtime(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("staging %s: %v", path, err)
	}
	if got, want := info.ModTime().UTC(), pinnedInputMtimeWant(); !got.Equal(want) {
		t.Errorf("%s was staged at %s, want the fixed %s", path, got, want)
	}
}

// TestPinnedInputsCarryTheFixedModificationTime drives each staging path that
// writes a pinned input and asserts the file carries the one fixed instant, so
// a COPY layer's digest depends on the input's bytes, not on when CI staged it.
func TestPinnedInputsCarryTheFixedModificationTime(t *testing.T) {
	t.Parallel()
	t.Run("bd and om", func(t *testing.T) {
		t.Parallel()
		b, _ := builderFor(t)
		paths, err := b.BuildBinaries(context.Background(), testRev)
		if err != nil {
			t.Fatalf("BuildBinaries: %v", err)
		}
		if len(paths) != len(wantBinaryNames) {
			t.Fatalf("BuildBinaries returned %d paths, want %d", len(paths), len(wantBinaryNames))
		}
		for _, name := range []string{"bd", "om"} {
			path := filepath.Join(b.Out, name)
			if !slices.Contains(paths, path) {
				t.Fatalf("BuildBinaries did not return %s", path)
			}
			checkPinnedMtime(t, path)
		}
	})
	t.Run("hm, the reporters and cloudcheck", func(t *testing.T) {
		t.Parallel()
		b, _, _ := toolsBuilder(t)
		paths, err := b.BuildTools(context.Background())
		if err != nil {
			t.Fatalf("BuildTools: %v", err)
		}
		want := []string{"hm", "devops-reporter", "release-reporter", "cloudcheck"}
		if len(paths) != len(want) {
			t.Fatalf("BuildTools returned %d paths, want %d", len(paths), len(want))
		}
		for i, name := range want {
			if got := filepath.Base(paths[i]); got != name {
				t.Fatalf("BuildTools path %d is %s, want %s", i, got, name)
			}
			checkPinnedMtime(t, paths[i])
		}
	})
	t.Run("claude", func(t *testing.T) {
		t.Parallel()
		f := &fakeFetcher{body: bytes.NewReader(claudeBytes)}
		b := claudeBuilder(t, f)
		path, err := b.fetchClaudeCLI(context.Background(), "https://example.test/claude", "2.1.314", claudeSum(claudeBytes), claudeCLIMaxBytes)
		if err != nil {
			t.Fatalf("fetchClaudeCLI: %v", err)
		}
		checkPinnedMtime(t, path)
	})
	t.Run("the client tools", func(t *testing.T) {
		t.Parallel()
		for _, name := range clientToolNamesForMtime {
			name := name
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				body := []byte(name + "-bytes")
				b := Builder{Out: t.TempDir(), Fetch: fetchOf(body)}
				pin := templates.ClientToolPin{Name: name, URL: "https://example.test/" + name, SHA256: sumHex(body)}
				path, err := b.fetchClientTool(context.Background(), pin, clientToolMaxBytes)
				if err != nil {
					t.Fatalf("fetchClientTool(%s): %v", name, err)
				}
				checkPinnedMtime(t, path)
			})
		}
	})
	t.Run("ssh_known_hosts", func(t *testing.T) {
		t.Parallel()
		b, _ := imageBuilder(t)
		if _, err := b.BuildImage(context.Background(), testRev, imageBinaries(b)); err != nil {
			t.Fatalf("BuildImage: %v", err)
		}
		checkPinnedMtime(t, filepath.Join(b.Out, KnownHostsName))
	})
}

// TestGastownCommitBinariesKeepTheirBuildTime: a binary built from the gastown
// commit, and a plugin built from the same commit, is not given the pinned
// input time: only an input fetched or built from a pin takes it.
func TestGastownCommitBinariesKeepTheirBuildTime(t *testing.T) {
	t.Parallel()
	b, _ := builderFor(t)
	paths, err := b.BuildBinaries(context.Background(), testRev)
	if err != nil {
		t.Fatalf("BuildBinaries: %v", err)
	}
	for _, name := range []string{"gt", "gt-wrap", "trackerd", "trackerbackup"} {
		path := filepath.Join(b.Out, name)
		if !slices.Contains(paths, path) {
			t.Fatalf("BuildBinaries did not return %s", path)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("staging %s: %v", path, err)
		}
		if info.ModTime().UTC().Equal(pinnedInputMtimeWant()) {
			t.Errorf("%s was given the pinned input time; it is built from the gastown commit and keeps its build time", path)
		}
	}
	plugins, err := b.BuildPlugins(context.Background(), testRev)
	if err != nil {
		t.Fatalf("BuildPlugins: %v", err)
	}
	if len(plugins) != len(imagePlugins) {
		t.Fatalf("BuildPlugins returned %d paths, want %d", len(plugins), len(imagePlugins))
	}
	for _, path := range plugins {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("staging %s: %v", path, err)
		}
		if info.ModTime().UTC().Equal(pinnedInputMtimeWant()) {
			t.Errorf("%s was given the pinned input time; a plugin keeps its build time", path)
		}
	}
}

// TestPinnedInputMtimeFailureStagesNothing: when the fixed time cannot be set,
// the staging call returns the error and leaves no context behind. A file left
// at the build time is the defect the fixed time exists to prevent, so this
// must never be a silent skip.
func TestPinnedInputMtimeFailureStagesNothing(t *testing.T) {
	t.Parallel()
	chtimesErr := errors.New("chtimes refused")
	fail := func(string, time.Time, time.Time) error { return chtimesErr }
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) (out string, err error)
	}{
		{
			name: "a downloaded file",
			run: func(t *testing.T) (string, error) {
				b := claudeBuilder(t, &fakeFetcher{body: bytes.NewReader(claudeBytes)})
				b.chtimes = fail
				_, err := b.fetchClaudeCLI(context.Background(), "https://example.test/claude", "2.1.314", claudeSum(claudeBytes), claudeCLIMaxBytes)
				return b.Out, err
			},
		},
		{
			name: "an extracted archive member",
			run: func(t *testing.T) (string, error) {
				archive := tgzOf(t, map[string]string{"openshell": "binary"}, []string{"openshell"})
				b := Builder{Out: t.TempDir(), Fetch: fetchOf(archive), chtimes: fail}
				pin := templates.ClientToolPin{Name: "openshell", URL: "https://example.test/openshell", SHA256: sumHex(archive), Member: "openshell"}
				_, err := b.fetchClientTool(context.Background(), pin, clientToolMaxBytes)
				return b.Out, err
			},
		},
		{
			name: "the known hosts file",
			run: func(t *testing.T) (string, error) {
				b := Builder{Out: t.TempDir(), Dockerfile: repoFile(t, DockerfilePath), chtimes: fail}
				return b.Out, b.stageKnownHosts()
			},
		},
		{
			name: "the binaries built from pins",
			run: func(t *testing.T) (string, error) {
				b, _ := builderFor(t)
				b.chtimes = fail
				_, err := b.BuildBinaries(context.Background(), testRev)
				return b.Out, err
			},
		},
		{
			name: "the tools built from pins",
			run: func(t *testing.T) (string, error) {
				b, _, _ := toolsBuilder(t)
				b.chtimes = fail
				_, err := b.BuildTools(context.Background())
				return b.Out, err
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := tc.run(t)
			if !errors.Is(err, chtimesErr) {
				t.Fatalf("error = %v, want the injected chtimes failure", err)
			}
			if got := entriesIn(out); len(got) != 0 {
				t.Errorf("a refused modification time left %q in the build context, want nothing", got)
			}
		})
	}
}

// TestPinnedInputMtimeRollbackFailureIsReported: when the fixed time cannot be
// set and the file cannot be taken back out either, the error names both, so a
// caller never reads a clean refusal over a context this call left changed.
func TestPinnedInputMtimeRollbackFailureIsReported(t *testing.T) {
	t.Parallel()
	chtimesErr := errors.New("chtimes refused")
	removeErr := errors.New("remove refused")
	failTime := func(string, time.Time, time.Time) error { return chtimesErr }
	failRemove := func(string) error { return removeErr }
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) error
	}{
		{
			name: "an extracted archive member",
			run: func(t *testing.T) error {
				archive := tgzOf(t, map[string]string{"openshell": "binary"}, []string{"openshell"})
				b := Builder{Out: t.TempDir(), Fetch: fetchOf(archive), chtimes: failTime, remove: failRemove}
				pin := templates.ClientToolPin{Name: "openshell", URL: "https://example.test/openshell", SHA256: sumHex(archive), Member: "openshell"}
				_, err := b.fetchClientTool(context.Background(), pin, clientToolMaxBytes)
				return err
			},
		},
		{
			name: "the known hosts file",
			run: func(t *testing.T) error {
				b := Builder{Out: t.TempDir(), Dockerfile: repoFile(t, DockerfilePath), chtimes: failTime, remove: failRemove}
				return b.stageKnownHosts()
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.run(t)
			if !errors.Is(err, chtimesErr) || !errors.Is(err, removeErr) {
				t.Fatalf("error = %v, want both the chtimes and the rollback failures", err)
			}
		})
	}
}

// clientToolNamesForMtime keeps the three pinned tool names next to the test
// that walks them, so a fourth pin added to templates is noticed here.
var clientToolNamesForMtime = []string{"openshell", "docker", "kubectl"}

// TestEveryPinnedClientToolIsCovered: the table above walks every name the
// templates pin, so a new pin added without a staging-time assertion is red.
func TestEveryPinnedClientToolIsCovered(t *testing.T) {
	t.Parallel()
	if len(templates.DaemonClientTools) != len(clientToolNamesForMtime) {
		t.Fatalf("%d client tool pins, the fixed-time test walks %d", len(templates.DaemonClientTools), len(clientToolNamesForMtime))
	}
	for _, p := range templates.DaemonClientTools {
		if !slices.Contains(clientToolNamesForMtime, p.Name) {
			t.Errorf("client tool %s is not walked by TestPinnedInputsCarryTheFixedModificationTime", p.Name)
		}
	}
}

// TestPinnedInputMtimeIsTheDocumentedInstant pins the exported constant to the
// date it documents, so a change to it is deliberate.
func TestPinnedInputMtimeIsTheDocumentedInstant(t *testing.T) {
	t.Parallel()
	if got, want := pinnedInputMtimeWant(), time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("PinnedInputMtimeUnix names %s, want the documented %s", got, want)
	}
}
