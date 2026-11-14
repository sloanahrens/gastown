package data

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	composeFile  = "docker-compose.yml"
	doltService  = "dolt"
	bdService    = "bd"
	keepLabel    = "org.gastown.keep"
	doltMemLimit = "8g"
	bdMemLimit   = "1g"
	bdCPUs       = 1
)

// composeUnit is the part of the compose file the contract reads.
type composeUnit struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image        string                `yaml:"image"`
	Restart      string                `yaml:"restart"`
	Command      []string              `yaml:"command"`
	Environment  map[string]string     `yaml:"environment"`
	Labels       map[string]string     `yaml:"labels"`
	Ports        []string              `yaml:"ports"`
	Volumes      []string              `yaml:"volumes"`
	MemLimit     string                `yaml:"mem_limit"`
	MemswapLimit string                `yaml:"memswap_limit"`
	CPUs         float64               `yaml:"cpus"`
	Healthcheck  composeHealthcheck    `yaml:"healthcheck"`
	DependsOn    map[string]composeDep `yaml:"depends_on"`
}

type composeHealthcheck struct {
	Test []string `yaml:"test"`
}

type composeDep struct {
	Condition string `yaml:"condition"`
}

// secretKey matches a line that assigns a secret-like key, as a mapping
// (KEY: value) or as a list entry (- KEY=value). The key must start the entry,
// so a flag such as --tokens-dir or a mount path is not read as a key.
var secretKey = regexp.MustCompile(`(?i)^\s*(?:-\s+)?([A-Za-z_]*(?:password|passwd|token|secret)[A-Za-z_]*)\s*[:=]\s*(.*)$`)

// bindMount matches a volume entry whose source is a host path or a variable
// naming one, as opposed to a named volume.
var bindMount = regexp.MustCompile(`^["']?[/$]`)

// tokenMount is the one bind mount the bd gateway has: the token directory,
// read-only, taken from GT_BD_TOKEN_DIR with no default.
var tokenMount = regexp.MustCompile(`^\$\{GT_BD_TOKEN_DIR:\?[^}]+\}:/run/bdgw/tokens:ro$`)

// variableImage matches an image read from an environment variable with no
// default, ${NAME:?message}. The variable carries the image reference, so the
// file never carries a tag or a digest of its own.
func variableImage(name string) *regexp.Regexp {
	return regexp.MustCompile(`^\$\{` + name + `:\?[^}]+\}$`)
}

// parseUnit decodes the compose file. A file that does not parse, or lacks
// either service, is refused before any other check runs.
func parseUnit(raw []byte) (composeUnit, error) {
	var u composeUnit
	if err := yaml.Unmarshal(raw, &u); err != nil {
		return u, fmt.Errorf("%s is not valid YAML: %w", composeFile, err)
	}
	for _, name := range []string{doltService, bdService} {
		if _, ok := u.Services[name]; !ok {
			return u, fmt.Errorf("%s defines no %s service", composeFile, name)
		}
	}
	return u, nil
}

// checkUnit returns every way raw breaks the compose contract, joined, or nil
// when the contract holds.
func checkUnit(raw []byte) error {
	u, err := parseUnit(raw)
	if err != nil {
		return err
	}
	return errors.Join(
		checkRestart(u),
		checkLimits(u),
		checkLoopbackPorts(u),
		checkHealthChecks(u),
		checkKeepLabel(u),
		checkDependsOn(u),
		checkImages(u),
		checkBdMounts(u),
		checkNoSecrets(raw),
	)
}

func checkRestart(u composeUnit) error {
	var errs []error
	for _, name := range []string{doltService, bdService} {
		if got := u.Services[name].Restart; got != "unless-stopped" {
			errs = append(errs, fmt.Errorf("%s restart=%q; want unless-stopped", name, got))
		}
	}
	return errors.Join(errs...)
}

// checkLimits holds the Dolt cap at the 8 GiB the container runs under today,
// with swap equal so the cap is never exceeded through swap, and the bd
// gateway at its design start of 1 CPU and 1 GiB.
func checkLimits(u composeUnit) error {
	dolt := u.Services[doltService]
	bd := u.Services[bdService]
	var errs []error
	if dolt.MemLimit != doltMemLimit {
		errs = append(errs, fmt.Errorf("dolt mem_limit=%q; want %s", dolt.MemLimit, doltMemLimit))
	}
	if dolt.MemswapLimit != dolt.MemLimit {
		errs = append(errs, fmt.Errorf("dolt memswap_limit=%q; want it equal to mem_limit %q", dolt.MemswapLimit, dolt.MemLimit))
	}
	if bd.MemLimit != bdMemLimit {
		errs = append(errs, fmt.Errorf("bd mem_limit=%q; want %s", bd.MemLimit, bdMemLimit))
	}
	if bd.CPUs != bdCPUs {
		errs = append(errs, fmt.Errorf("bd cpus=%g; want %d", bd.CPUs, bdCPUs))
	}
	return errors.Join(errs...)
}

// checkLoopbackPorts requires every published port to bind 127.0.0.1, so the
// Mac's loopback is the only address that reaches either service.
func checkLoopbackPorts(u composeUnit) error {
	var errs []error
	for _, name := range []string{doltService, bdService} {
		ports := u.Services[name].Ports
		if len(ports) == 0 {
			errs = append(errs, fmt.Errorf("%s publishes no port", name))
		}
		for _, p := range ports {
			if !strings.HasPrefix(p, "127.0.0.1:") {
				errs = append(errs, fmt.Errorf("%s port %q is not bound to 127.0.0.1", name, p))
			}
		}
	}
	return errors.Join(errs...)
}

// checkHealthChecks requires each service to probe itself the way its design
// names: Dolt by a query, the gateway by its own ready check.
func checkHealthChecks(u composeUnit) error {
	var errs []error
	if len(u.Services[doltService].Healthcheck.Test) == 0 {
		errs = append(errs, errors.New("dolt has no healthcheck"))
	} else if !strings.Contains(strings.Join(u.Services[doltService].Healthcheck.Test, " "), "select 1") {
		errs = append(errs, errors.New("dolt healthcheck does not run select 1"))
	}
	if len(u.Services[bdService].Healthcheck.Test) == 0 {
		errs = append(errs, errors.New("bd has no healthcheck"))
	} else if !strings.Contains(strings.Join(u.Services[bdService].Healthcheck.Test, " "), "bdgw ready") {
		errs = append(errs, errors.New("bd healthcheck does not run bdgw ready"))
	}
	return errors.Join(errs...)
}

// checkKeepLabel requires the keep label on both services: the slot reaper
// skips containers that carry it.
func checkKeepLabel(u composeUnit) error {
	var errs []error
	for _, name := range []string{doltService, bdService} {
		if v, ok := u.Services[name].Labels[keepLabel]; !ok || v != "true" {
			errs = append(errs, fmt.Errorf("%s lacks the %s=true label", name, keepLabel))
		}
	}
	return errors.Join(errs...)
}

// checkDependsOn starts bd only once Dolt answers its health check.
func checkDependsOn(u composeUnit) error {
	dep, ok := u.Services[bdService].DependsOn[doltService]
	if !ok {
		return errors.New("bd depends_on has no dolt entry")
	}
	if dep.Condition != "service_healthy" {
		return fmt.Errorf("bd depends_on dolt condition=%q; want service_healthy", dep.Condition)
	}
	return nil
}

// checkImages requires each image to come from its environment variable with
// no default, so the image is the one the operator gave and nothing runs a tag.
func checkImages(u composeUnit) error {
	var errs []error
	if bd := u.Services[bdService].Image; !variableImage("BD_IMAGE").MatchString(bd) {
		errs = append(errs, fmt.Errorf("bd image %q must be ${BD_IMAGE:?...}, the one variable that names the gt-bd digest reference", bd))
	}
	if dolt := u.Services[doltService].Image; !variableImage("GT_DOLT_IMAGE").MatchString(dolt) {
		errs = append(errs, fmt.Errorf("dolt image %q must be ${GT_DOLT_IMAGE:?...}, a dolt-sql-server image pinned by digest", dolt))
	}
	return errors.Join(errs...)
}

// checkBdMounts refuses any host mount on bd but the token directory. The
// gateway holds no town tree: its workspace is its own volume.
func checkBdMounts(u composeUnit) error {
	var errs []error
	for _, v := range u.Services[bdService].Volumes {
		if bindMount.MatchString(v) && !tokenMount.MatchString(v) {
			errs = append(errs, fmt.Errorf("bd mounts %q; its only bind mount is the token directory, read-only", v))
		}
	}
	return errors.Join(errs...)
}

// checkNoSecrets scans the uncommented lines for a secret-like key assigned a
// literal value. A variable reference, a $$ escape and a file path name a
// secret without carrying it, so those values pass.
func checkNoSecrets(raw []byte) error {
	var errs []error
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		m := secretKey.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		value := strings.Trim(strings.TrimSpace(m[2]), `"'`)
		if value == "" || strings.HasPrefix(value, "${") || strings.HasPrefix(value, "$$") || strings.HasPrefix(value, "/") {
			continue
		}
		errs = append(errs, fmt.Errorf("%s:%d sets %s to a literal; take it from the token directory or the environment", composeFile, i+1, m[1]))
	}
	return errors.Join(errs...)
}

// loadCompose reads the real compose file and parses it.
func loadCompose(t *testing.T) ([]byte, composeUnit) {
	t.Helper()
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", composeFile, err)
	}
	u, err := parseUnit(raw)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return raw, u
}

func TestComposeLimits(t *testing.T) {
	t.Parallel()
	_, u := loadCompose(t)
	if err := checkLimits(u); err != nil {
		t.Errorf("%s limits:\n%v", composeFile, err)
	}
}

func TestComposeLoopbackPorts(t *testing.T) {
	t.Parallel()
	_, u := loadCompose(t)
	if err := checkLoopbackPorts(u); err != nil {
		t.Errorf("%s ports:\n%v", composeFile, err)
	}
}

func TestComposeHealthChecks(t *testing.T) {
	t.Parallel()
	_, u := loadCompose(t)
	if err := checkHealthChecks(u); err != nil {
		t.Errorf("%s health checks:\n%v", composeFile, err)
	}
}

func TestComposeKeepLabel(t *testing.T) {
	t.Parallel()
	_, u := loadCompose(t)
	if err := checkKeepLabel(u); err != nil {
		t.Errorf("%s keep label:\n%v", composeFile, err)
	}
}

func TestComposeNoSecrets(t *testing.T) {
	t.Parallel()
	raw, _ := loadCompose(t)
	if err := checkNoSecrets(raw); err != nil {
		t.Errorf("%s carries a secret:\n%v", composeFile, err)
	}
}

func TestComposeDependsOn(t *testing.T) {
	t.Parallel()
	_, u := loadCompose(t)
	if err := checkDependsOn(u); err != nil {
		t.Errorf("%s depends_on:\n%v", composeFile, err)
	}
}

// TestComposeBdImageVariable holds the bd image to the one documented variable,
// BD_IMAGE, required: never a literal, a tag, or a default that renders without it.
func TestComposeBdImageVariable(t *testing.T) {
	t.Parallel()
	_, u := loadCompose(t)
	if err := checkImages(u); err != nil {
		t.Errorf("%s images:\n%v", composeFile, err)
	}
}

// TestComposeContract runs every check against the real compose file.
func TestComposeContract(t *testing.T) {
	t.Parallel()
	raw, _ := loadCompose(t)
	if err := checkUnit(raw); err != nil {
		t.Errorf("%s breaks the compose contract:\n%v", composeFile, err)
	}
}

// refusals each mutate one line of the real compose file. Every row must fail
// the contract with an error containing want.
var refusals = []struct {
	name string
	old  string
	new  string
	want string
}{
	{"not YAML", "services:\n", "services: [\n", "is not valid YAML"},
	{"no bd service", "  bd:\n    image:", "  bdx:\n    image:", "defines no bd service"},
	{"dolt cap raised", "    mem_limit: 8g\n", "    mem_limit: 16g\n", `dolt mem_limit="16g"`},
	{"dolt cap missing", "    mem_limit: 8g\n", "", `dolt mem_limit=""`},
	{"dolt swap differs", "memswap_limit: 8g", "memswap_limit: 16g", `dolt memswap_limit="16g"`},
	{"bd cap raised", "mem_limit: 1g", "mem_limit: 2g", `bd mem_limit="2g"`},
	{"bd cpus missing", "    cpus: 1\n", "", "bd cpus=0"},
	{"dolt port not loopback", `"127.0.0.1:3307:3306"`, `"3307:3306"`, `dolt port "3307:3306" is not bound to 127.0.0.1`},
	{"bd publishes no port", "      - \"127.0.0.1:7440:7440\"\n", "", "bd publishes no port"},
	{"dolt healthcheck not a query", `"-q", "select 1"]`, `"-q", "true"]`, "dolt healthcheck does not run select 1"},
	{"dolt has no healthcheck", "    memswap_limit: 8g\n    healthcheck:\n", "    memswap_limit: 8g\n    healthchek:\n", "dolt has no healthcheck"},
	{"bd healthcheck not bdgw ready", `"/usr/local/bin/bdgw", "ready"`, `"/bin/true", "ready"`, "bd healthcheck does not run bdgw ready"},
	{"bd has no healthcheck", "    cpus: 1\n    healthcheck:\n", "    cpus: 1\n    healthchek:\n", "bd has no healthcheck"},
	{"bd keep label wrong", `org.gastown.keep: "true"` + "\n    ports:\n      - \"127.0.0.1:7440", `org.gastown.keep: "false"` + "\n    ports:\n      - \"127.0.0.1:7440", "bd lacks the org.gastown.keep=true label"},
	{"bd depends_on removed", "    depends_on:\n      dolt:\n        condition: service_healthy\n", "", "bd depends_on has no dolt entry"},
	{"bd depends_on not healthy", "condition: service_healthy", "condition: service_started", `bd depends_on dolt condition="service_started"; want service_healthy`},
	{"bd image literal", "${BD_IMAGE:?set BD_IMAGE to the gt-bd image as a digest reference (name@sha256:...)}", "forgejo:3000/bot-registry/gt-bd:latest", "must be ${BD_IMAGE:?...}"},
	{"bd image old variable name", "${BD_IMAGE:?set", "${GT_BD_IMAGE:?set", "must be ${BD_IMAGE:?...}"},
	{"bd image default renders unset", "${BD_IMAGE:?set BD_IMAGE to the gt-bd image as a digest reference (name@sha256:...)}", "${BD_IMAGE:-forgejo:3000/bot-registry/gt-bd:latest}", "must be ${BD_IMAGE:?...}"},
	{"dolt image literal", "${GT_DOLT_IMAGE:?set GT_DOLT_IMAGE to the dolt-sql-server image pinned by digest}", "dolthub/dolt-sql-server:2.7.2", "must be ${GT_DOLT_IMAGE:?...}"},
	{"bd town mount", "      - bd-logs:/var/log/bdgw\n", "      - bd-logs:/var/log/bdgw\n      - /Users/sloan/gt:/Users/sloan/gt\n", "its only bind mount is the token directory"},
	{"bd token mount writable", ":/run/bdgw/tokens:ro", ":/run/bdgw/tokens", "its only bind mount is the token directory"},
	{"restart wrong", "    restart: unless-stopped\n    depends_on:", "    restart: always\n    depends_on:", `bd restart="always"; want unless-stopped`},
	{"password literal", "      HOME: /home/bdgw\n", "      HOME: /home/bdgw\n      DOLT_ROOT_PASSWORD: hunter2\n", "sets DOLT_ROOT_PASSWORD to a literal"},
	{"token literal", "TRACKER_TOKEN_FILE: /run/bdgw/tokens/tracker", "TRACKER_TOKEN_FILE: abc123", "sets TRACKER_TOKEN_FILE to a literal"},
}

// TestComposeRefusals drives each refusal of the contract on a mutated copy of
// the real file. Each row must name its own failure.
func TestComposeRefusals(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", composeFile, err)
	}
	if err := checkUnit(raw); err != nil {
		t.Fatalf("the file must pass the contract before any mutation: %v", err)
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if n := strings.Count(string(raw), tt.old); n != 1 {
				t.Fatalf("%q occurs %d times in %s; want exactly once", tt.old, n, composeFile)
			}
			mutated := strings.Replace(string(raw), tt.old, tt.new, 1)
			err := checkUnit([]byte(mutated))
			if err == nil {
				t.Fatalf("mutated file passed the contract; want an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v; want it to contain %q", err, tt.want)
			}
		})
	}
}
