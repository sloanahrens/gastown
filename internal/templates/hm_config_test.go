package templates_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// hmHostConfig is the Mac's hm config as the dry run read it: the database URL
// on the Mac's loopback, and the embed and rerank services already reachable by
// the container's name for the Mac.
const hmHostConfig = `{
  "database_url": "postgres://hm:s3cret-pa55@localhost:5433/hm?sslmode=disable",
  "embed_url": "http://host.docker.internal:8080/embed",
  "rerank_url": "http://host.docker.internal:8080/rerank",
  "top_k": 6
}`

// decodeHMConfig is a derived config's fields, as hm reads them.
func decodeHMConfig(t *testing.T, content []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(content, &got); err != nil {
		t.Fatalf("the derived hm config does not parse: %v\n%s", err, content)
	}
	return got
}

// The database URL's loopback host becomes the name a container reaches the Mac
// by, with its port, user, password and database kept; a remote database is
// copied unchanged, as is every other field. A config the derivation cannot read
// is refused naming the field, and the refusals never repeat the URL.
func TestHMContainerConfigRewritesTheLocalHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		config     string
		wantURL    string
		wantReason string
	}{
		{
			name:    "localhost",
			config:  `{"database_url": "postgres://hm:s3cret@localhost:5433/hm?sslmode=disable"}`,
			wantURL: "postgres://hm:s3cret@host.docker.internal:5433/hm?sslmode=disable",
		},
		{
			name:    "127.0.0.1",
			config:  `{"database_url": "postgres://127.0.0.1:5433/hm"}`,
			wantURL: "postgres://host.docker.internal:5433/hm",
		},
		{
			name:    "a loopback host with no port",
			config:  `{"database_url": "postgres://hm@localhost/hm"}`,
			wantURL: "postgres://hm@host.docker.internal/hm",
		},
		{
			name:    "a remote database is unchanged",
			config:  `{"database_url": "postgres://hm:s3cret@db.example.test:5433/hm?sslmode=require"}`,
			wantURL: "postgres://hm:s3cret@db.example.test:5433/hm?sslmode=require",
		},
		{name: "no database_url", config: `{"embed_url": "http://host.docker.internal:8080/embed"}`,
			wantReason: "database_url is missing"},
		{name: "database_url is not a string", config: `{"database_url": 5433}`,
			wantReason: "database_url is not a string"},
		{name: "the file is not JSON", config: `{"database_url":`,
			wantReason: "does not parse as a JSON object"},
		// A host with a space cannot be a URL; the message names the field and
		// never the URL, whose password would otherwise reach a log.
		{name: "database_url does not parse", config: `{"database_url": "postgres://hm:s3 cret@localhost:5433/hm"}`,
			wantReason: "database_url (URL form) does not parse"},
		{name: "a field name holds a control character", config: `{"database_url": "postgres://hm@host.docker.internal:5433/hm", "\u0001x": 1}`,
			wantReason: "field name holds a control character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := templates.RenderHMConfig([]byte(tc.config))
			if tc.wantReason != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantReason) {
					t.Fatalf("RenderHMConfig error = %v, want it to contain %q", err, tc.wantReason)
				}
				if strings.Contains(err.Error(), "s3 cret") || strings.Contains(err.Error(), "s3cret") {
					t.Errorf("the error repeats the database password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("RenderHMConfig: %v", err)
			}
			got := decodeHMConfig(t, out)
			if got["database_url"] != tc.wantURL {
				t.Errorf("database_url = %q, want %q", got["database_url"], tc.wantURL)
			}
			// Every other field is copied unchanged.
			var host map[string]any
			if err := json.Unmarshal([]byte(tc.config), &host); err != nil {
				t.Fatalf("the fixture does not parse: %v", err)
			}
			for field, want := range host {
				if field == "database_url" {
					continue
				}
				if jsonValue(got[field]) != jsonValue(want) {
					t.Errorf("%s = %v, want it copied unchanged as %v", field, got[field], want)
				}
			}
		})
	}
}

// jsonValue renders a decoded JSON value so a field's copy can be compared
// regardless of how the two sides spell a number or a nest.
func jsonValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// hmConfigVolume is the derived hm config's mount on the daemon service.
func hmConfigVolume(vols []composeVolume) (composeVolume, bool) {
	for _, v := range vols {
		if v.Target == templates.LiveHMConfigPath {
			return v, true
		}
	}
	return composeVolume{}, false
}

// The derived file is written owner-only, replacing a wider mode a file already
// had; the render mounts it read-only where HM_CONFIG names it, and neither the
// compose file nor the derived file's path carries the URL or its password.
func TestHMContainerConfigIsPrivateAndNeverPrinted(t *testing.T) {
	t.Parallel()
	derived, err := templates.RenderHMConfig([]byte(hmHostConfig))
	if err != nil {
		t.Fatalf("RenderHMConfig: %v", err)
	}
	path := filepath.Join(t.TempDir(), "hm-config.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("writing the file the derived one replaces: %v", err)
	}
	if err := templates.WriteHMConfigFile(path, derived); err != nil {
		t.Fatalf("WriteHMConfigFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the derived hm config: %v", err)
	}
	if want := os.FileMode(0o600); info.Mode().Perm() != want {
		t.Errorf("the derived hm config's mode = %o, want %o", info.Mode().Perm(), want)
	}

	spec := liveSpec()
	spec.HMConfigFile = path
	out, f := render(t, spec)
	daemon := f.Services["daemon"]
	mount, ok := hmConfigVolume(daemon.Volumes)
	if !ok {
		t.Fatalf("daemon volumes = %+v, want the derived hm config read-only at %s", daemon.Volumes, templates.LiveHMConfigPath)
	}
	if want := (composeVolume{Type: "bind", Source: path, Target: templates.LiveHMConfigPath, ReadOnly: true}); mount != want {
		t.Errorf("hm config mount = %+v, want %+v", mount, want)
	}
	if got := daemon.Environment["HM_CONFIG"]; got != templates.LiveHMConfigPath {
		t.Errorf("HM_CONFIG = %q, want the mounted path %s", got, templates.LiveHMConfigPath)
	}
	for _, secret := range []string{"s3cret-pa55", "localhost:5433"} {
		if strings.Contains(out, secret) {
			t.Errorf("the rendered file carries %q:\n%s", secret, out)
		}
	}
	// The derived file itself carries the rewritten URL, and that is the only
	// place the database address lives.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the derived hm config: %v", err)
	}
	if !bytes.Equal(onDisk, derived) {
		t.Errorf("the written file = %q, want the derived config %q", onDisk, derived)
	}
}

// The render refuses an hm config it cannot name or mount: a relative path, a
// scratch spec, and a daemon.env HM_CONFIG naming a path no mount provides.
func TestHMContainerConfigSpecRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*templates.ComposeSpec)
		want   string
	}{
		{name: "a relative path", want: "not an absolute path", mutate: func(s *templates.ComposeSpec) {
			s.HMConfigFile = ".runtime/container/hm-config.json"
		}},
		{name: "a control character", want: "control character", mutate: func(s *templates.ComposeSpec) {
			s.HMConfigFile = "/towns/live/.runtime/container/hm\x00-config.json"
		}},
		{name: "a scratch spec", want: "scratch spec", mutate: func(s *templates.ComposeSpec) {
			*s = scratchRefusalSpec()
			s.HMConfigFile = "/towns/live/.runtime/container/hm-config.json"
		}},
		{name: "HM_CONFIG names another path", want: "Env names HM_CONFIG", mutate: func(s *templates.ComposeSpec) {
			s.HMConfigFile = "/towns/live/.runtime/container/hm-config.json"
			s.Env = map[string]string{"HM_CONFIG": "/somewhere/else.json"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := liveSpec()
			tc.mutate(&spec)
			if out, err := templates.RenderDaemonCompose(spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RenderDaemonCompose error = %v, want one containing %q\n%s", err, tc.want, out)
			}
		})
	}
}

// scratchRefusalSpec is the smallest scratch spec the render accepts before the
// field under test is set.
func scratchRefusalSpec() templates.ComposeSpec {
	return templates.ComposeSpec{
		Project:          "gt-scratch",
		ContainerName:    "gt-scratch-daemon-1",
		Image:            "gt-daemon:4f2a1c9",
		TownRoot:         "/towns/live",
		StopGrace:        templates.MinimumStopGrace,
		Scratch:          true,
		LiveTownRoot:     "/towns/live",
		ScratchDoltImage: "dolthub/dolt-sql-server:1.43.0",
	}
}

// The render's HM_CONFIG line is the one the daemon.env entry would repeat: an
// entry naming the mounted path is not written twice.
func TestHMContainerConfigAnswersOneHMConfigLine(t *testing.T) {
	t.Parallel()
	spec := liveSpec()
	spec.HMConfigFile = "/towns/live/.runtime/container/hm-config.json"
	spec.Env = map[string]string{"HM_CONFIG": templates.LiveHMConfigPath}
	out, _ := render(t, spec)
	if got := strings.Count(out, "HM_CONFIG:"); got != 1 {
		t.Errorf("the rendered file carries HM_CONFIG %d times, want once:\n%s", got, out)
	}
}

// TestHMContainerConfigRewritesBothDSNForms: the Postgres driver accepts a URL
// and a key=value string, and a loopback host in either one is the container
// itself. Each form is parsed and its host rewritten; the rest of the string is
// kept byte for byte; a form that cannot be parsed or rewritten is refused with
// the form named and never repeats the password.
func TestHMContainerConfigRewritesBothDSNForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		dsn        string
		want       string
		wantReason string
	}{
		{name: "key=value, bare",
			dsn:  "host=localhost port=5433 user=hm password=s3cret dbname=hm sslmode=disable",
			want: "host=host.docker.internal port=5433 user=hm password=s3cret dbname=hm sslmode=disable"},
		{name: "key=value, host last and 127.0.0.1",
			dsn:  "port=5433 user=hm dbname=hm host=127.0.0.1",
			want: "port=5433 user=hm dbname=hm host=host.docker.internal"},
		{name: "key=value, spaces around the equals sign",
			dsn:  "host = localhost port = 5433 user = hm",
			want: "host = host.docker.internal port = 5433 user = hm"},
		{name: "key=value, quoted values keep their quoting",
			dsn:  "host='localhost' port=5433 password='p w\\'x' dbname=hm",
			want: "host='host.docker.internal' port=5433 password='p w\\'x' dbname=hm"},
		{name: "key=value, a host list rewrites each loopback element",
			dsn:  "host=localhost,db.example.test port=5433,5432 dbname=hm",
			want: "host=host.docker.internal,db.example.test port=5433,5432 dbname=hm"},
		{name: "key=value, IPv6 loopback",
			dsn:  "host=::1 port=5433 dbname=hm",
			want: "host=host.docker.internal port=5433 dbname=hm"},
		{name: "key=value, a remote host is unchanged",
			dsn:  "host=db.example.test port=5433 password=s3cret dbname=hm",
			want: "host=db.example.test port=5433 password=s3cret dbname=hm"},
		{name: "URL, IPv6 loopback in brackets",
			dsn:  "postgres://hm:s3cret@[::1]:5433/hm",
			want: "postgres://hm:s3cret@host.docker.internal:5433/hm"},
		{name: "URL, 127.0.0.2 is loopback too",
			dsn:  "postgresql://hm@127.0.0.2:5433/hm",
			want: "postgresql://hm@host.docker.internal:5433/hm"},
		{name: "URL, the host query parameter overrides the authority",
			dsn:  "postgres://hm@/hm?host=localhost&port=5433",
			want: "postgres://hm@/hm?host=host.docker.internal&port=5433"},

		{name: "key=value, no host defaults to the local machine",
			dsn: "port=5433 user=hm password=s3cret dbname=hm", wantReason: "(key=value form) names no host"},
		{name: "key=value, a unix-socket host",
			dsn: "host=/var/run/postgresql user=hm password=s3cret", wantReason: "(key=value form) names a unix-socket host"},
		{name: "key=value, a loopback hostaddr would still point at the container",
			dsn: "host=db.example.test hostaddr=127.0.0.1 password=s3cret", wantReason: "(key=value form) has a loopback hostaddr"},
		{name: "key=value, a key with no equals sign",
			dsn: "host=localhost s3cret", wantReason: "(key=value form) has a key with no"},
		{name: "key=value, an unterminated quote",
			dsn: "host=localhost password='s3cret", wantReason: "(key=value form) has an unterminated quoted value"},
		{name: "URL, several hosts in the authority",
			dsn: "postgres://hm:s3cret@localhost:5433,db.example.test:5432/hm", wantReason: "(URL form)"},
		{name: "URL, a unix-socket host in the query",
			dsn: "postgres://hm:s3cret@/hm?host=/var/run/postgresql", wantReason: "(URL form) names a unix-socket host"},
		{name: "a scheme that is not postgres",
			dsn: "mysql://hm:s3cret@localhost:3306/hm", wantReason: "(URL form) has a scheme other than postgres"},
		{name: "URL, a loopback hostaddr would still point at the container",
			dsn: "postgres://hm:s3cret@db.example.test:5433/hm?hostaddr=127.0.0.1", wantReason: "(URL form) has a loopback hostaddr"},
		{name: "key=value, a pair with no key",
			dsn: "=s3cret host=localhost", wantReason: "(key=value form) has a pair with no key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := json.Marshal(map[string]string{"database_url": tc.dsn, "town_root": "/town"})
			if err != nil {
				t.Fatal(err)
			}
			out, err := templates.RenderHMConfig(cfg)
			if tc.wantReason != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantReason) {
					t.Fatalf("RenderHMConfig error = %v, want it to contain %q", err, tc.wantReason)
				}
				if strings.Contains(err.Error(), "s3cret") {
					t.Errorf("the error repeats the database password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("RenderHMConfig: %v", err)
			}
			if got := decodeHMConfig(t, out)["database_url"]; got != tc.want {
				t.Errorf("database_url = %q, want %q", got, tc.want)
			}
		})
	}
}
