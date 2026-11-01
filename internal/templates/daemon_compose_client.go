package templates

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LiveOpenShellConfigHome is the XDG_CONFIG_HOME the live container's daemon
// runs with, and LiveOpenShellConfigPath is where the host's openshell config
// directory (gateway metadata and the mTLS client certificates) is mounted
// read-only beneath it, so the container's openshell finds the gateways the way
// the host's does (gt-yj0vh).
const (
	LiveOpenShellConfigHome = "/run/gt/xdg"
	LiveOpenShellConfigPath = LiveOpenShellConfigHome + "/openshell"
)

// composeOwnedEnv are the environment names the render sets itself; a spec's Env
// may not name one, so a daemon.env entry can never override the container's
// identity, its Docker engine or its client configuration.
var composeOwnedEnv = []string{
	"DOCKER_HOST", "GT_DAEMON_CONTAINER_NAME", "GT_DAEMON_SUPERVISOR", "GT_DAEMON_TAKEOVER",
	"GT_TOWN_ROOT", "KUBECONFIG", "XDG_CONFIG_HOME",
}

var composeEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateClient refuses Env, KubeconfigFiles and OpenShellConfigDir values the
// render cannot carry, and any of them on a scratch spec.
func (s ComposeSpec) validateClient() error {
	if s.Scratch && (len(s.Env) > 0 || len(s.KubeconfigFiles) > 0 || s.OpenShellConfigDir != "") {
		return fmt.Errorf("compose spec: Env, KubeconfigFiles or OpenShellConfigDir is set on a scratch spec; only the live render carries the client configuration")
	}
	for name, value := range s.Env {
		switch {
		case !composeEnvName.MatchString(name):
			return fmt.Errorf("compose spec: Env name %q is not an environment variable name", name)
		case isOwnedEnv(name):
			return fmt.Errorf("compose spec: Env name %q is set by the render itself", name)
		case strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }):
			return fmt.Errorf("compose spec: Env %s holds a control character, which a compose value cannot carry", name)
		}
	}
	for _, p := range append([]string{s.OpenShellConfigDir}, s.KubeconfigFiles...) {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == ':' }) {
			return fmt.Errorf("compose spec: %q is not an absolute path without a colon or control character; the render mounts it and KUBECONFIG joins paths with a colon", p)
		}
	}
	return nil
}

func isOwnedEnv(name string) bool {
	for _, o := range composeOwnedEnv {
		if o == name {
			return true
		}
	}
	return false
}

// liveClientEnv is the extra environment lines of the daemon service: KUBECONFIG
// and XDG_CONFIG_HOME when set, then Env sorted by name. Each is a line of six
// spaces, in the form the fixed lines have.
func liveClientEnv(s ComposeSpec) string {
	var b strings.Builder
	line := func(k, v string) {
		fmt.Fprintf(&b, "      %s: %s\n", k, strconv.Quote(composeLiteral(v)))
	}
	if len(s.KubeconfigFiles) > 0 {
		line("KUBECONFIG", strings.Join(s.KubeconfigFiles, ":"))
	}
	if s.OpenShellConfigDir != "" {
		line("XDG_CONFIG_HOME", LiveOpenShellConfigHome)
	}
	names := make([]string, 0, len(s.Env))
	for n := range s.Env {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		line(n, s.Env[n])
	}
	return b.String()
}

// liveClientMounts is the read-only mount of the openshell config directory.
// The directory holds client certificates; the render carries its path, never
// a file's contents.
func liveClientMounts(s ComposeSpec) string {
	if s.OpenShellConfigDir == "" {
		return ""
	}
	return liveTokenMount(s.OpenShellConfigDir, LiveOpenShellConfigPath)
}
