package tracker

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const composeFile = "docker-compose.yml"

// passwordEnvFile is the env file beside the compose file that carries the
// tracker-db password.
const passwordEnvFile = "tracker-db.env"

// passwordLine finds a password assignment in a compose line, as a mapping
// (POSTGRES_PASSWORD: x) or as a list entry (- POSTGRES_PASSWORD=x).
var passwordLine = regexp.MustCompile(`(?i)password[a-z_]*\s*[:=]\s*(.*)$`)

// composeEscape matches a variable reference escaped for the container,
// $$NAME. Compose turns it into $NAME, so the file carries a reference and
// never the value.
var composeEscape = regexp.MustCompile(`^\$\$[A-Za-z_][A-Za-z0-9_]*$`)

// maxConnectionsFlag matches the server flag value in a command list.
var maxConnectionsFlag = regexp.MustCompile(`^max_connections=[1-9]\d*$`)

// memLimit matches a Docker memory size with a unit, such as 2g or 512m.
var memLimit = regexp.MustCompile(`(?i)^[1-9]\d*[bkmg]$`)

// pgImage matches a Postgres 17 exact minor tag or a digest pin.
var pgImage = regexp.MustCompile(`^postgres:17\.\d+$|^postgres@sha256:[0-9a-f]{64}$`)

// checkCompose returns every way raw breaks the tracker-db contract, joined,
// or nil when the contract holds. A file that cannot be parsed or has no
// tracker-db service is refused before any other check runs.
func checkCompose(raw []byte) error {
	svc, err := parseTrackerDB(raw)
	if err != nil {
		return err
	}
	return errors.Join(
		checkImage(svc),
		checkRestart(svc),
		checkEnvFile(svc),
		checkLimits(svc),
		checkMaxConnections(svc),
		checkLoopbackPorts(svc),
		checkDataDir(svc),
		checkHealthcheck(svc),
		checkNoPasswordLiteral(raw),
		checkHeaderDSN(raw),
	)
}

// parseTrackerDB decodes the tracker-db service from the compose file.
func parseTrackerDB(raw []byte) (map[string]any, error) {
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid YAML: %w", composeFile, err)
	}
	svc, ok := doc.Services["tracker-db"]
	if !ok {
		return nil, fmt.Errorf("%s defines no tracker-db service", composeFile)
	}
	return svc, nil
}

// field returns a tracker-db key as text. A missing key is an error, so a
// refusal names the key it lacks.
func field(svc map[string]any, key string) (string, error) {
	return serviceField("tracker-db", svc, key)
}

// serviceField returns a service key as text. A missing key is an error, so a
// refusal names the service and the key it lacks.
func serviceField(name string, svc map[string]any, key string) (string, error) {
	v, ok := svc[key]
	if !ok {
		return "", fmt.Errorf("%s has no %s", name, key)
	}
	return fmt.Sprint(v), nil
}

func checkImage(svc map[string]any) error {
	image, err := field(svc, "image")
	if err != nil {
		return err
	}
	if !pgImage.MatchString(image) {
		return fmt.Errorf("tracker-db image %q is not postgres 17 pinned to a minor tag or digest", image)
	}
	return nil
}

func checkRestart(svc map[string]any) error {
	restart, err := field(svc, "restart")
	if err != nil {
		return err
	}
	if restart != "unless-stopped" {
		return fmt.Errorf("tracker-db restart=%q; want unless-stopped", restart)
	}
	return nil
}

func checkEnvFile(svc map[string]any) error {
	envFile, err := field(svc, "env_file")
	if err != nil {
		return err
	}
	if envFile != passwordEnvFile {
		return fmt.Errorf("tracker-db env_file=%q; want %s, where the password lives", envFile, passwordEnvFile)
	}
	return nil
}

func checkLimits(svc map[string]any) error {
	mem, err := field(svc, "mem_limit")
	if err != nil {
		return err
	}
	if !memLimit.MatchString(mem) {
		return fmt.Errorf("tracker-db mem_limit=%q; want a size with a unit such as 2g", mem)
	}
	cpus, err := field(svc, "cpus")
	if err != nil {
		return err
	}
	if n, err := strconv.ParseFloat(cpus, 64); err != nil || n <= 0 {
		return fmt.Errorf("tracker-db cpus=%q; want a number above 0", cpus)
	}
	pids, err := field(svc, "pids_limit")
	if err != nil {
		return err
	}
	if n, err := strconv.Atoi(pids); err != nil || n <= 0 {
		return fmt.Errorf("tracker-db pids_limit=%q; want a whole number above 0", pids)
	}
	return nil
}

func checkMaxConnections(svc map[string]any) error {
	cmd, _ := svc["command"].([]any)
	for i := 0; i+1 < len(cmd); i++ {
		if fmt.Sprint(cmd[i]) != "-c" {
			continue
		}
		if maxConnectionsFlag.MatchString(fmt.Sprint(cmd[i+1])) {
			return nil
		}
	}
	return fmt.Errorf("tracker-db command has no -c max_connections=N flag above 0: %v", cmd)
}

func checkLoopbackPorts(svc map[string]any) error {
	ports, _ := svc["ports"].([]any)
	if len(ports) == 0 {
		return errors.New("tracker-db publishes no port")
	}
	for _, p := range ports {
		if !strings.HasPrefix(fmt.Sprint(p), "127.0.0.1:") {
			return fmt.Errorf("tracker-db port %v is not bound to 127.0.0.1", p)
		}
	}
	return nil
}

// checkDataDir requires the cluster directory to come from TRACKER_PG_DATA
// with no default, so an unset variable stops compose instead of writing
// data to a path the operator did not choose.
func checkDataDir(svc map[string]any) error {
	const target = ":/var/lib/postgresql/data"
	vols, _ := svc["volumes"].([]any)
	for _, v := range vols {
		entry := fmt.Sprint(v)
		if !strings.HasSuffix(entry, target) {
			continue
		}
		if strings.HasPrefix(entry, "${TRACKER_PG_DATA:?") {
			return nil
		}
		return fmt.Errorf("tracker-db data volume %q does not take its path from TRACKER_PG_DATA with no default", entry)
	}
	return errors.New("tracker-db mounts no data volume at /var/lib/postgresql/data")
}

func checkHealthcheck(svc map[string]any) error {
	hc, ok := svc["healthcheck"].(map[string]any)
	if !ok {
		return errors.New("tracker-db has no healthcheck")
	}
	if !strings.Contains(fmt.Sprint(hc["test"]), "pg_isready") {
		return fmt.Errorf("tracker-db healthcheck does not run pg_isready: %v", hc["test"])
	}
	return nil
}

// checkNoPasswordLiteral scans the uncommented lines for a password assigned
// a literal value. An interpolated ${...} value and an escaped reference
// $$NAME are allowed, because both name a variable instead of carrying one.
func checkNoPasswordLiteral(raw []byte) error {
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		m := passwordLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		value := strings.Trim(strings.TrimSpace(m[1]), `"'`)
		if value != "" && !strings.HasPrefix(value, "${") && !composeEscape.MatchString(value) {
			return fmt.Errorf("%s:%d sets a password literal; take it from %s", composeFile, i+1, passwordEnvFile)
		}
	}
	return nil
}

// headerLines returns the comment block at the top of the file, before the
// first line of YAML.
func headerLines(raw []byte) []string {
	var header []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			break
		}
		header = append(header, trimmed)
	}
	return header
}

// checkHeaderDSN requires the DSN in the header comment to bound the pool
// below 40, leaving 20 of max_connections=50 for backups, migrations and admin.
func checkHeaderDSN(raw []byte) error {
	var dsn string
	for _, line := range headerLines(raw) {
		if i := strings.Index(line, "postgres://"); i >= 0 {
			dsn = strings.Fields(line[i:])[0]
			break
		}
	}
	if dsn == "" {
		return fmt.Errorf("the header comment of %s carries no postgres:// DSN", composeFile)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("header DSN does not parse: %w", err)
	}
	v := u.Query().Get("pool_max_conns")
	if v == "" {
		return errors.New("header DSN has no pool_max_conns")
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("header DSN pool_max_conns=%q is not a number", v)
	}
	if n <= 0 || n >= 40 {
		return fmt.Errorf("header DSN pool_max_conns=%d; want 1..39 so 20 of max_connections=50 stay free", n)
	}
	return nil
}

// ensureService is the one-shot service that creates the tracker database
// when the first init did not (gt-caf0o6).
const ensureService = "tracker-db-ensure"

// ensureCommandTokens must all appear in the one-shot command: it probes for
// the database, creates it owned by tracker, and takes the password from the
// container environment rather than carrying it.
var ensureCommandTokens = []string{
	"CREATE DATABASE tracker OWNER tracker",
	"pg_database",
	"PGPASSWORD",
	"$$POSTGRES_PASSWORD",
	"-h tracker-db",
	"-U tracker",
	"-d postgres",
}

// checkEnsureService returns every way raw breaks the tracker-db-ensure
// contract, joined, or nil when the one-shot database creator holds it.
func checkEnsureService(raw []byte) error {
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s is not valid YAML: %w", composeFile, err)
	}
	svc, ok := doc.Services[ensureService]
	if !ok {
		return fmt.Errorf("%s defines no %s service", composeFile, ensureService)
	}
	return errors.Join(
		checkEnsureImage(svc, doc.Services["tracker-db"]),
		checkEnsureRestart(svc),
		checkEnsureEnvFile(svc),
		checkEnsureDependsOn(svc),
		checkEnsureCommand(svc),
		checkNoPasswordLiteral(raw),
	)
}

// checkEnsureImage requires the one-shot service to run the same pinned
// Postgres image as tracker-db, so it carries the same psql and the same
// server version the probe talks to.
func checkEnsureImage(ensure, base map[string]any) error {
	if base == nil {
		return fmt.Errorf("%s defines no tracker-db service to compare %s with", composeFile, ensureService)
	}
	baseImage, err := field(base, "image")
	if err != nil {
		return err
	}
	image, err := serviceField(ensureService, ensure, "image")
	if err != nil {
		return err
	}
	if image != baseImage {
		return fmt.Errorf("%s image=%q; want tracker-db's %q", ensureService, image, baseImage)
	}
	if !pgImage.MatchString(image) {
		return fmt.Errorf("%s image %q is not postgres 17 pinned to a minor tag or digest", ensureService, image)
	}
	return nil
}

// checkEnsureRestart requires restart "no": the service runs once per
// `docker compose up`, and a failure is left for the operator to see.
func checkEnsureRestart(svc map[string]any) error {
	restart, err := serviceField(ensureService, svc, "restart")
	if err != nil {
		return err
	}
	if restart != "no" {
		return fmt.Errorf("%s restart=%q; want no, so it runs once", ensureService, restart)
	}
	return nil
}

func checkEnsureEnvFile(svc map[string]any) error {
	envFile, err := serviceField(ensureService, svc, "env_file")
	if err != nil {
		return err
	}
	if envFile != passwordEnvFile {
		return fmt.Errorf("%s env_file=%q; want %s, where POSTGRES_PASSWORD lives", ensureService, envFile, passwordEnvFile)
	}
	return nil
}

// checkEnsureDependsOn requires the long-form dependency that waits for
// tracker-db's healthcheck, so psql runs against a server that answers.
func checkEnsureDependsOn(svc map[string]any) error {
	deps, ok := svc["depends_on"].(map[string]any)
	if !ok {
		return fmt.Errorf("%s depends_on=%v; want a mapping tracker-db: {condition: service_healthy}", ensureService, svc["depends_on"])
	}
	dep, ok := deps["tracker-db"].(map[string]any)
	if !ok {
		return fmt.Errorf("%s depends_on has no tracker-db mapping with a condition", ensureService)
	}
	if cond := fmt.Sprint(dep["condition"]); cond != "service_healthy" {
		return fmt.Errorf("%s depends_on tracker-db condition=%q; want service_healthy", ensureService, cond)
	}
	return nil
}

// commandText joins a service's entrypoint and command, in either the list or
// the string form, so one token check covers both.
func commandText(svc map[string]any) string {
	var parts []string
	for _, key := range []string{"entrypoint", "command"} {
		switch v := svc[key].(type) {
		case []any:
			for _, p := range v {
				parts = append(parts, fmt.Sprint(p))
			}
		case string:
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, "\n")
}

// unescapedDollar matches a single $ that compose reads as interpolation
// start. A literal $ in the command is written $$, which compose turns into $.
var unescapedDollar = regexp.MustCompile(`(^|[^$])\$([^$]|$)`)

// ensureProbeGuard matches the probe's psql command substitution followed by a
// status test, so a probe that cannot answer fails the service instead of
// reading as an absent database and falling into the create branch.
var ensureProbeGuard = regexp.MustCompile(`\$\(psql[^)]*\)\s*\|\|`)

func checkEnsureCommand(svc map[string]any) error {
	cmd := commandText(svc)
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("%s has no entrypoint or command", ensureService)
	}
	var problems []error
	var missing []string
	for _, token := range ensureCommandTokens {
		if !strings.Contains(cmd, token) {
			missing = append(missing, token)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, fmt.Errorf("%s command lacks %s", ensureService, strings.Join(missing, ", ")))
	}
	if unescapedDollar.MatchString(cmd) {
		problems = append(problems, fmt.Errorf("%s command carries an unescaped $; write $$ so compose leaves the shell's $ alone", ensureService))
	}
	if !ensureProbeGuard.MatchString(cmd) {
		problems = append(problems, fmt.Errorf("%s does not test the probe's exit status, so a failed probe falls into the create branch", ensureService))
	}
	return errors.Join(problems...)
}

// TestTrackerComposeLimits runs the contract against the real compose file.
func TestTrackerComposeLimits(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", composeFile, err)
	}
	if err := checkCompose(raw); err != nil {
		t.Errorf("%s breaks the tracker-db contract:\n%v", composeFile, err)
	}
}

// validComposeFixture is a compose file that satisfies every check. Each
// refusal row mutates one line of it.
const validComposeFixture = `# Local Postgres for the tracker.
#
#   postgres://tracker@127.0.0.1:5432/tracker?pool_max_conns=30
#
# In the cloud this file is replaced by a managed database.
services:
  tracker-db:
    image: postgres:17.5
    restart: unless-stopped
    env_file: tracker-db.env
    ports:
      - "127.0.0.1:5432:5432"
    volumes:
      - "${TRACKER_PG_DATA:?set TRACKER_PG_DATA to a directory under ~/gt}:/var/lib/postgresql/data"
    mem_limit: 2g
    cpus: 2
    pids_limit: 512
    command:
      - postgres
      - -c
      - max_connections=50
      - -c
      - shared_buffers=512MB
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U tracker -d tracker"]
      interval: 10s
      timeout: 5s
      retries: 5
`

// TestTrackerComposeLimitsRefusals drives each refusal branch of the contract
// on a mutated copy of the fixture. Every row must fail with its message.
func TestTrackerComposeLimitsRefusals(t *testing.T) {
	t.Parallel()
	if err := checkCompose([]byte(validComposeFixture)); err != nil {
		t.Fatalf("fixture must pass the contract before any mutation: %v", err)
	}
	tests := []struct {
		name string
		old  string
		new  string
		want string
	}{
		{"not YAML", "services:\n", "services: [\n", "is not valid YAML"},
		{"no tracker-db service", "  tracker-db:\n", "  tracker-dbx:\n", "defines no tracker-db service"},
		{"image unpinned", "postgres:17.5", "postgres:latest", "is not postgres 17 pinned"},
		{"image major only", "postgres:17.5", "postgres:17", "is not postgres 17 pinned"},
		{"restart missing", "    restart: unless-stopped\n", "", "has no restart"},
		{"restart wrong", "unless-stopped", "always", "want unless-stopped"},
		{"env_file missing", "    env_file: tracker-db.env\n", "", "has no env_file"},
		{"env_file other", "env_file: tracker-db.env", "env_file: other.env", "want tracker-db.env"},
		{"mem_limit missing", "    mem_limit: 2g\n", "", "has no mem_limit"},
		{"mem_limit without unit", "mem_limit: 2g", "mem_limit: 2", "mem_limit=\"2\""},
		{"cpus missing", "    cpus: 2\n", "", "has no cpus"},
		{"cpus zero", "cpus: 2", "cpus: 0", "cpus=\"0\""},
		{"cpus not a number", "cpus: 2", "cpus: many", "cpus=\"many\""},
		{"pids_limit not a number", "pids_limit: 512", "pids_limit: lots", "pids_limit=\"lots\""},
		{"pids_limit missing", "    pids_limit: 512\n", "", "has no pids_limit"},
		{"pids_limit zero", "pids_limit: 512", "pids_limit: 0", "pids_limit=\"0\""},
		{"max_connections flag dropped", "max_connections=50", "work_mem=4MB", "no -c max_connections=N flag above 0"},
		{"max_connections zero", "max_connections=50", "max_connections=0", "no -c max_connections=N flag above 0"},
		{"port not loopback", `"127.0.0.1:5432:5432"`, `"5432:5432"`, "is not bound to 127.0.0.1"},
		{"no port published", "    ports:\n      - \"127.0.0.1:5432:5432\"\n", "    expose:\n      - \"5432\"\n", "publishes no port"},
		{"data path has default", "${TRACKER_PG_DATA:?set TRACKER_PG_DATA to a directory under ~/gt}", "${TRACKER_PG_DATA:-/tmp/pg}", "does not take its path from TRACKER_PG_DATA"},
		{"data volume missing", ":/var/lib/postgresql/data", ":/var/lib/other", "mounts no data volume"},
		{"healthcheck missing", "    healthcheck:\n", "    healthchek:\n", "has no healthcheck"},
		{"healthcheck not pg_isready", "pg_isready -U tracker -d tracker", "true", "does not run pg_isready"},
		{"password literal mapping", "    env_file: tracker-db.env\n", "    environment:\n      POSTGRES_PASSWORD: hunter2\n    env_file: tracker-db.env\n", "sets a password literal"},
		{"password literal list quoted", "    env_file: tracker-db.env\n", "    environment:\n      - POSTGRES_PASSWORD=\"hunter2\"\n    env_file: tracker-db.env\n", "sets a password literal"},
		{"DSN missing", "postgres://tracker@127.0.0.1:5432/tracker?pool_max_conns=30", "tracker DSN elsewhere", "carries no postgres:// DSN"},
		{"DSN lacks pool_max_conns", "?pool_max_conns=30", "", "has no pool_max_conns"},
		{"DSN does not parse", "postgres://tracker@127.0.0.1", "postgres://tracker@[127.0.0.1", "header DSN does not parse"},
		{"DSN pool_max_conns not a number", "pool_max_conns=30", "pool_max_conns=many", "is not a number"},
		{"DSN pool_max_conns zero", "pool_max_conns=30", "pool_max_conns=0", "pool_max_conns=0; want 1..39"},
		{"DSN pool_max_conns at 40", "pool_max_conns=30", "pool_max_conns=40", "pool_max_conns=40; want 1..39"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if n := strings.Count(validComposeFixture, tt.old); n != 1 {
				t.Fatalf("fixture has %d copies of %q; the row must mutate exactly one", n, tt.old)
			}
			mutated := strings.Replace(validComposeFixture, tt.old, tt.new, 1)
			err := checkCompose([]byte(mutated))
			if err == nil {
				t.Fatalf("checkCompose accepted the mutation; want an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("checkCompose error = %q; want it to contain %q", err, tt.want)
			}
		})
	}
}

// composeDoc is the compose file's services, for a test that mutates one
// service and re-checks the document.
type composeDoc struct {
	Services map[string]map[string]any `yaml:"services"`
}

// ensureFixture returns the real compose file with one mutation applied to its
// parsed form, so a refusal row changes the document and not a line of text.
func ensureFixture(t *testing.T, mutate func(doc *composeDoc)) []byte {
	t.Helper()
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", composeFile, err)
	}
	var doc composeDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", composeFile, err)
	}
	mutate(&doc)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatalf("marshalling the mutated %s: %v", composeFile, err)
	}
	return out
}

// service returns a service from the parsed document, failing the row when the
// service the mutation assumes is gone.
func (d *composeDoc) service(t *testing.T, name string) map[string]any {
	t.Helper()
	svc, ok := d.Services[name]
	if !ok {
		t.Fatalf("%s has no %s service to mutate", composeFile, name)
	}
	return svc
}

// rewrite returns script with every old replaced by new, failing the row when
// the token it replaces is absent.
func rewrite(t *testing.T, script, old, new string) string {
	t.Helper()
	if !strings.Contains(script, old) {
		t.Fatalf("the %s command carries no %q to rewrite", ensureService, old)
	}
	return strings.ReplaceAll(script, old, new)
}

// setEnsureScript replaces the one-shot command's script in a parsed service.
func setEnsureScript(t *testing.T, svc map[string]any, script string) {
	t.Helper()
	items, ok := svc["entrypoint"].([]any)
	if !ok || len(items) < 3 {
		t.Fatalf("%s entrypoint is not the sh -c list", ensureService)
	}
	items[2] = script
}

// ensureScript returns the one-shot command's sh -c script.
func ensureScript(t *testing.T, svc map[string]any) string {
	t.Helper()
	items, ok := svc["entrypoint"].([]any)
	if !ok || len(items) < 3 {
		t.Fatalf("%s entrypoint is not the sh -c list", ensureService)
	}
	return fmt.Sprint(items[2])
}

// TestTrackerComposeEnsureService runs the one-shot database creator's
// contract against the real compose file.
func TestTrackerComposeEnsureService(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", composeFile, err)
	}
	if err := checkEnsureService(raw); err != nil {
		t.Errorf("%s breaks the %s contract:\n%v", composeFile, ensureService, err)
	}
}

// TestTrackerComposeEnsureServiceRefusals drives each refusal branch on a
// mutated copy of the real compose file. Every row must fail with its message.
func TestTrackerComposeEnsureServiceRefusals(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", composeFile, err)
	}
	if err := checkEnsureService(raw); err != nil {
		t.Fatalf("%s must hold the %s contract before any mutation: %v", composeFile, ensureService, err)
	}
	tests := []struct {
		name string
		raw  func(t *testing.T) []byte
		want string
	}{
		{"not YAML", func(*testing.T) []byte { return []byte("services: [\n") }, "is not valid YAML"},
		{"service missing", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) { delete(d.Services, ensureService) })
		}, "defines no tracker-db-ensure service"},
		{"tracker-db missing", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) { delete(d.Services, "tracker-db") })
		}, "defines no tracker-db service to compare"},
		{"tracker-db image missing", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) { delete(d.service(t, "tracker-db"), "image") })
		}, "tracker-db has no image"},
		{"ensure image missing", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) { delete(d.service(t, ensureService), "image") })
		}, "tracker-db-ensure has no image"},
		{"image differs", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) { d.service(t, ensureService)["image"] = "postgres:17.4" })
		}, `image="postgres:17.4"; want tracker-db's`},
		{"image unpinned", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) {
				d.service(t, "tracker-db")["image"] = "postgres:latest"
				d.service(t, ensureService)["image"] = "postgres:latest"
			})
		}, "is not postgres 17 pinned"},
		{"restart missing", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) { delete(d.service(t, ensureService), "restart") })
		}, "has no restart"},
		{"restart wrong", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) { d.service(t, ensureService)["restart"] = "always" })
		}, `restart="always"`},
		{"env_file missing", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) { delete(d.service(t, ensureService), "env_file") })
		}, "has no env_file"},
		{"env_file other", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) { d.service(t, ensureService)["env_file"] = "other.env" })
		}, "want tracker-db.env"},
		{"depends_on missing", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) { delete(d.service(t, ensureService), "depends_on") })
		}, "want a mapping tracker-db: {condition: service_healthy}"},
		{"depends_on short form", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) {
				d.service(t, ensureService)["depends_on"] = []any{"tracker-db"}
			})
		}, "want a mapping tracker-db: {condition: service_healthy}"},
		{"depends_on other service", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) {
				d.service(t, ensureService)["depends_on"] = map[string]any{"tracker-db2": map[string]any{"condition": "service_healthy"}}
			})
		}, "has no tracker-db mapping with a condition"},
		{"condition not healthy", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) {
				d.service(t, ensureService)["depends_on"] = map[string]any{"tracker-db": map[string]any{"condition": "service_started"}}
			})
		}, `condition="service_started"`},
		{"no command", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) { delete(d.service(t, ensureService), "entrypoint") })
		}, "has no entrypoint or command"},
		{"literal password", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) {
				svc := d.service(t, ensureService)
				setEnsureScript(t, svc, rewrite(t, ensureScript(t, svc), "$$POSTGRES_PASSWORD", "hunter2"))
			})
		}, "sets a password literal"},
		{"unescaped dollar", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) {
				svc := d.service(t, ensureService)
				setEnsureScript(t, svc, rewrite(t, ensureScript(t, svc), "$$POSTGRES_PASSWORD", "$POSTGRES_PASSWORD"))
			})
		}, "unescaped $"},
		{"probe failure folded into create", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) {
				svc := d.service(t, ensureService)
				setEnsureScript(t, svc, rewrite(t, ensureScript(t, svc), `'tracker'") || exit 1`, `'tracker'")`))
			})
		}, "does not test the probe's exit status"},
		{"no pg_database probe", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) {
				svc := d.service(t, ensureService)
				setEnsureScript(t, svc, rewrite(t, ensureScript(t, svc), "pg_database", "pg_class"))
			})
		}, "pg_database"},
		{"no create", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) {
				svc := d.service(t, ensureService)
				setEnsureScript(t, svc, rewrite(t, ensureScript(t, svc), "CREATE DATABASE tracker OWNER tracker", "SELECT 1"))
			})
		}, "CREATE DATABASE tracker OWNER tracker"},
		{"no PGPASSWORD", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) {
				svc := d.service(t, ensureService)
				setEnsureScript(t, svc, rewrite(t, ensureScript(t, svc), "PGPASSWORD", "PASS"))
			})
		}, "PGPASSWORD"},
		{"other host", func(t *testing.T) []byte {
			return ensureFixture(t, func(d *composeDoc) {
				svc := d.service(t, ensureService)
				setEnsureScript(t, svc, rewrite(t, ensureScript(t, svc), "-h tracker-db", "-h db"))
			})
		}, "-h tracker-db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkEnsureService(tt.raw(t))
			if err == nil {
				t.Fatalf("checkEnsureService accepted the mutation; want an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("checkEnsureService error = %q; want it to contain %q", err, tt.want)
			}
		})
	}
}
