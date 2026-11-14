package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// DataUnitCheckName is the name 'gt doctor --check' takes for DataUnitCheck.
const DataUnitCheckName = "data-unit"

// DataUnitPsFormat is the docker compose ps template the check reads the unit's
// services with: one line per service, its name, state and health. The compose
// unit's status command prints the same template.
const DataUnitPsFormat = "{{.Service}} {{.State}} {{.Health}}"

// The data compose unit (deploy/data/docker-compose.yml, project gt-data) runs
// Dolt and the bd gateway. The service names and the port below are the ones
// that file fixes; a change to it is a change here.
const (
	dataUnitDoltService = "dolt"
	dataUnitBdService   = "bd"
	// dataUnitGatewayURL is the gateway's published address on the Mac host.
	dataUnitGatewayURL = "http://127.0.0.1:7440"
	// dataUnitTokenPrefix starts the token file the readiness rung presents,
	// the daemon class's (`<class>.<caller>.token`).
	dataUnitTokenPrefix = "daemon."
	// dataUnitTokenMode is the only mode a gateway token file may hold.
	dataUnitTokenMode = os.FileMode(0o600)
)

// DataUnitTokenDir is the gateway's token directory for the town. gt dolt start
// sets it as GT_BD_TOKEN_DIR for the compose unit, which mounts it read-only, and
// the check lists it. It is untracked runtime state under the town's .runtime.
func DataUnitTokenDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "bd-gateway", "tokens")
}

// DataUnitCheck reports whether the data compose unit is healthy. It walks the
// rungs in order and names the first that fails: the unit's status, the Dolt
// service, the bd service, the token directory's modes, then the gateway's
// /readyz with a daemon token. A token file wider than 0600 stops the walk
// before /readyz, so the gateway is never asked with a token that is readable
// by others.
//
// The check is opt-in by name: it runs only when 'gt doctor --check data-unit'
// names it. Otherwise it reports SKIPPED with the reason and never fails; that
// SKIPPED line is intended, and a default 'gt doctor' run lists it. It reads,
// never writes: no start, stop or repair.
type DataUnitCheck struct {
	BaseCheck

	// named reports whether --check named this check.
	named bool

	// ps returns the unit's services in DataUnitPsFormat, for the town. Nil is
	// refused when the check is named: a check that cannot read the unit must
	// not pass.
	ps func(townRoot string) ([]byte, error)

	// http answers /readyz. Nil is a client bounded by readyzTimeout.
	http *http.Client
}

// NewDataUnitCheck creates the data compose unit check. named is whether
// --check named it; ps reads the unit's services.
func NewDataUnitCheck(named bool, ps func(townRoot string) ([]byte, error)) *DataUnitCheck {
	return &DataUnitCheck{
		BaseCheck: BaseCheck{
			CheckName:        DataUnitCheckName,
			CheckDescription: "Check the data compose unit: Dolt and bd services healthy, gateway readyz, token file modes",
			CheckCategory:    CategoryInfrastructure,
		},
		named: named,
		ps:    ps,
	}
}

// Run walks the rungs of the data compose unit.
func (c *DataUnitCheck) Run(ctx *CheckContext) *CheckResult {
	res := &CheckResult{Name: c.Name(), Category: c.Category()}
	if !c.named {
		res.Status = StatusSkipped
		res.Message = "not selected: name it with --check data-unit"
		return res
	}
	if c.ps == nil {
		res.Status = StatusError
		res.Message = "the compose unit's status is not wired, so the data unit check cannot read it"
		return res
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

	out, err := c.ps(ctx.TownRoot)
	if err != nil {
		return fail("unit", err)
	}
	for _, service := range []string{dataUnitDoltService, dataUnitBdService} {
		if err := dataUnitServiceHealthy(out, service); err != nil {
			return fail(service, err)
		}
		passed = append(passed, fmt.Sprintf("service %s is running and healthy", service))
	}

	dir := DataUnitTokenDir(ctx.TownRoot)
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
	if err := dataUnitReadyz(client, dataUnitGatewayURL, strings.TrimSpace(string(token))); err != nil {
		return fail("readyz", err)
	}
	passed = append(passed, fmt.Sprintf("readyz: %s answers true with %s", dataUnitGatewayURL, daemonFile))

	res.Status = StatusOK
	res.Message = "the data compose unit is healthy: Dolt and bd are up, the gateway is ready, and the token files are mode 0600"
	res.Details = passed
	return res
}

// dataUnitServiceHealthy is nil when service is listed once in out, running, and
// its health check reports healthy. out is DataUnitPsFormat's lines. Each other
// case is an error that names the service and what it is: missing, listed twice,
// not running, or not healthy. A service with no health check reads as "none".
func dataUnitServiceHealthy(out []byte, service string) error {
	var fields []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != service {
			continue
		}
		if fields != nil {
			return fmt.Errorf("service %s is listed more than once", service)
		}
		fields = f
	}
	if fields == nil {
		return fmt.Errorf("service %s is not in the compose unit", service)
	}
	if len(fields) < 2 {
		return fmt.Errorf("service %s reports no state", service)
	}
	if fields[1] != "running" {
		return fmt.Errorf("service %s is %s, not running", service, fields[1])
	}
	health := "none"
	if len(fields) > 2 {
		health = fields[2]
	}
	if health != "healthy" {
		return fmt.Errorf("service %s is running but its health is %s, not healthy", service, health)
	}
	return nil
}

// dataUnitTokenFiles reads the gateway's token directory. Every entry must be a
// regular file of mode 0600; the ones that are not are returned by name in wide,
// and the caller stops there. daemonFile is the first daemon-class token file in
// name order, the one the readiness rung presents. A directory that does not
// exist is an error that names the path and how to create it.
func dataUnitTokenFiles(dir string) (daemonFile string, wide []string, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, fmt.Errorf("no token directory at %s: create it as the design's step 4 says, mode 0700, one <class>.<caller>.token file each at mode 0600", dir)
	}
	if err != nil {
		return "", nil, fmt.Errorf("the token directory %s cannot be read: %w", dir, err)
	}
	return dataUnitTokenEntries(dir, entries)
}

// dataUnitTokenEntries is dataUnitTokenFiles over the directory's entries, so a
// test can hand it an entry whose stat fails.
func dataUnitTokenEntries(dir string, entries []fs.DirEntry) (daemonFile string, wide []string, err error) {
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

// dataUnitReadyz asks the gateway's /readyz at base with token. Only HTTP 200 is
// ready, as the gateway's own client reads it. The token is never put in an
// error, and the body is not read: its status alone decides the rung.
func dataUnitReadyz(client *http.Client, base, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), readyzTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
	if err != nil {
		return fmt.Errorf("the gateway readiness request cannot be built: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("the gateway at %s did not answer /readyz: %w", base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the gateway answered /readyz with HTTP %d, not ready", resp.StatusCode)
	}
	return nil
}
