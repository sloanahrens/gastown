package doltserver

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// validContainerSpec is a spec every field of which ContainerPlan accepts.
func validContainerSpec() ContainerSpec {
	return ContainerSpec{
		Name:          "gt-dolt",
		Image:         "dolt:test",
		DataDir:       "/town/.dolt-data",
		BackupDir:     "/home/u/gt-backups/dolt",
		ConfigPath:    "/town/.dolt-data/config.yaml",
		MemoryGiB:     8,
		HostPort:      3307,
		ContainerPort: 3306,
		Restart:       "unless-stopped",
	}
}

func TestContainerPlanArgv(t *testing.T) {
	t.Parallel()

	got, err := ContainerPlan(validContainerSpec())
	if err != nil {
		t.Fatalf("ContainerPlan: %v", err)
	}
	want := []string{
		"docker", "create",
		"--name", "gt-dolt",
		"--memory", "8g",
		"--memory-swap", "8g",
		"--restart", "unless-stopped",
		"-p", "127.0.0.1:3307:3306",
		"-v", "/town/.dolt-data:/town/.dolt-data",
		"-v", "/home/u/gt-backups/dolt:/home/u/gt-backups/dolt",
		"-v", "/town/.dolt-data/config.yaml:/town/.dolt-data/config.yaml:ro",
		"--label", "org.gastown.keep=true",
		"dolt:test",
		"--config", "/town/.dolt-data/config.yaml",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %#v\nwant %#v", got, want)
	}
}

func TestContainerPlanKeepLabelBeforeImage(t *testing.T) {
	t.Parallel()

	// The label's place is found from the image reference, not from how many
	// elements follow it, so each spec below puts the image at a different
	// index and the label must sit directly before it in every case.
	specs := map[string]func(*ContainerSpec){
		"default":          func(*ContainerSpec) {},
		"longer data path": func(s *ContainerSpec) { s.DataDir = "/a/b/c/d/e/.dolt-data"; s.ConfigPath = s.DataDir + "/config.yaml" },
		"other image":      func(s *ContainerSpec) { s.Image = "registry.example/dolt:1.2.3" },
	}
	for name, set := range specs {
		set := set
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := validContainerSpec()
			set(&s)
			argv, err := ContainerPlan(s)
			if err != nil {
				t.Fatalf("ContainerPlan: %v", err)
			}
			imageAt := slices.Index(argv, s.Image)
			if imageAt < 2 {
				t.Fatalf("argv %#v holds no image reference %q after a flag pair", argv, s.Image)
			}
			if argv[imageAt-2] != "--label" || argv[imageAt-1] != "org.gastown.keep=true" {
				t.Fatalf("the two elements before the image are %q %q, want --label org.gastown.keep=true\n%#v",
					argv[imageAt-2], argv[imageAt-1], argv)
			}
			labels := 0
			for _, a := range argv {
				if a == "org.gastown.keep=true" {
					labels++
				}
			}
			if labels != 1 {
				t.Fatalf("the keep label appears %d times, want once", labels)
			}
		})
	}
}

func TestContainerPlanLoopbackAndSamePathMounts(t *testing.T) {
	t.Parallel()

	for _, base := range []string{"/town/.dolt-data", "/town dir/.dolt-data", "/a-b/c_d"} {
		base := base
		t.Run(base, func(t *testing.T) {
			t.Parallel()
			s := validContainerSpec()
			s.DataDir = base
			s.BackupDir = base + "/backups"
			s.ConfigPath = base + "/config.yaml"
			argv, err := ContainerPlan(s)
			if err != nil {
				t.Fatalf("ContainerPlan: %v", err)
			}
			for i, a := range argv {
				switch a {
				case "-p":
					if got := argv[i+1]; !strings.HasPrefix(got, "127.0.0.1:") {
						t.Errorf("published port %q is not on 127.0.0.1", got)
					}
				case "-v":
					mount := argv[i+1]
					path, rest, ok := strings.Cut(mount, ":")
					if !ok {
						t.Fatalf("mount %q has no inside path", mount)
					}
					inside, opts, _ := strings.Cut(rest, ":")
					if inside != path {
						t.Errorf("mount %q mounts %q inside, want the same path", mount, inside)
					}
					if opts != "" && opts != "ro" {
						t.Errorf("mount %q has options %q, want none or ro", mount, opts)
					}
				}
			}
		})
	}
}

func TestContainerPlanRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		set  func(*ContainerSpec)
	}{
		{name: "empty name", set: func(s *ContainerSpec) { s.Name = "" }},
		{name: "name with a leading dash", set: func(s *ContainerSpec) { s.Name = "-bad" }},
		{name: "name with a space", set: func(s *ContainerSpec) { s.Name = "bad name" }},
		{name: "name with a slash", set: func(s *ContainerSpec) { s.Name = "gt/dolt" }},
		{name: "empty image", set: func(s *ContainerSpec) { s.Image = "" }},
		{name: "empty config path", set: func(s *ContainerSpec) { s.ConfigPath = "" }},
		{name: "empty data directory", set: func(s *ContainerSpec) { s.DataDir = "" }},
		{name: "empty backup directory", set: func(s *ContainerSpec) { s.BackupDir = "" }},
		{name: "relative data directory", set: func(s *ContainerSpec) { s.DataDir = "data/.dolt-data" }},
		{name: "relative backup directory", set: func(s *ContainerSpec) { s.BackupDir = "backups" }},
		{name: "relative config path", set: func(s *ContainerSpec) { s.ConfigPath = "config.yaml" }},
		{name: "host port 0", set: func(s *ContainerSpec) { s.HostPort = 0 }},
		{name: "host port 70000", set: func(s *ContainerSpec) { s.HostPort = 70000 }},
		{name: "container port 0", set: func(s *ContainerSpec) { s.ContainerPort = 0 }},
		{name: "container port 70000", set: func(s *ContainerSpec) { s.ContainerPort = 70000 }},
		{name: "memory 0", set: func(s *ContainerSpec) { s.MemoryGiB = 0 }},
		{name: "memory below 0", set: func(s *ContainerSpec) { s.MemoryGiB = -1 }},
		{name: "restart sometimes", set: func(s *ContainerSpec) { s.Restart = "sometimes" }},
		{name: "restart empty", set: func(s *ContainerSpec) { s.Restart = "" }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := validContainerSpec()
			tt.set(&s)
			argv, err := ContainerPlan(s)
			if err == nil {
				t.Fatalf("ContainerPlan accepted the spec: %#v", argv)
			}
		})
	}
}

func TestContainerPlanRefusesFlagLikeImageAndUncleanPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		set  func(*ContainerSpec)
	}{
		{name: "image starting with a dash", set: func(s *ContainerSpec) { s.Image = "-dolt:test" }},
		{name: "data directory is root", set: func(s *ContainerSpec) { s.DataDir = "/" }},
		{name: "backup directory is root", set: func(s *ContainerSpec) { s.BackupDir = "/" }},
		{name: "config path is root", set: func(s *ContainerSpec) { s.ConfigPath = "/" }},
		{name: "data directory with ..", set: func(s *ContainerSpec) { s.DataDir = "/town/../data" }},
		{name: "data directory with a trailing slash", set: func(s *ContainerSpec) { s.DataDir = "/town/.dolt-data/" }},
		{name: "backup directory with a doubled slash", set: func(s *ContainerSpec) { s.BackupDir = "/home//backups" }},
		{name: "config path with a doubled slash", set: func(s *ContainerSpec) { s.ConfigPath = "/town//config.yaml" }},
		{name: "data directory with a colon", set: func(s *ContainerSpec) { s.DataDir = "/town:data" }},
		{name: "backup directory is the docker socket", set: func(s *ContainerSpec) { s.BackupDir = "/var/run/docker.sock" }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := validContainerSpec()
			tt.set(&s)
			argv, err := ContainerPlan(s)
			if err == nil {
				t.Fatalf("ContainerPlan accepted the spec: %#v", argv)
			}
		})
	}
}

func TestContainerPlanNeverEmitsAPrivilegedOption(t *testing.T) {
	t.Parallel()

	specs := []ContainerSpec{validContainerSpec()}
	for _, name := range []string{"gt-dolt", "gt-dolt-rehearsal"} {
		for _, ports := range [][2]int{{3307, 3307}, {3317, 3307}, {13306, 3306}} {
			s := validContainerSpec()
			s.Name = name
			s.HostPort, s.ContainerPort = ports[0], ports[1]
			specs = append(specs, s)
		}
	}
	for _, s := range specs {
		argv, err := ContainerPlan(s)
		if err != nil {
			t.Fatalf("ContainerPlan(%q): %v", s.Name, err)
		}
		for i, a := range argv {
			if a == "--privileged" || a == "--network" {
				t.Errorf("%q: argv holds %q", s.Name, a)
			}
			if strings.Contains(a, "docker.sock") {
				t.Errorf("%q: argv holds the Docker socket: %q", s.Name, a)
			}
			if a == "-p" && !strings.HasPrefix(argv[i+1], "127.0.0.1:") {
				t.Errorf("%q: published port %q is not on 127.0.0.1", s.Name, argv[i+1])
			}
		}
	}
}

func TestContainerPortFromConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    int
		wantErr bool
	}{
		{name: "listener port", content: "listener:\n  port: 3306\n", want: 3306},
		{
			name:    "the managed config layout",
			content: "log_level: warning\n\nlistener:\n  host: 127.0.0.1\n  port: 5507\n  max_connections: 100\n\ndata_dir: \"/town/.dolt-data\"\n",
			want:    5507,
		},
		{name: "no listener block", content: "log_level: warning\n", wantErr: true},
		{name: "listener without a port", content: "listener:\n  max_connections: 100\n", wantErr: true},
		{name: "port zero", content: "listener:\n  port: 0\n", wantErr: true},
		{name: "port above the range", content: "listener:\n  port: 70000\n", wantErr: true},
		{name: "port not a number", content: "listener:\n  port: tcp\n", wantErr: true},
		{name: "unparsable file", content: "listener: [\n", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
				t.Fatalf("write the config: %v", err)
			}
			got, err := ContainerPortFromConfig(os.ReadFile, path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ContainerPortFromConfig(%q) = %d, want an error", tt.content, got)
				}
				if !strings.Contains(err.Error(), path) {
					t.Fatalf("error %q does not name the config file %s", err, path)
				}
				return
			}
			if err != nil {
				t.Fatalf("ContainerPortFromConfig(%q): %v", tt.content, err)
			}
			if got != tt.want {
				t.Fatalf("port = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestContainerPortFromConfigNamesAMissingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "absent.yaml")
	got, err := ContainerPortFromConfig(os.ReadFile, path)
	if err == nil {
		t.Fatalf("ContainerPortFromConfig of a missing file = %d, want an error", got)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the config file %s", err, path)
	}
}

func TestContainerPortFromConfigRefusesARelativePath(t *testing.T) {
	t.Parallel()

	// The file is never written: the path rule is what refuses it, and it
	// must refuse before the plan reads anything.
	got, err := ContainerPortFromConfig(os.ReadFile, filepath.Join("data", "config.yaml"))
	if err == nil {
		t.Fatalf("ContainerPortFromConfig of a relative path = %d, want an error", got)
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error %q does not say the path must be absolute", err)
	}
}

func TestContainerPortFromConfigSaysThePortIsMissing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listener:\n  max_connections: 100\n"), 0600); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	_, err := ContainerPortFromConfig(os.ReadFile, path)
	if err == nil {
		t.Fatal("ContainerPortFromConfig accepted a listener with no port")
	}
	if !strings.Contains(err.Error(), "no listener.port") {
		t.Fatalf("error %q does not say the port is missing", err)
	}
}

func TestContainerPortFromConfigReadsThroughTheReader(t *testing.T) {
	t.Parallel()

	// The path is never written: a port parsed from the reader's bytes proves
	// the read goes through the caller's reader, not the live file.
	const path = "/town/.dolt-data/config.yaml"
	readFile := func(got string) ([]byte, error) {
		if got != path {
			t.Errorf("readFile(%q), want %q", got, path)
		}
		return []byte("listener:\n  port: 3399\n"), nil
	}
	got, err := ContainerPortFromConfig(readFile, path)
	if err != nil {
		t.Fatalf("ContainerPortFromConfig: %v", err)
	}
	if got != 3399 {
		t.Fatalf("port = %d, want 3399", got)
	}
}

func TestContainerPortFromConfigRefusesANilReader(t *testing.T) {
	t.Parallel()

	// Falling back to os.ReadFile would read the live config behind a caller
	// that injected no reader, so a nil reader is refused.
	got, err := ContainerPortFromConfig(nil, "/town/.dolt-data/config.yaml")
	if err == nil {
		t.Fatalf("ContainerPortFromConfig with a nil reader = %d, want an error", got)
	}
	if !strings.Contains(err.Error(), "no file reader") {
		t.Fatalf("error %q does not say the reader is missing", err)
	}
}
