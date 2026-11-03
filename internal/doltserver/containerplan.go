package doltserver

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	configpkg "github.com/steveyegge/gastown/internal/config"
)

// ContainerSpec is the docker create plan for a container that serves the
// town's Dolt data.
type ContainerSpec struct {
	Name, Image, DataDir, BackupDir, ConfigPath string
	MemoryGiB, HostPort, ContainerPort          int
	Restart                                     string // no, on-failure, always or unless-stopped
}

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
// backups the server writes keep working.
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
		s.Image,
		"dolt", "sql-server", "--config", s.ConfigPath,
	}, nil
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
