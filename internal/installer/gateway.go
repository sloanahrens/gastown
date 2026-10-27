package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GatewayMode is what a gateway step does: install the agent, or print the plan
// and change nothing.
type GatewayMode string

const (
	// GatewayInstall writes the unit and bootstraps it when launchd has not
	// loaded it.
	GatewayInstall GatewayMode = "install"
	// GatewayDryRun prints what an install would do.
	GatewayDryRun GatewayMode = "dry-run"
)

const (
	gatewayLabel        = "com.gastown.openshell-gateway"
	gatewayTemplateName = gatewayLabel + ".plist.tmpl"
)

// Gateway is the OpenShell gateway's launchd agent step: the script's
// igt_gateway_agent and the site that calls it (gt-tpg0p.9). The gateway is
// independent of the gt binary, so every failure here is one WARNING and the
// install carries on. The agent is bootstrapped only when launchd has not
// loaded it and never kickstarted: a kickstart drops every live sandbox session
// the gateway is serving (docs/openshell-gateway.md).
type Gateway struct {
	// C carries the exit contract's writers and its command runner.
	C *Contract
	// Files is the filesystem the step reads and writes; the zero value is the
	// real one.
	Files Files
	// RigDir is the checkout being installed; its scripts/launchd holds the
	// template.
	RigDir string
	// Home renders __HOME__ and holds the operator's gateway.toml, the log
	// directory and, by default, the LaunchAgents directory.
	Home string
	// LaunchdDir is INSTALL_GT_LAUNCHD_DIR, else Home/Library/LaunchAgents.
	LaunchdDir string
	// SelfDir is the running program's own directory, the template's fallback
	// when the checkout carries none. It must be absolute: a relative one
	// resolves against the working directory, which is not where the program
	// lives (gt-tpg0p.9).
	SelfDir string
}

// GatewayStep runs the OpenShell gateway step in mode; see Gateway.
func (r *Run) GatewayStep(mode GatewayMode) bool {
	return Gateway{
		C:          r.C,
		Files:      r.Files,
		RigDir:     r.Paths.RigDir,
		Home:       r.Paths.Home,
		LaunchdDir: r.Paths.LaunchdDir,
		SelfDir:    r.Paths.SelfDir,
	}.Step(mode)
}

// Step performs the gateway step. It reports false when the step failed: a
// template that is missing or cannot be rendered, a unit that cannot be written,
// or a bootstrap that did not run. A gateway that was never configured is not a
// failure and reports true.
func (g Gateway) Step(mode GatewayMode) bool {
	toml := filepath.Join(g.Home, ".config", "openshell", "gateway.toml")
	if !g.Files.isFile(toml) {
		g.C.Log("gateway: skipped, no " + toml)
		return true
	}
	tmpl := g.template()
	if !g.Files.isFile(tmpl) {
		g.C.Warn("gateway: no template at " + tmpl)
		return false
	}
	rendered, err := g.render(tmpl)
	if err != nil {
		g.C.Warn("gateway: could not render " + tmpl)
		return false
	}
	plist := filepath.Join(g.launchdDir(), gatewayLabel+".plist")
	changed := !g.current(plist, rendered)
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	loaded := g.loaded(domain)
	if mode == GatewayDryRun {
		if changed {
			g.C.Log("gateway: would write " + plist)
		}
		if !loaded {
			g.C.Log("gateway: would run: launchctl bootstrap " + domain + " " + plist)
		}
		return true
	}
	if changed && !g.writeUnit(plist, rendered) {
		return false
	}
	if !loaded {
		if _, err := g.C.exec("launchctl", "bootstrap", domain, plist); err != nil {
			g.C.Warn("gateway: launchctl bootstrap " + domain + " " + plist + " failed")
			return false
		}
		g.C.Log("gateway: bootstrapped " + gatewayLabel)
		return true
	}
	if changed {
		g.C.Log("gateway: " + gatewayLabel +
			" is loaded and keeps its old unit until: launchctl kickstart -k " + domain + "/" + gatewayLabel)
	}
	return true
}

// template is the script's lookup: the checkout being installed first, then the
// program's own directory. An empty or relative SelfDir has no fallback —
// filepath.Join would resolve it against the working directory, and the step
// must never render and bootstrap a template from wherever the install was
// started (gt-tpg0p.9) — so the checkout's path stands and the step warns that
// it has no template.
func (g Gateway) template() string {
	fromRig := filepath.Join(g.RigDir, "scripts", "launchd", gatewayTemplateName)
	if g.Files.isFile(fromRig) || !filepath.IsAbs(g.SelfDir) {
		return fromRig
	}
	return filepath.Join(g.SelfDir, "launchd", gatewayTemplateName)
}

// launchdDir is INSTALL_GT_LAUNCHD_DIR when set, else the installing user's
// LaunchAgents directory, the script's one default.
func (g Gateway) launchdDir() string {
	if g.LaunchdDir != "" {
		return g.LaunchdDir
	}
	return filepath.Join(g.Home, "Library", "LaunchAgents")
}

// render replaces __HOME__ with the XML-escaped home, as the script's python
// does, and leaves exactly one trailing newline: the shell's $(...) strips the
// template's own and printf puts one back.
func (g Gateway) render(path string) (string, error) {
	data, err := g.Files.readFile(path)
	if err != nil {
		return "", err
	}
	escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(g.Home)
	text := strings.ReplaceAll(string(data), "__HOME__", escaped)
	return strings.TrimRight(text, "\n") + "\n", nil
}

// current reports whether plist already holds rendered, which is the script's
// `[ "$(cat "$plist")" = "$rendered" ]`: the trailing newlines do not count.
func (g Gateway) current(plist, rendered string) bool {
	data, err := g.Files.readFile(plist)
	if err != nil {
		return false
	}
	return strings.TrimRight(string(data), "\n") == strings.TrimRight(rendered, "\n")
}

// loaded reports whether launchd has the agent. Its output is not read, as the
// script discards it.
func (g Gateway) loaded(domain string) bool {
	_, err := g.C.exec("launchctl", "print", domain+"/"+gatewayLabel)
	return err == nil
}

// writeUnit makes the unit's two directories and replaces plist with rendered,
// a temporary file first so a reader never sees half a unit. It reports its own
// WARNING and returns false when it could not.
func (g Gateway) writeUnit(plist, rendered string) bool {
	dir := filepath.Dir(plist)
	logDir := filepath.Join(g.Home, "gt", ".runtime", "openshell")
	if g.Files.mkdirAll(dir) != nil || g.Files.mkdirAll(logDir) != nil {
		g.C.Warn("gateway: cannot create " + dir)
		return false
	}
	tmp := fmt.Sprintf("%s.tmp.%d", plist, os.Getpid())
	if g.Files.writeFile(tmp, []byte(rendered), 0o644) != nil {
		_ = g.Files.remove(tmp)
		g.C.Warn("gateway: cannot write " + plist)
		return false
	}
	if g.Files.rename(tmp, plist) != nil {
		_ = g.Files.remove(tmp)
		g.C.Warn("gateway: cannot write " + plist)
		return false
	}
	g.C.Log("gateway: wrote " + plist)
	return true
}
