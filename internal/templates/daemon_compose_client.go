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
	"GT_TOWN_ROOT", "KUBECONFIG", "TZ", "XDG_CONFIG_HOME",
}

var composeZoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`)

var composeEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateClient refuses Env, KubeconfigFiles and OpenShellConfigDir values the
// render cannot carry, and any of them on a scratch spec.
func (s ComposeSpec) validateClient() error {
	if s.Scratch && (len(s.Env) > 0 || len(s.KubeconfigFiles) > 0 || s.OpenShellConfigDir != "" ||
		s.OMTokenMetaFile != "" || s.LandingWorkRoot != "" || s.TimeZone != "") {
		return fmt.Errorf("compose spec: Env, KubeconfigFiles, OpenShellConfigDir, OMTokenMetaFile, LandingWorkRoot or TimeZone is set on a scratch spec; only the live render carries the client configuration")
	}
	if s.TimeZone != "" && !composeZoneName.MatchString(s.TimeZone) {
		return fmt.Errorf("compose spec: TimeZone %q is not an IANA zone name such as America/Chicago", s.TimeZone)
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
	if s.LandingWorkRoot != "" && !filepath.IsAbs(s.LandingWorkRoot) {
		return fmt.Errorf("compose spec: LandingWorkRoot %q is not absolute; it is bound at the same path in the container, and a relative one would resolve against docker's working directory", s.LandingWorkRoot)
	}
	if s.LandingWorkRoot != "" {
		clean := filepath.Clean(s.LandingWorkRoot)
		if clean == "/" || pathContains(s.TownRoot, clean) || pathContains(clean, s.TownRoot) {
			return fmt.Errorf("compose spec: LandingWorkRoot %q is the filesystem root or overlaps the town root %q; it is bound read-write beside the town, not over it", s.LandingWorkRoot, s.TownRoot)
		}
		if s.OMTokenMetaFile != "" && (pathContains(clean, s.OMTokenMetaFile) || pathContains(s.OMTokenMetaFile, clean)) {
			return fmt.Errorf("compose spec: LandingWorkRoot %q and OMTokenMetaFile %q overlap, so two mounts would share a target", s.LandingWorkRoot, s.OMTokenMetaFile)
		}
	}
	for _, p := range append([]string{s.OpenShellConfigDir, s.OMTokenMetaFile, s.LandingWorkRoot}, s.KubeconfigFiles...) {
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
	if s.TimeZone != "" {
		line("TZ", s.TimeZone)
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
	var b strings.Builder
	if s.OpenShellConfigDir != "" {
		b.WriteString(liveTokenMount(s.OpenShellConfigDir, LiveOpenShellConfigPath))
	}
	if s.OMTokenMetaFile != "" {
		b.WriteString(liveTokenMount(s.OMTokenMetaFile, s.OMTokenMetaFile))
	}
	if s.LandingWorkRoot != "" {
		q := strconv.Quote(composeLiteral(s.LandingWorkRoot))
		fmt.Fprintf(&b, "      - type: bind\n        source: %s\n        target: %s\n", q, q)
	}
	return b.String()
}

// pathContains reports whether child is dir or lies under it, on cleaned paths.
func pathContains(dir, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(child))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
