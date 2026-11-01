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

// field returns a service key as text. A missing key is an error, so a
// refusal names the key it lacks.
func field(svc map[string]any, key string) (string, error) {
	v, ok := svc[key]
	if !ok {
		return "", fmt.Errorf("tracker-db has no %s", key)
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
// a literal value. An interpolated ${...} value is allowed.
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
		if value != "" && !strings.HasPrefix(value, "${") {
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
