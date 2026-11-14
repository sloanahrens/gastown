package doctor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/templates"
)

// The data compose unit (deploy/data/docker-compose.yml, project gt-data) runs
// Dolt and the bd gateway. The names and the port below are the ones that file
// fixes; a change to it is a change here.
const (
	dataUnitDoltContainer = "gt-dolt"
	dataUnitBdContainer   = "gt-bd"
	// dataUnitGatewayURL is the gateway's published address on the Mac host.
	dataUnitGatewayURL = "http://127.0.0.1:7440"
	// dataUnitTokenDirEnv names the token directory the compose unit mounts.
	dataUnitTokenDirEnv = "GT_BD_TOKEN_DIR"
	// dataUnitTokenPrefix starts the token file the readiness rung presents,
	// the daemon class's (`<class>.<caller>.token`).
	dataUnitTokenPrefix = "daemon."
	// dataUnitTokenMode is the only mode a gateway token file may hold.
	dataUnitTokenMode = os.FileMode(0o600)
	// dataUnitInspectFormat prints a container's state and health as two
	// fields, so a container with no health check reads as "none".
	dataUnitInspectFormat = "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}"
)

// DataUnitCheck reports whether the data compose unit is healthy. It walks the
// rungs in order and names the first that fails: the Dolt container, the bd
// container, the token directory's modes, then the gateway's /readyz with a
// daemon token. A token file wider than 0600 stops the walk before /readyz, so
// the gateway is never asked with a token that is readable by others.
//
// The check is skipped, not failed, when the compose unit is not the town's
// selected Dolt path. Report only; it starts and repairs nothing.
type DataUnitCheck struct {
	BaseCheck

	// selected reports whether the compose unit is the town's selected Dolt
	// path. Nil is refused: a check that cannot tell whether it applies must
	// not pass.
	selected func(townRoot string) (bool, error)

	// getenv reads GT_BD_TOKEN_DIR. Nil is os.Getenv.
	getenv func(string) string

	// docker inspects the containers. Nil is templates.RunDockerInspect.
	docker templates.ContainerDocker

	// http answers /readyz. Nil is a client bounded by readyzTimeout.
	http *http.Client
}

// NewDataUnitCheck creates the data compose unit check.
func NewDataUnitCheck() *DataUnitCheck {
	return &DataUnitCheck{
		BaseCheck: BaseCheck{
			CheckName:        "data-unit",
			CheckDescription: "Check the data compose unit: Dolt and bd containers healthy, gateway readyz, token file modes",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run walks the rungs of the data compose unit.
func (c *DataUnitCheck) Run(ctx *CheckContext) *CheckResult {
	res := &CheckResult{Name: c.Name(), Category: c.Category()}
	if c.selected == nil {
		res.Status = StatusError
		res.Message = "the compose unit's selection is not wired, so the data unit check cannot tell whether it applies"
		return res
	}
	on, err := c.selected(ctx.TownRoot)
	if err != nil {
		res.Status = StatusError
		res.Message = "cannot read whether the compose unit is the selected Dolt path"
		res.Details = []string{err.Error()}
		return res
	}
	if !on {
		res.Status = StatusSkipped
		res.Message = "the compose unit is not the selected Dolt path; not applicable"
		return res
	}

	getenv := c.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	run := c.docker
	if run == nil {
		run = templates.RunDockerInspect
	}
	client := c.http
	if client == nil {
		client = &http.Client{Timeout: readyzTimeout}
	}

	var passed []string
	fail := func(rung string, err error) *CheckResult {
		res.Status = StatusError
		res.Message = fmt.Sprintf("data unit rung %s failed: %v", rung, err)
		res.Details = passed
		return res
	}

	for _, container := range []string{dataUnitDoltContainer, dataUnitBdContainer} {
		rung := "dolt"
		if container == dataUnitBdContainer {
			rung = "bd"
		}
		if err := dataUnitContainerHealthy(run, container); err != nil {
			return fail(rung, err)
		}
		passed = append(passed, fmt.Sprintf("%s: %s is running and healthy", rung, container))
	}

	dir := getenv(dataUnitTokenDirEnv)
	if dir == "" {
		return fail("tokens", fmt.Errorf("%s is not set, so the gateway's token directory is unknown", dataUnitTokenDirEnv))
	}
	daemonFile, wide, err := dataUnitTokenFiles(dir)
	if len(wide) > 0 {
		return fail("tokens", fmt.Errorf("token files not mode 0600: %s", strings.Join(wide, ", ")))
	}
	if err != nil {
		return fail("tokens", err)
	}
	passed = append(passed, fmt.Sprintf("tokens: every file in %s is mode 0600", dir))

	token, err := os.ReadFile(filepath.Join(dir, daemonFile))
	if err != nil {
		return fail("readyz", fmt.Errorf("the daemon token file %s cannot be read", daemonFile))
	}
	if err := dataUnitReadyz(client, strings.TrimSpace(string(token))); err != nil {
		return fail("readyz", err)
	}
	passed = append(passed, fmt.Sprintf("readyz: %s answers true with %s", dataUnitGatewayURL, daemonFile))

	res.Status = StatusOK
	res.Message = "the data compose unit is healthy: Dolt and bd are up, the gateway is ready, and the token files are mode 0600"
	res.Details = passed
	return res
}

// dataUnitContainerHealthy is nil when the named container is running and its
// health check reports healthy. Anything else is named by its state. The docker
// error is its exit alone, never its output.
func dataUnitContainerHealthy(run templates.ContainerDocker, container string) error {
	ctx, cancel := context.WithTimeout(context.Background(), templates.ContainerInspectTimeout)
	defer cancel()
	out, err := run(ctx, []string{"docker", "inspect", "-f", dataUnitInspectFormat, container})
	if err != nil {
		return fmt.Errorf("container %s could not be inspected: %v", container, err)
	}
	state := strings.Fields(string(out))
	if len(state) != 2 {
		return fmt.Errorf("container %s reported no state and health", container)
	}
	if state[0] != "running" {
		return fmt.Errorf("container %s is %s, not running", container, state[0])
	}
	if state[1] != "healthy" {
		return fmt.Errorf("container %s is running but its health is %s, not healthy", container, state[1])
	}
	return nil
}

// dataUnitTokenFiles reads the gateway's token directory. Every entry must be a
// regular file of mode 0600; the ones that are not are returned by name in wide,
// and the caller stops there. daemonFile is the first daemon-class token file in
// name order, the one the readiness rung presents.
func dataUnitTokenFiles(dir string) (daemonFile string, wide []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, fmt.Errorf("the token directory %s cannot be read: %w", dir, err)
	}
	if len(entries) == 0 {
		return "", nil, fmt.Errorf("the token directory %s holds no token files", dir)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return "", nil, fmt.Errorf("the token file %s cannot be stat'd: %w", e.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != dataUnitTokenMode {
			wide = append(wide, fmt.Sprintf("%s (mode %04o)", e.Name(), info.Mode().Perm()))
			continue
		}
		if daemonFile == "" && strings.HasPrefix(e.Name(), dataUnitTokenPrefix) {
			daemonFile = e.Name()
		}
	}
	if len(wide) > 0 {
		return "", wide, nil
	}
	if daemonFile == "" {
		return "", nil, fmt.Errorf("the token directory %s holds no %s*.token file to present to the gateway", dir, dataUnitTokenPrefix)
	}
	return daemonFile, nil, nil
}

// dataUnitReadyz asks the gateway's /readyz with token. Only HTTP 200 is ready,
// as the gateway's own client reads it. The token is never put in an error.
func dataUnitReadyz(client *http.Client, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), readyzTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dataUnitGatewayURL+"/readyz", nil)
	if err != nil {
		return fmt.Errorf("the gateway readiness request cannot be built: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("the gateway at %s did not answer /readyz: %w", dataUnitGatewayURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, readyzBodyLimit))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the gateway answered /readyz with HTTP %d, not ready", resp.StatusCode)
	}
	return nil
}
