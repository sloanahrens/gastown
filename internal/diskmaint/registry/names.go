// Package registry talks to the container registry's v2 HTTP API and decides
// which manifests a maintenance run may delete. Every name taken from the
// registry or from the sandbox records is validated before it reaches a URL
// path or a command.
package registry

import (
	"errors"
	"regexp"
)

var (
	// ErrDeletesDisabled means the registry refused a delete because delete
	// support is turned off.
	ErrDeletesDisabled = errors.New("registry deletes are disabled (HTTP 405)")
	// ErrNotFound means the registry has no such repository, tag or manifest.
	ErrNotFound = errors.New("registry: not found (HTTP 404)")
	// ErrBadName means a repo, tag, digest or container name failed validation.
	ErrBadName = errors.New("registry: name failed validation")
)

var (
	repoPattern      = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	tagPattern       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	containerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// ValidRepo reports whether s is a repository name safe to put in a URL path.
func ValidRepo(s string) bool { return repoPattern.MatchString(s) }

// ValidTag reports whether s is a tag name safe to put in a URL path.
func ValidTag(s string) bool { return tagPattern.MatchString(s) }

// ValidDigest reports whether s is a sha256 manifest digest.
func ValidDigest(s string) bool { return digestPattern.MatchString(s) }

// ValidContainer reports whether s is a container name safe to put in a command.
func ValidContainer(s string) bool { return containerPattern.MatchString(s) }
