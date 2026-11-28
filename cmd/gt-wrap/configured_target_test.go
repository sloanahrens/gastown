package main

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredTarget(t *testing.T) {
	t.Parallel()
	t.Run("opens the cluster target for the named tool", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.writeConfig(t, kubernetesConfig)
		got, err := configuredTarget(h.surface, "om")
		if err != nil || got != h.fake || !h.kubeBuilt || h.kubeTool != "om" {
			t.Fatalf("configuredTarget = %v, %v; kubernetes built = %v for tool %q; want the target built for om", got, err, h.kubeBuilt, h.kubeTool)
		}
		if want := filepath.Join(h.home, "gt"); h.builtTown != want {
			t.Fatalf("built the target for town %q, want %q", h.builtTown, want)
		}
	})
	t.Run("returns an unreadable config's error", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.surface.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
		got, err := configuredTarget(h.surface, "gt")
		if err == nil || !strings.Contains(err.Error(), "read the target config file") || got != nil || h.targetSet {
			t.Fatalf("configuredTarget = %v, %v, target built = %v; want the config read failure and no target", got, err, h.targetSet)
		}
	})
	t.Run("returns a missing town's error", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		markerless := t.TempDir()
		h.surface.getenv = func(name string) string {
			switch name {
			case "HOME":
				return h.home
			case "GT_TOWN_ROOT":
				return markerless
			}
			return ""
		}
		got, err := configuredTarget(h.surface, "gt")
		if err == nil || !strings.Contains(err.Error(), "holds no "+townMarker) || got != nil || h.targetSet {
			t.Fatalf("configuredTarget = %v, %v, target built = %v; want the missing town marker failure and no target", got, err, h.targetSet)
		}
	})
	t.Run("returns the target's refusal for a tool it cannot run", func(t *testing.T) {
		t.Parallel()
		for _, config := range []string{
			`{"target": "bdgateway", "bdgateway": {"url": "http://gw", "token_file": "/token"}}`,
			`{"target": "bddocker", "bddocker": {"container": "gt-bd"}}`,
		} {
			h := newHarness(t)
			h.writeConfig(t, config)
			got, err := configuredTarget(h.surface, "gt")
			if err == nil || !strings.Contains(err.Error(), "runs bd") || got != nil || h.bdBuilt || h.bdDockerBuilt {
				t.Fatalf("configuredTarget(%s) = %v, %v; want the tool refusal and no target", config, got, err)
			}
		}
	})
}
