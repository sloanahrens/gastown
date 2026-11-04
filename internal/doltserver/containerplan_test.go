package doltserver

import (
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
		MemoryGiB:     12,
		HostPort:      3307,
		ContainerPort: 3307,
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
		"--memory", "12g",
		"--memory-swap", "12g",
		"--restart", "unless-stopped",
		"-p", "127.0.0.1:3307:3307",
		"-v", "/town/.dolt-data:/town/.dolt-data",
		"-v", "/home/u/gt-backups/dolt:/home/u/gt-backups/dolt",
		"-v", "/town/.dolt-data/config.yaml:/town/.dolt-data/config.yaml:ro",
		"--label", "org.gastown.keep=true",
		"dolt:test",
		"dolt", "sql-server", "--config", "/town/.dolt-data/config.yaml",
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
