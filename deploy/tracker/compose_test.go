package tracker

import (
	"bytes"
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

// passwordLine finds a password assignment in a compose line, as a mapping
// (POSTGRES_PASSWORD: x) or as a list entry (- POSTGRES_PASSWORD=x).
var passwordLine = regexp.MustCompile(`(?i)password[a-z_]*\s*[:=]\s*(.*)$`)

// maxConnectionsFlag matches the server flag value in a command list.
var maxConnectionsFlag = regexp.MustCompile(`^max_connections=(\d+)$`)

// readCompose returns the compose file and its decoded tracker-db service. A
// file that cannot be read or parsed, or a missing service, fails the test.
func readCompose(t *testing.T) ([]byte, map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", composeFile, err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(raw)).Decode(&doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", composeFile, err)
	}
	svc, ok := doc.Services["tracker-db"]
	if !ok {
		t.Fatalf("%s defines no tracker-db service", composeFile)
	}
	return raw, svc
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

func TestTrackerComposeLimits(t *testing.T) {
	t.Parallel()
	raw, svc := readCompose(t)

	t.Run("resource limits", func(t *testing.T) {
		t.Parallel()
		for _, key := range []string{"mem_limit", "cpus", "pids_limit"} {
			if _, ok := svc[key]; !ok {
				t.Errorf("tracker-db has no %s", key)
			}
		}
	})

	t.Run("max_connections flag", func(t *testing.T) {
		t.Parallel()
		cmd, _ := svc["command"].([]any)
		for i := 0; i+1 < len(cmd); i++ {
			if fmt.Sprint(cmd[i]) != "-c" {
				continue
			}
			if maxConnectionsFlag.MatchString(fmt.Sprint(cmd[i+1])) {
				return
			}
		}
		t.Errorf("tracker-db command has no -c max_connections=N flag: %v", cmd)
	})

	t.Run("loopback port", func(t *testing.T) {
		t.Parallel()
		ports, _ := svc["ports"].([]any)
		if len(ports) == 0 {
			t.Fatalf("tracker-db publishes no port")
		}
		for _, p := range ports {
			if !strings.HasPrefix(fmt.Sprint(p), "127.0.0.1:") {
				t.Errorf("tracker-db port %v is not bound to 127.0.0.1", p)
			}
		}
	})

	t.Run("no password literal", func(t *testing.T) {
		t.Parallel()
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
				t.Errorf("%s:%d sets a password literal; take it from tracker-db.env", composeFile, i+1)
			}
		}
	})

	t.Run("header DSN pool bound", func(t *testing.T) {
		t.Parallel()
		var dsn string
		for _, line := range headerLines(raw) {
			if i := strings.Index(line, "postgres://"); i >= 0 {
				dsn = strings.Fields(line[i:])[0]
				break
			}
		}
		if dsn == "" {
			t.Fatalf("the header comment of %s carries no postgres:// DSN", composeFile)
		}
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("header DSN does not parse: %v", err)
		}
		v := u.Query().Get("pool_max_conns")
		if v == "" {
			t.Fatalf("header DSN has no pool_max_conns")
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("header DSN pool_max_conns=%q is not a number", v)
		}
		if n <= 0 || n >= 40 {
			t.Errorf("header DSN pool_max_conns=%d; want 1..39 so 20 of max_connections=50 stay free", n)
		}
	})
}
