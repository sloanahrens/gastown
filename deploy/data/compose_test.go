package data

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
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

// secretName matches a key or a KEY=value name that names a secret.
var secretName = regexp.MustCompile(`(?i)password|passwd|token|secret`)

// envEntry matches a list entry in KEY=value form, as compose's environment list takes.
var envEntry = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// secretRef matches a value that names a secret without carrying it: a
// variable with no default, or a file in the token directory. A default such
// as ${NAME:-x} carries x, and a $$ escape is a literal dollar, so neither matches.
var secretRef = regexp.MustCompile(`^(\$\{[A-Za-z_][A-Za-z0-9_]*(:\?[^}]*)?\}|\$[A-Za-z_][A-Za-z0-9_]*|/run/bdgw/tokens/[A-Za-z0-9._-]+)$`)

// namedVolume matches a volume entry whose source is a Docker named volume: a
// bare name with no path separator. Every other source is a host path.
var namedVolume = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*:`)

// tokenMount is the one host mount the bd gateway has: the token directory,
// read-only, taken from GT_BD_TOKEN_DIR with no default.
var tokenMount = regexp.MustCompile(`^\$\{GT_BD_TOKEN_DIR:\?[^}]+\}:/run/bdgw/tokens:ro$`)

// variableImage matches an image read from an environment variable with no
// default, ${NAME:?message}, whose message names the digest form. Compose cannot
// validate the value it is given; the value is set where the unit is started (G15).
func variableImage(name, digestForm string) *regexp.Regexp {
	return regexp.MustCompile(`^\$\{` + name + `:\?[^}]*` + regexp.QuoteMeta(digestForm) + `[^}]*\}$`)
}

// probeArgv is the exec form of a healthcheck: CMD, then exactly these words.
// A shell form, or a different command with the same words, does not match.
func probeArgv(test []string, words []string) bool {
	return len(test) == len(words)+1 && test[0] == "CMD" && slices.Equal(test[1:], words)
}

var (
	doltProbe = []string{"dolt", "--host", "127.0.0.1", "--port", "3306", "-u", "root", "sql", "-q", "select 1"}
	bdProbe   = []string{"/usr/local/bin/bdgw", "ready", "--tokens-dir", "/run/bdgw/tokens", "--verbs", "/etc/bdgw/verbs.json", "--workspace", "/ws"}
)

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
// names: Dolt by a query, the gateway by its own ready check, both in exec form.
func checkHealthChecks(u composeUnit) error {
	var errs []error
	dolt := u.Services[doltService].Healthcheck.Test
	if len(dolt) == 0 {
		errs = append(errs, errors.New("dolt has no healthcheck"))
	} else if !probeArgv(dolt, doltProbe) {
		errs = append(errs, errors.New("dolt healthcheck does not run select 1 through dolt, in CMD form"))
	}
	bd := u.Services[bdService].Healthcheck.Test
	if len(bd) == 0 {
		errs = append(errs, errors.New("bd has no healthcheck"))
	} else if !probeArgv(bd, bdProbe) {
		errs = append(errs, errors.New("bd healthcheck does not run bdgw ready, in CMD form"))
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
// no default. The variable's message must name the digest form, so the
// operator is told a digest is required. The value itself is checked where
// it is set (gt dolt start, G15), not here.
func checkImages(u composeUnit) error {
	var errs []error
	if bd := u.Services[bdService].Image; !variableImage("BD_IMAGE", "@sha256:").MatchString(bd) {
		errs = append(errs, fmt.Errorf("bd image %q must be ${BD_IMAGE:?...} with a message naming the digest reference (name@sha256:...)", bd))
	}
	if dolt := u.Services[doltService].Image; !variableImage("GT_DOLT_IMAGE", "digest").MatchString(dolt) {
		errs = append(errs, fmt.Errorf("dolt image %q must be ${GT_DOLT_IMAGE:?...} with a message naming the digest", dolt))
	}
	return errors.Join(errs...)
}

// checkBdMounts refuses any host mount on bd but the token directory. A
// volume whose source is not a bare Docker name, including ./, ../ and ~/
// paths and variables, is a host mount. The gateway holds no town tree: its
// workspace is its own volume.
func checkBdMounts(u composeUnit) error {
	var errs []error
	for _, v := range u.Services[bdService].Volumes {
		if !namedVolume.MatchString(v) && !tokenMount.MatchString(v) {
			errs = append(errs, fmt.Errorf("bd mounts %q; its only host mount is the token directory, read-only", v))
		}
	}
	return errors.Join(errs...)
}

// checkNoSecrets decodes the file and walks every mapping key and every
// KEY=value list entry. A name that names a secret must take a reference
// (checked by secretRef), never a literal. Decoding first reads a flow mapping,
// a quoted key, or a $$ escape as the value it carries, and comments are gone.
func checkNoSecrets(raw []byte) error {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s is not valid YAML: %w", composeFile, err)
	}
	var errs []error
	walkSecrets(doc, &errs)
	return errors.Join(errs...)
}

// walkSecrets visits node and everything under it. String keys are visited in
// sorted order, so the errors come out the same on every run.
func walkSecrets(node any, errs *[]error) {
	switch n := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if secretName.MatchString(k) {
				checkSecretValue(k, n[k], errs)
			}
			walkSecrets(n[k], errs)
		}
	case map[any]any:
		for k, v := range n {
			name := fmt.Sprint(k)
			if secretName.MatchString(name) {
				checkSecretValue(name, v, errs)
			}
			walkSecrets(v, errs)
		}
	case []any:
		for _, item := range n {
			if s, ok := item.(string); ok {
				if m := envEntry.FindStringSubmatch(s); m != nil && secretName.MatchString(m[1]) {
					checkSecretValue(m[1], m[2], errs)
				}
				continue
			}
			walkSecrets(item, errs)
		}
	}
}

// checkSecretValue refuses a literal under a secret name. An empty value and a
// reference (secretRef) pass.
func checkSecretValue(name string, value any, errs *[]error) {
	s := ""
	switch v := value.(type) {
	case nil:
	case string:
		s = v
	default:
		s = fmt.Sprint(v)
	}
	if s == "" || secretRef.MatchString(s) {
		return
	}
	*errs = append(*errs, fmt.Errorf("%s sets %s to a literal; take it from the token directory or the environment", composeFile, name))
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
	{"dolt healthcheck is a shell form", `"CMD", "dolt", "--host"`, `"CMD-SHELL", "dolt", "--host"`, "dolt healthcheck does not run select 1"},
	{"dolt has no healthcheck", "    memswap_limit: 8g\n    healthcheck:\n", "    memswap_limit: 8g\n    healthchek:\n", "dolt has no healthcheck"},
	{"bd healthcheck not bdgw ready", `"/usr/local/bin/bdgw", "ready"`, `"/bin/true", "ready"`, "bd healthcheck does not run bdgw ready"},
	{"bd healthcheck is a shell form", `"CMD", "/usr/local/bin/bdgw"`, `"CMD-SHELL", "/usr/local/bin/bdgw"`, "bd healthcheck does not run bdgw ready"},
	{"bd has no healthcheck", "    cpus: 1\n    healthcheck:\n", "    cpus: 1\n    healthchek:\n", "bd has no healthcheck"},
	{"bd keep label wrong", `org.gastown.keep: "true"` + "\n    ports:\n      - \"127.0.0.1:7440", `org.gastown.keep: "false"` + "\n    ports:\n      - \"127.0.0.1:7440", "bd lacks the org.gastown.keep=true label"},
	{"bd depends_on removed", "    depends_on:\n      dolt:\n        condition: service_healthy\n", "", "bd depends_on has no dolt entry"},
	{"bd depends_on not healthy", "condition: service_healthy", "condition: service_started", `bd depends_on dolt condition="service_started"; want service_healthy`},
	{"bd image literal", "${BD_IMAGE:?set BD_IMAGE to the gt-bd image as a digest reference (name@sha256:...)}", "forgejo:3000/bot-registry/gt-bd:latest", "must be ${BD_IMAGE:?...}"},
	{"bd image old variable name", "${BD_IMAGE:?set", "${GT_BD_IMAGE:?set", "must be ${BD_IMAGE:?...}"},
	{"bd image default renders unset", "${BD_IMAGE:?set BD_IMAGE to the gt-bd image as a digest reference (name@sha256:...)}", "${BD_IMAGE:-forgejo:3000/bot-registry/gt-bd:latest}", "must be ${BD_IMAGE:?...}"},
	{"bd image message names no digest", "(name@sha256:...)", "(name:latest)", "must be ${BD_IMAGE:?...}"},
	{"dolt image literal", "${GT_DOLT_IMAGE:?set GT_DOLT_IMAGE to the dolt-sql-server image pinned by digest}", "dolthub/dolt-sql-server:2.7.2", "must be ${GT_DOLT_IMAGE:?...}"},
	{"dolt image message names no digest", "pinned by digest}", "pinned by tag}", "must be ${GT_DOLT_IMAGE:?...}"},
	{"bd town mount", "      - bd-logs:/var/log/bdgw\n", "      - bd-logs:/var/log/bdgw\n      - /Users/sloan/gt:/Users/sloan/gt\n", "its only host mount is the token directory"},
	{"bd relative mount", "      - bd-logs:/var/log/bdgw\n", "      - bd-logs:/var/log/bdgw\n      - ./gt:/town\n", "its only host mount is the token directory"},
	{"bd parent-relative mount", "      - bd-logs:/var/log/bdgw\n", "      - bd-logs:/var/log/bdgw\n      - ../gt:/town\n", "its only host mount is the token directory"},
	{"bd home-relative mount", "      - bd-logs:/var/log/bdgw\n", "      - bd-logs:/var/log/bdgw\n      - ~/gt:/town\n", "its only host mount is the token directory"},
	{"bd token mount writable", ":/run/bdgw/tokens:ro", ":/run/bdgw/tokens", "its only host mount is the token directory"},
	{"restart wrong", "    restart: unless-stopped\n    depends_on:", "    restart: always\n    depends_on:", `bd restart="always"; want unless-stopped`},
	{"password literal", "      HOME: /home/bdgw\n", "      HOME: /home/bdgw\n      DOLT_ROOT_PASSWORD: hunter2\n", "sets DOLT_ROOT_PASSWORD to a literal"},
	{"password escaped dollar", "      HOME: /home/bdgw\n", "      HOME: /home/bdgw\n      DOLT_ROOT_PASSWORD: $$hunter2\n", "sets DOLT_ROOT_PASSWORD to a literal"},
	{"password in flow mapping under an extension", "name: gt-data\n", "name: gt-data\nx-extra: {DOLT_ROOT_PASSWORD: hunter2}\n", "sets DOLT_ROOT_PASSWORD to a literal"},
	{"password in list entry", "      - /ws\n", "      - /ws\n      - DOLT_ROOT_PASSWORD=hunter2\n", "sets DOLT_ROOT_PASSWORD to a literal"},
	{"token literal", "TRACKER_TOKEN_FILE: /run/bdgw/tokens/tracker", "TRACKER_TOKEN_FILE: abc123", "sets TRACKER_TOKEN_FILE to a literal"},
	{"token literal behind a slash", "TRACKER_TOKEN_FILE: /run/bdgw/tokens/tracker", "TRACKER_TOKEN_FILE: /etc/shadow", "sets TRACKER_TOKEN_FILE to a literal"},
	{"token default carries a literal", "TRACKER_TOKEN_FILE: /run/bdgw/tokens/tracker", "TRACKER_TOKEN_FILE: ${TRACKER_TOKEN_FILE:-abc123}", "sets TRACKER_TOKEN_FILE to a literal"},
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
