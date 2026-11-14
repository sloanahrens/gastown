package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// imageDir is where the daemon image ships the wrapper and bd.real side by side.
// Its files exist only in the fake stat below, so no test writes an executable.
const imageDir = "/usr/local/bin"

// realBdBeside puts the harness in the daemon image's layout: the wrapper is bd
// under imageDir with bd.real beside it as an executable file. Without it bd is not
// in forwarder mode, which is the default the other tests keep.
func realBdBeside(h *harness) {
	self, real := filepath.Join(imageDir, "bd"), filepath.Join(imageDir, "bd.real")
	h.surface.executable = func() (string, error) { return self, nil }
	fallback := h.surface.stat
	h.surface.stat = func(path string) (fs.FileInfo, error) {
		switch path {
		case self, real:
			return fakeFile{mode: 0o755}, nil
		}
		return fallback(path)
	}
}

// forwarderEnv answers HOME and the two gateway variables the daemon image
// configures, so a bd call with no config file reaches the forwarder.
func forwarderEnv(h *harness, url, tokenFile string) {
	h.surface.getenv = func(name string) string {
		switch name {
		case "HOME":
			return h.home
		case "GT_TOWN_ROOT":
			return h.town
		case "BD_GATEWAY_URL":
			return url
		case "BD_GATEWAY_TOKEN_FILE":
			return tokenFile
		}
		return ""
	}
}

// tokenFile writes a gateway token to a temp file the harness's real readFile reads.
func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.token")
	if err := os.WriteFile(path, []byte("sekrit\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

// TestRunForwarderWithoutConfig: a bd call with no config file is the daemon
// image's forwarder when bd.real sits beside the wrapper: the gateway BD_GATEWAY_URL
// names carries it, and with no URL the local bd runs.
func TestRunForwarderWithoutConfig(t *testing.T) {
	t.Parallel()
	t.Run("the gateway BD_GATEWAY_URL names", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		realBdBeside(h)
		token := tokenFile(t)
		forwarderEnv(h, "http://host.docker.internal:7440", token)
		if exit := h.run("bd", "show", "gt-x"); exit != 0 {
			t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
		}
		if !h.fwdBuilt {
			t.Fatal("no forwarder target was built")
		}
		want := bdGatewayConfig{URL: "http://host.docker.internal:7440", TokenFile: token}
		if h.fwdCfg != want {
			t.Fatalf("forwarder config = %+v, want %+v", h.fwdCfg, want)
		}
		if h.targetSet || h.bdBuilt {
			t.Fatal("the docker or bdgateway target was built for a bd call with no config file")
		}
		if !slices.Equal(h.fake.call.Argv, []string{"bd", "show", "gt-x"}) {
			t.Fatalf("launched %q, want bd show gt-x", h.fake.call.Argv)
		}
		if h.builtTown != h.town {
			t.Fatalf("built the forwarder for town %q, want %q", h.builtTown, h.town)
		}
	})

	t.Run("no gateway runs the local bd", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		realBdBeside(h)
		forwarderEnv(h, "", "")
		// The local bd needs no town root, so the caller's directory is no town.
		h.surface.getwd = func() (string, error) { return t.TempDir(), nil }
		if exit := h.run("bd", "status"); exit != 0 {
			t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
		}
		if !h.fwdBuilt {
			t.Fatal("no forwarder target was built")
		}
		if h.fwdCfg.URL != "" || h.fwdCfg.TokenFile != "" {
			t.Fatalf("forwarder config = %+v, want no gateway", h.fwdCfg)
		}
		if h.builtTown != "" {
			t.Fatalf("built the forwarder for town %q; the local bd needs none", h.builtTown)
		}
	})

	t.Run("a config file still names the target", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		realBdBeside(h)
		forwarderEnv(h, "http://host.docker.internal:7440", tokenFile(t))
		h.writeConfig(t, `{"target": "docker"}`)
		if exit := h.run("bd", "show", "gt-x"); exit != 0 {
			t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
		}
		if h.fwdBuilt {
			t.Fatal("the forwarder ran a bd call the config file names a target for")
		}
		if !h.targetSet {
			t.Fatal("the docker target the config file names was not built")
		}
	})

	t.Run("another tool keeps its docker default", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		realBdBeside(h)
		forwarderEnv(h, "", "")
		if exit := h.run("gt", "status"); exit != 0 {
			t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
		}
		if h.fwdBuilt {
			t.Fatal("a gt call was routed through the bd forwarder")
		}
		if !h.targetSet {
			t.Fatal("the docker default was not built for a gt call with no config file")
		}
	})
}

// TestRunBdWithoutRealBdKeepsTheDockerDefault: with no bd.real beside the wrapper
// bd is not in forwarder mode, so a bd call with no config file takes the docker
// default it took before the wrapper was installed, whatever the gateway variables
// say.
func TestRunBdWithoutRealBdKeepsTheDockerDefault(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, url, token string
	}{
		{name: "no gateway configured", url: "", token: ""},
		{name: "a gateway configured", url: "http://host.docker.internal:7440", token: "/unused"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			forwarderEnv(h, row.url, row.token)
			if exit := h.run("bd", "show", "gt-x"); exit != 0 {
				t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
			}
			if h.fwdBuilt {
				t.Fatal("the forwarder was built with no bd.real beside the wrapper")
			}
			if !h.targetSet {
				t.Fatal("the docker default was not built for a bd call with no config file")
			}
		})
	}
}

// TestRunForwarderRefusesAURLWithoutAToken: a gateway URL the image configures
// without a token file is refused by naming both variables, never routed to the
// local bd.
func TestRunForwarderRefusesAURLWithoutAToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	realBdBeside(h)
	forwarderEnv(h, "http://host.docker.internal:7440", "")
	if exit := h.run("bd", "show", "gt-x"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	for _, want := range []string{"BD_GATEWAY_URL", "BD_GATEWAY_TOKEN_FILE"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Fatalf("stderr %q does not name %q", h.stderr, want)
		}
	}
	if h.fwdBuilt || h.targetSet || h.fake.launched {
		t.Fatal("a call with no token reached a target")
	}
}

// TestRunForwarderRefusesAnUnreadableToken: a gateway URL whose token file cannot be
// read is refused naming the file, never run through the local bd.
func TestRunForwarderRefusesAnUnreadableToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	realBdBeside(h)
	missing := filepath.Join(t.TempDir(), "missing.token")
	forwarderEnv(h, "http://host.docker.internal:7440", missing)
	if exit := h.run("bd", "show", "gt-x"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), missing) {
		t.Fatalf("stderr %q does not name the token file %s", h.stderr, missing)
	}
	if h.fwdBuilt || h.targetSet || h.fake.launched {
		t.Fatal("a call with an unreadable token reached a target")
	}
}

// TestRunRefusesAnUnwiredForwarder: a wrapper built without the forwarder
// constructor refuses a bd call it has no target for, rather than reading the
// missing wiring as the docker default.
func TestRunRefusesAnUnwiredForwarder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	realBdBeside(h)
	forwarderEnv(h, "", "")
	h.surface.newForwarder = nil
	if exit := h.run("bd", "show", "gt-x"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "no forwarder target is wired") {
		t.Fatalf("stderr %q does not say the forwarder target is unwired", h.stderr)
	}
	if h.targetSet || h.fake.launched {
		t.Fatal("an unwired forwarder launched a call")
	}
}

// TestRunForwarderKeepsTheFileArgRewrite: the forwarder's gateway workspace does
// not mirror the caller's filesystem, so a file a flag names travels as stdin
// exactly as through the bdgateway target a config file names.
func TestRunForwarderKeepsTheFileArgRewrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	realBdBeside(h)
	forwarderEnv(h, "http://host.docker.internal:7440", tokenFile(t))
	path := writeMacFile(t, h.town, "body.md", "the bead text\n")
	if exit := h.run("bd", "create", "--body-file", path); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	want := []string{"bd", "create", "--body-file", "-"}
	if !slices.Equal(h.fake.call.Argv, want) {
		t.Fatalf("target argv = %q, want %q", h.fake.call.Argv, want)
	}
	if h.fake.call.Stdin == nil {
		t.Fatal("the forwarder's gateway got no stdin; the file was not fed")
	}
}

// TestRunWithoutHOMERefuses: without HOME the wrapper cannot tell whether a config
// file exists, so every tool refuses and names HOME, including bd: no call becomes
// the forwarder because the setting it would read went missing.
func TestRunWithoutHOMERefuses(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"bd", "gt"} {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			realBdBeside(h)
			h.surface.getenv = func(name string) string {
				if name == "GT_TOWN_ROOT" {
					return h.town
				}
				return ""
			}
			if exit := h.run(tool, "show", "gt-x"); exit != 1 {
				t.Fatalf("run(%s) = %d, want 1", tool, exit)
			}
			if !strings.Contains(h.stderr.String(), "HOME") {
				t.Fatalf("stderr %q does not name HOME", h.stderr)
			}
			if h.fwdBuilt || h.targetSet || h.fake.launched {
				t.Fatal("a call without HOME reached a target")
			}
		})
	}
}

// TestRunForwarderGateIsBdOnly: the forwarder mode belongs to the bd name, so the
// wrapper called as gt-wrap still runs its own commands and the four tools keep
// their config-driven behaviour.
func TestRunForwarderGateIsBdOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	realBdBeside(h)
	forwarderEnv(h, "http://host.docker.internal:7440", tokenFile(t))
	if exit := h.run("gt-wrap"); exit != 1 {
		t.Fatalf("run(gt-wrap) = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "install") {
		t.Fatalf("stderr %q does not name the commands gt-wrap runs", h.stderr)
	}
	if h.fwdBuilt || h.fake.launched {
		t.Fatal("gt-wrap's own command was routed through the forwarder")
	}
}
