// Package dockerimg reads the images a docker daemon holds and the containers
// that use them, and parses the sizes docker prints. It exists so a package that
// reads a docker daemon does not build its own docker calls.
package dockerimg

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
)

var (
	// ErrNoRunner reports a Client built without a docker runner.
	ErrNoRunner = errors.New("no docker runner configured")
	// ErrNoDockerHost reports a Client that does not name the daemon. Without it
	// docker would talk to whatever daemon the environment picks.
	ErrNoDockerHost = errors.New("no docker host configured; the daemon is not named")
	// ErrListImages reports an unreadable image list. The runner's error carries
	// the command's stderr, so it is dropped in favor of this fixed reason.
	ErrListImages = errors.New("list docker images failed; the image list is unreadable")
	// ErrListContainers reports an unreadable container list.
	ErrListContainers = errors.New("list docker containers failed; the in-use set is unreadable")
	// ErrInspect reports an unreadable container image lookup.
	ErrInspect = errors.New("inspect container images failed; the in-use set is unreadable")
	// ErrParse reports a line of docker output that does not parse, so the
	// listing is refused rather than read partly.
	ErrParse = errors.New("docker output line does not parse; the listing is refused")
	// ErrSize reports an image size ParseSize cannot read. A measurement that
	// needs every byte refuses on it rather than counting it as zero.
	ErrSize = errors.New("image size does not parse; the reading is refused")
)

var (
	// containerPattern matches the short or full container ids docker ps prints.
	containerPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	// sizeUnits are the units docker's human-readable sizes use.
	sizeUnits = map[string]float64{
		"B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
	}
)

// Image is one line of docker image ls --format {{json .}}.
type Image struct {
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
	ID         string `json:"ID"`
	Size       string `json:"Size"`
	// CreatedAt is the image's creation time as docker prints it, e.g.
	// "2026-11-14 22:16:03 +0000 UTC", for a caller that ranks images by age.
	CreatedAt string `json:"CreatedAt"`
}

// Bytes is the image's listed size, or 0 when docker printed one that does not
// parse: a size is best-effort and never a reason to refuse.
func (im Image) Bytes() int64 {
	n, err := ParseSize(im.Size)
	if err != nil {
		return 0
	}
	return n
}

// ParseSize reads a size as docker prints it ("1.5GB", "0B") and returns
// ErrSize for one it cannot read.
func ParseSize(size string) (int64, error) {
	i := strings.IndexFunc(size, func(r rune) bool { return !(r >= '0' && r <= '9' || r == '.') })
	if i <= 0 {
		return 0, ErrSize
	}
	n, err := strconv.ParseFloat(size[:i], 64)
	mult, ok := sizeUnits[size[i:]]
	if err != nil || !ok || n < 0 {
		return 0, ErrSize
	}
	return int64(math.Round(n * mult)), nil
}

// Client reads the daemon Host names, running every call through Run.
type Client struct {
	// Host is the daemon socket, passed to every docker call as DOCKER_HOST,
	// e.g. unix://<home>/.colima/<profile>/docker.sock.
	Host string
	// Run runs docker.
	Run diskmaint.Runner
}

// Ready reports whether the client can reach a daemon at all.
func (c Client) Ready() error {
	if c.Run == nil {
		return ErrNoRunner
	}
	if c.Host == "" {
		return ErrNoDockerHost
	}
	return nil
}

// Docker runs docker against the named daemon with args.
func (c Client) Docker(ctx context.Context, args ...string) ([]byte, error) {
	if err := c.Ready(); err != nil {
		return nil, err
	}
	return c.Run(ctx, []string{"DOCKER_HOST=" + c.Host}, "docker", args...)
}

// Images reads the daemon's image list. Every line must parse and carry a
// valid image id; one that does not refuses the whole listing.
func (c Client) Images(ctx context.Context) ([]Image, error) {
	out, err := c.Docker(ctx, "image", "ls", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		return nil, listErr(err, ErrListImages)
	}
	var images []Image
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var im Image
		if err := json.Unmarshal([]byte(line), &im); err != nil || !registry.ValidDigest(im.ID) {
			return nil, ErrParse
		}
		images = append(images, im)
	}
	return images, nil
}

// InUse is the set of image ids a container, running or stopped, uses. Any
// container or image id that does not parse refuses the set, so a container
// the set misses can never make a dangling image look unused.
func (c Client) InUse(ctx context.Context) (map[string]bool, error) {
	out, err := c.Docker(ctx, "ps", "-a", "-q")
	if err != nil {
		return nil, listErr(err, ErrListContainers)
	}
	ids := strings.Fields(string(out))
	used := map[string]bool{}
	if len(ids) == 0 {
		return used, nil
	}
	for _, id := range ids {
		if !containerPattern.MatchString(id) {
			return nil, ErrParse
		}
	}
	args := append([]string{"inspect", "--format", "{{.Image}}"}, ids...)
	out, err = c.Docker(ctx, args...)
	if err != nil {
		return nil, listErr(err, ErrInspect)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id := strings.TrimSpace(line)
		if !registry.ValidDigest(id) {
			return nil, ErrParse
		}
		used[id] = true
	}
	return used, nil
}

// listErr is the fixed reason for a failed docker call. A missing runner or host
// is named as such; any other failure is the given sentinel, since the runner's
// error carries the command's stderr.
func listErr(err, fallback error) error {
	if errors.Is(err, ErrNoRunner) || errors.Is(err, ErrNoDockerHost) {
		return err
	}
	return fallback
}
