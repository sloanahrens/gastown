package doltserver

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	configpkg "github.com/steveyegge/gastown/internal/config"
)

// ContainerSpec is the docker create plan for a container that serves the
// town's Dolt data.
//
// ContainerPort is the port the server listens on inside the container: the
// port the config's listener names, which ContainerPortFromConfig reads.
type ContainerSpec struct {
	Name, Image, DataDir, BackupDir, ConfigPath string
	MemoryGiB, HostPort, ContainerPort          int
	Restart                                     string // no, on-failure, always or unless-stopped
}

// keepLabel marks the container as one the town keeps: the slot reaper matches
// it before it classifies a container (gt-awq73), so the production Dolt
// container is not reaped on age when the town configures no container name.
const keepLabel = "org.gastown.keep=true"

// containerRestartPolicies are the four values docker create accepts for
// --restart. A policy outside them is refused rather than handed to docker.
var containerRestartPolicies = map[string]bool{
	"no":             true,
	"on-failure":     true,
	"always":         true,
	"unless-stopped": true,
}

// ContainerPlan returns the docker create argv for s, element by element, so
// an operator can read, review and run it; it runs no process and writes no
// file.
//
// The data and backup directories are mounted at the same absolute path inside
// and outside the container, so host tools that read .dolt-data and the
// backups the server writes keep working. The host port is published on the
// loopback and maps to ContainerPort, the listener port the server binds
// inside the container. The keep label sits directly before the image
// reference. The arguments after the image are --config and its path only:
// they go to the image's own entrypoint, which starts the server.
func ContainerPlan(s ContainerSpec) ([]string, error) {
	if s.Name == "" {
		return nil, errors.New("container name is required")
	}
	// ParseExternalContainer is the one owner of the name rule: it refuses
	// anything but letters, digits, '_', '.' and '-', starting with a letter
	// or digit. An empty name is answered "" with no error there, hence the
	// check above.
	if _, err := (&configpkg.DoltEndpoint{ExternalContainer: s.Name}).ParseExternalContainer(); err != nil {
		return nil, fmt.Errorf("container name: %w", err)
	}
	if s.Image == "" {
		return nil, errors.New("image is required")
	}
	if strings.HasPrefix(s.Image, "-") {
		return nil, fmt.Errorf("image %q starts with '-', which docker reads as a flag", s.Image)
	}
	if err := validateContainerPath("data directory", s.DataDir); err != nil {
		return nil, err
	}
	if err := validateContainerPath("backup directory", s.BackupDir); err != nil {
		return nil, err
	}
	if err := validateContainerPath("config path", s.ConfigPath); err != nil {
		return nil, err
	}
	if s.MemoryGiB < 1 {
		return nil, fmt.Errorf("memory %d GiB: want at least 1 GiB", s.MemoryGiB)
	}
	if s.HostPort < 1 || s.HostPort > 65535 {
		return nil, fmt.Errorf("host port %d: want 1 to 65535", s.HostPort)
	}
	if s.ContainerPort < 1 || s.ContainerPort > 65535 {
		return nil, fmt.Errorf("container port %d: want 1 to 65535", s.ContainerPort)
	}
	if !containerRestartPolicies[s.Restart] {
		return nil, fmt.Errorf("restart policy %q: want one of no, on-failure, always, unless-stopped", s.Restart)
	}

	memory := fmt.Sprintf("%dg", s.MemoryGiB)
	dataMount := s.DataDir + ":" + s.DataDir
	backupMount := s.BackupDir + ":" + s.BackupDir
	configMount := s.ConfigPath + ":" + s.ConfigPath + ":ro"
	return []string{
		"docker", "create",
		"--name", s.Name,
		"--memory", memory,
		"--memory-swap", memory,
		"--restart", s.Restart,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", s.HostPort, s.ContainerPort),
		"-v", dataMount,
		"-v", backupMount,
		"-v", configMount,
		"--label", keepLabel,
		s.Image,
		"--config", s.ConfigPath,
	}, nil
}

// ContainerPortFromConfig returns the port the Dolt server listens on inside
// the container, from listener.port in the config file at path. The file is
// read through readFile, the reader the calling command injects, so a test
// reads a config without touching the filesystem. A nil reader is refused
// rather than replaced by a read of the live file.
//
// The live container publishes the host's port to this listener port, so the
// command reads it instead of assuming the host's port. A path ContainerPlan
// would refuse, a file that cannot be read or does not parse, and a file that
// names no listener.port or names one outside 1..65535 are all errors naming
// path: the plan never guesses a port.
func ContainerPortFromConfig(readFile func(path string) ([]byte, error), path string) (int, error) {
	if readFile == nil {
		return 0, errors.New("container port: no file reader")
	}
	if err := validateContainerPath("config path", path); err != nil {
		return 0, fmt.Errorf("container port: %w", err)
	}
	data, err := readFile(path)
	if err != nil {
		return 0, fmt.Errorf("container port: reading the config: %w", err)
	}
	var cfg struct {
		Listener struct {
			Port *int `yaml:"port"`
		} `yaml:"listener"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return 0, fmt.Errorf("container port: %s: %w", path, err)
	}
	if cfg.Listener.Port == nil {
		return 0, fmt.Errorf("container port: %s names no listener.port", path)
	}
	port := *cfg.Listener.Port
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("container port: %s names listener.port %d: want 1 to 65535", path, port)
	}
	return port, nil
}

// validateContainerPath refuses a path docker cannot mount as written. The
// -v flag splits on ':', so a path holding one would mount a different path,
// or a named volume, than the plan says; a mount of the host's whole
// filesystem or of the Docker socket is refused rather than emitted.
func validateContainerPath(label, path string) error {
	if path == "" {
		return fmt.Errorf("%s is required", label)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s %q is not an absolute path", label, path)
	}
	if cleaned := filepath.Clean(path); cleaned != path {
		return fmt.Errorf("%s %q is not clean; use %q", label, path, cleaned)
	}
	if path == "/" {
		return fmt.Errorf("%s must not be /", label)
	}
	if strings.Contains(path, ":") {
		return fmt.Errorf("%s %q contains ':', which docker's -v syntax splits on", label, path)
	}
	if filepath.Base(path) == "docker.sock" {
		return fmt.Errorf("%s %q is the Docker socket", label, path)
	}
	return nil
}
