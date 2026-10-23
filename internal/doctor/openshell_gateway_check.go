package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// openShellLaunchdLabel is the launchd agent that keeps the OpenShell gateway
// running; scripts/install-gt.sh installs it beside the daemon unit.
const openShellLaunchdLabel = "com.gastown.openshell-gateway"

// openShellCheckTimeout bounds each command the check runs.
const openShellCheckTimeout = 30 * time.Second

// openShellRunner runs argv (argv[0] is the program) and returns its stdout. A
// non-nil error means the command failed or did not start; tests inject one so
// no openshell, docker or launchctl process runs.
type openShellRunner func(argv []string) ([]byte, error)

// OpenShellGatewayCheck reports the gateway, image, provider and launchd gaps
// that openshell seats would hit, and passes without running anything when the
// town has no openshell seat.
type OpenShellGatewayCheck struct {
	BaseCheck

	// run runs a command; nil is the real process runner.
	run openShellRunner
	// settings reads the town settings; nil is config.LoadOrCreateTownSettings,
	// which reads and never writes.
	settings func(townRoot string) (*config.TownSettings, error)
	// goos is the host OS; "" is runtime.GOOS. The launchd agent is macOS only.
	goos string
	// uid is the user id of the launchd gui domain; nil is os.Getuid.
	uid func() int
}

// NewOpenShellGatewayCheck creates a new openshell-gateway check.
func NewOpenShellGatewayCheck() *OpenShellGatewayCheck {
	return &OpenShellGatewayCheck{
		BaseCheck: BaseCheck{
			CheckName:        "openshell-gateway",
			CheckDescription: "Verify the OpenShell gateway, sandbox images, providers and launchd agent serve the openshell seats",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// openShellProfile is one sandbox profile an openshell seat runs in, with its
// defaults filled in.
type openShellProfile struct {
	name      string
	gateway   string
	image     string
	providers []string
}

// Run warns once per missing piece, and skips a gateway's provider list when
// that gateway does not answer.
func (c *OpenShellGatewayCheck) Run(ctx *CheckContext) *CheckResult {
	read := c.settings
	if read == nil {
		read = config.LoadOrCreateTownSettings
	}
	ts, err := read(config.TownSettingsPath(ctx.TownRoot))
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "unknown: could not read the town settings",
			Details: []string{err.Error()},
		}
	}

	profiles, err := openShellProfiles(ts)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "unknown: could not read polecat_pool.seats",
			Details: []string{err.Error()},
		}
	}
	if len(profiles) == 0 {
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "no openshell seats"}
	}

	run := c.run
	if run == nil {
		run = runOpenShellCommand
	}

	var problems []string
	problems = append(problems, checkGateways(run, profiles)...)
	problems = append(problems, checkImages(run, profiles)...)
	goos := c.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos == "darwin" {
		problems = append(problems, c.checkLaunchd(run)...)
	}

	if len(problems) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d openshell gateway problem(s) for %d profile(s)", len(problems), len(profiles)),
			Details: problems,
			FixHint: "Restart the gateway with: launchctl kickstart -k gui/$(id -u)/" + openShellLaunchdLabel + ", then rerun gt doctor",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("openshell gateway serves %d profile(s): connected, providers and images present, launchd agent loaded", len(profiles)),
	}
}

// openShellProfiles returns the sandbox profiles the openshell seats use, one
// per name, sorted by name.
func openShellProfiles(ts *config.TownSettings) ([]openShellProfile, error) {
	seats, err := config.Seats(ts, nil)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []openShellProfile
	for _, seat := range seats {
		if seat.Backend != config.BackendOpenShell || seen[seat.Sandbox] {
			continue
		}
		seen[seat.Sandbox] = true
		p := ts.Sandboxes[seat.Sandbox].WithDefaults()
		out = append(out, openShellProfile{
			name:      seat.Sandbox,
			gateway:   p.Gateway,
			image:     p.Image,
			providers: p.Providers,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// checkGateways reports each gateway the profiles name that does not answer as
// connected, and each provider a profile names that the gateway does not list.
func checkGateways(run openShellRunner, profiles []openShellProfile) []string {
	byGateway := map[string][]openShellProfile{}
	var gateways []string
	for _, p := range profiles {
		if _, ok := byGateway[p.gateway]; !ok {
			gateways = append(gateways, p.gateway)
		}
		byGateway[p.gateway] = append(byGateway[p.gateway], p)
	}
	sort.Strings(gateways)

	var problems []string
	for _, gw := range gateways {
		if msg := checkGatewayStatus(run, gw); msg != "" {
			problems = append(problems, msg)
			continue
		}
		listed, msg := listGatewayProviders(run, gw)
		if msg != "" {
			problems = append(problems, msg)
			continue
		}
		for _, p := range byGateway[gw] {
			for _, prov := range p.providers {
				if !listed[prov] {
					problems = append(problems, fmt.Sprintf("openshell provider list: provider %q (profile %s) is not registered with gateway %s", prov, p.name, gw))
				}
			}
		}
	}
	return problems
}

// checkGatewayStatus returns "" when `openshell -g <gw> status` reports the
// gateway connected, else the warning line.
func checkGatewayStatus(run openShellRunner, gw string) string {
	out, err := run([]string{"openshell", "-g", gw, "status", "--output", "json"})
	if err != nil {
		return fmt.Sprintf("openshell status: gateway %s did not answer: %v", gw, err)
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return fmt.Sprintf("openshell status: gateway %s: decode: %v", gw, err)
	}
	if st.Status != "connected" {
		return fmt.Sprintf("openshell status: gateway %s reports %q, not connected", gw, st.Status)
	}
	return ""
}

// listGatewayProviders returns the names of the providers gw lists, or the
// warning line when the list cannot be read.
func listGatewayProviders(run openShellRunner, gw string) (map[string]bool, string) {
	out, err := run([]string{"openshell", "-g", gw, "provider", "list", "--output", "json"})
	if err != nil {
		return nil, fmt.Sprintf("openshell provider list: gateway %s: %v", gw, err)
	}
	var list struct {
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Sprintf("openshell provider list: gateway %s: decode: %v", gw, err)
	}
	names := make(map[string]bool, len(list.Providers))
	for _, p := range list.Providers {
		names[p.Name] = true
	}
	return names, ""
}

// checkImages reports each distinct profile image that docker cannot inspect.
// When docker cannot start at all, it reports one line for the whole check
// instead, so the skip is visible and the images are not blamed one by one.
func checkImages(run openShellRunner, profiles []openShellProfile) []string {
	seen := map[string]bool{}
	var problems []string
	for _, p := range profiles {
		if seen[p.image] {
			continue
		}
		seen[p.image] = true
		_, err := run([]string{"docker", "image", "inspect", p.image})
		if errors.Is(err, exec.ErrNotFound) {
			return []string{fmt.Sprintf("docker image check skipped: docker CLI not on PATH, so the images of %d profile(s) are unverified", len(profiles))}
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("docker image inspect: image %s (profile %s) is not present: %v", p.image, p.name, err))
		}
	}
	return problems
}

// checkLaunchd reports the gateway's launchd agent when launchctl does not
// print it as loaded.
func (c *OpenShellGatewayCheck) checkLaunchd(run openShellRunner) []string {
	uid := c.uid
	if uid == nil {
		uid = os.Getuid
	}
	target := fmt.Sprintf("gui/%d/%s", uid(), openShellLaunchdLabel)
	if _, err := run([]string{"launchctl", "print", target}); err != nil {
		return []string{fmt.Sprintf("launchctl print %s: agent is not loaded: %v", target, err)}
	}
	return nil
}

// runOpenShellCommand is the real runner: it executes argv under
// openShellCheckTimeout and reports stderr with a failure.
func runOpenShellCommand(argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openShellCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is built by this check from the town's profiles
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
