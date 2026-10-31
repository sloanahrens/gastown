package templates_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

const (
	liveForgeTestHost  = "forgejo:3000"
	liveForgeTestToken = "/host/forgejo/forgejo-landing.env"
)

// liveForgeSpec is the live town with the forge credential set: the pair the
// container's git credential helper is set from.
func liveForgeSpec() templates.ComposeSpec {
	s := liveSpec()
	s.ForgeHost = liveForgeTestHost
	s.ForgeTokenFile = liveForgeTestToken
	return s
}

// The live spec with the pair renders the entrypoint flags and a read-only
// mount of the host's token file at the fixed in-container path, and adds no
// scratch service. With neither, the file is the golden the live render always
// had. Removing the mount or the command line turns this red (gt-rpx80).
func TestRenderDaemonCompose_LiveForgeCredential(t *testing.T) {
	t.Parallel()

	t.Run("a host and token file", func(t *testing.T) {
		t.Parallel()
		out, f := render(t, liveForgeSpec())
		svc, ok := f.Services["daemon"]
		if !ok {
			t.Fatalf("services = %v, want a daemon service", f.Services)
		}

		want := []string{
			"--forge-host", liveForgeTestHost,
			"--forge-token-file", templates.LiveForgeTokenPath,
		}
		if !slices.Equal(svc.Command, want) {
			t.Errorf("daemon command = %v, want %v", svc.Command, want)
		}

		mount := composeVolume{
			Type:     "bind",
			Source:   liveForgeTestToken,
			Target:   templates.LiveForgeTokenPath,
			ReadOnly: true,
		}
		if !slices.Contains(svc.Volumes, mount) {
			t.Errorf("daemon volumes = %v, want the read-only token mount %+v", svc.Volumes, mount)
		}
		townMount := composeVolume{Type: "bind", Source: "/towns/live", Target: "/towns/live"}
		if !slices.Contains(svc.Volumes, townMount) {
			t.Errorf("daemon volumes = %v, want the town mount %+v beside it", svc.Volumes, townMount)
		}

		// The live render carries the live project's services alone: a scratch
		// name here would be a stack this file does not start.
		names := make([]string, 0, len(f.Services))
		for name := range f.Services {
			names = append(names, name)
		}
		slices.Sort(names)
		if want := []string{"daemon", engineName}; !slices.Equal(names, want) {
			t.Errorf("services = %v, want %v", names, want)
		}
		if strings.Contains(out, "scratch") {
			t.Errorf("the live render with the forge credential carries a scratch name:\n%s", out)
		}
	})

	t.Run("neither", func(t *testing.T) {
		t.Parallel()
		out, f := render(t, liveSpec())

		golden, err := os.ReadFile(filepath.Join("testdata", "daemon-compose.golden.yml"))
		if err != nil {
			t.Fatalf("reading golden file: %v", err)
		}
		if out != string(golden) {
			t.Errorf("a live spec with no forge credential differs from the golden\ngot:\n%s\nwant:\n%s", out, golden)
		}
		if cmd := f.Services["daemon"].Command; len(cmd) != 0 {
			t.Errorf("daemon command = %v, want none without the forge credential", cmd)
		}
		if vols := f.Services["daemon"].Volumes; len(vols) != 1 {
			t.Errorf("daemon volumes = %v, want the town mount alone", vols)
		}
	})
}

// A live spec carrying both credentials renders one command line with both
// pairs and one read-only mount for each, at their own fixed paths: two
// credentials in one daemon, not one replacing the other (gt-yaqct, gt-rpx80).
func TestRenderDaemonCompose_LiveCredentialsTogether(t *testing.T) {
	t.Parallel()
	spec := liveForgeSpec()
	spec.OMTokenFile = "/ops/om/token"
	out, f := render(t, spec)

	svc := f.Services["daemon"]
	want := []string{
		"--om-token-file", templates.LiveOMTokenPath,
		"--forge-host", liveForgeTestHost,
		"--forge-token-file", templates.LiveForgeTokenPath,
	}
	if !slices.Equal(svc.Command, want) {
		t.Errorf("daemon command = %v, want %v", svc.Command, want)
	}
	for _, mount := range []composeVolume{
		{Type: "bind", Source: spec.OMTokenFile, Target: templates.LiveOMTokenPath, ReadOnly: true},
		{Type: "bind", Source: liveForgeTestToken, Target: templates.LiveForgeTokenPath, ReadOnly: true},
	} {
		if !slices.Contains(svc.Volumes, mount) {
			t.Errorf("daemon volumes = %v, want %+v", svc.Volumes, mount)
		}
	}
	for _, flag := range []string{"--om-token-file", "--forge-host", "--forge-token-file"} {
		if got := strings.Count(out, flag); got != 1 {
			t.Errorf("the file names %s %d times, want once on the daemon's one command line:\n%s", flag, got, out)
		}
	}
}

// Each row is a live spec whose forge credential the renderer must refuse.
// Removing the check a row names turns that row red (gt-rpx80).
func TestComposeSpecValidateLiveForge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		host  string
		token string
		want  string
	}{
		{"a host without a token file", liveForgeTestHost, "", "carries both or neither"},
		{"a token file without a host", "", liveForgeTestToken, "carries both or neither"},
		{"a relative token file", liveForgeTestHost, "forgejo/forgejo-landing.env", "is not absolute"},
		{"a token file with a control character", liveForgeTestHost, "/host/forgejo/landing\n.env", "control character"},
		{"a host with a scheme", "http://forgejo:3000", liveForgeTestToken, "not a bare host:port"},
		{"a host with no port", "forgejo", liveForgeTestToken, "not a bare host:port"},
		{"a host beginning with a dash", "-forgejo:3000", liveForgeTestToken, "not a bare host:port"},
		{"a host with a space", "forge jo:3000", liveForgeTestToken, "not a bare host:port"},
		{"a host with port zero", "forgejo:0", liveForgeTestToken, "is not a port"},
		{"a host with a port above 65535", "forgejo:99999", liveForgeTestToken, "is not a port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := liveSpec()
			s.ForgeHost = tc.host
			s.ForgeTokenFile = tc.token

			_, err := templates.RenderDaemonCompose(s)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RenderDaemonCompose = %v, want a refusal saying %q", err, tc.want)
			}
		})
	}
}

// A scratch spec takes its forge credential from ScratchForge, so the pair on
// it would be dropped in silence; the render refuses instead.
func TestComposeSpecValidateScratchRefusesTheLiveForgeFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		host  string
		token string
	}{
		{"both", liveForgeTestHost, liveForgeTestToken},
		{"the host alone", liveForgeTestHost, ""},
		{"the token file alone", "", liveForgeTestToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := scratchSpec()
			s.ForgeHost = tc.host
			s.ForgeTokenFile = tc.token

			_, err := templates.RenderDaemonCompose(s)

			if err == nil || !strings.Contains(err.Error(), "scratch spec") {
				t.Fatalf("RenderDaemonCompose = %v, want a refusal saying the fields are set on a scratch spec", err)
			}
		})
	}
}
