package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/templates"
)

// The Dolt image check (gt-goda5m): the town runs the newest stable
// dolthub/dolt-sql-server image, so once an hour the daemon reads the image of
// the town's Dolt container and asks Docker Hub for the newest release. A
// container more than one release behind raises one low attention item
// that names both versions, and the item clears when they match. The check
// never pulls an image and never restarts a container.
const (
	// doltImageCheckEvery is how often the check asks: releases are a few a
	// month, so an hourly look is prompt without being a poll.
	doltImageCheckEvery = time.Hour
	// doltImageCheckBudget bounds one check's reads. The check runs on the
	// heartbeat, so a registry that will not answer holds the heartbeat for at
	// most this long, once an hour.
	doltImageCheckBudget = time.Minute
	// doltImageRequestTimeout bounds one registry request.
	doltImageRequestTimeout = 20 * time.Second
	// doltImageTagsURL is Docker Hub's tag listing of the Dolt server image.
	doltImageTagsURL = "https://hub.docker.com/v2/repositories/dolthub/dolt-sql-server/tags"
	// doltImageTagsPageSize is how many tags one page asks for.
	doltImageTagsPageSize = 100
	// doltImageTagsMaxPages bounds the listing. A listing that has not ended by
	// then cannot be known to hold the newest release, so it is an error.
	doltImageTagsMaxPages = 20
	// doltImageTagsMaxBytes bounds one page's body.
	doltImageTagsMaxBytes = 4 << 20
	// doltImageKey is the one item the check holds.
	doltImageKey = "dolt-image-behind"
)

// doltPlainVersion matches a plain major.minor.patch release. A tag with any
// other shape (latest, a release candidate, a commit) is not a release.
var doltPlainVersion = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// doltVersion is one plain release: its numbers and the tag they came from.
type doltVersion struct {
	major, minor, patch int
	tag                 string
}

// parseDoltVersion reads tag as a plain release.
func parseDoltVersion(tag string) (doltVersion, bool) {
	m := doltPlainVersion.FindStringSubmatch(tag)
	if m == nil {
		return doltVersion{}, false
	}
	var nums [3]int
	for i := range nums {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return doltVersion{}, false
		}
		nums[i] = n
	}
	return doltVersion{major: nums[0], minor: nums[1], patch: nums[2], tag: tag}, true
}

// less reports whether v is an older release than o.
func (v doltVersion) less(o doltVersion) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}

// doltImageBehind reports whether a container on running is behind the
// releases listed. The gap is counted in releases, not in patch numbers, so a
// minor boundary is crossed like any other: one release newer is not behind,
// since a release published a moment ago is not a lapse. Two or more newer
// releases are.
func doltImageBehind(running doltVersion, releases []doltVersion) bool {
	newer := 0
	for _, r := range releases {
		if running.less(r) {
			newer++
		}
	}
	return newer > 1
}

// doltReleases is every plain release among tags. A listing with no plain
// release is an error: nothing to compare against is not a release.
func doltReleases(tags []string) ([]doltVersion, error) {
	var releases []doltVersion
	for _, tag := range tags {
		if v, ok := parseDoltVersion(tag); ok {
			releases = append(releases, v)
		}
	}
	if len(releases) == 0 {
		return nil, errors.New("the registry listed no plain major.minor.patch tag")
	}
	return releases, nil
}

// doltNewest is the highest release in releases, which must not be empty.
func doltNewest(releases []doltVersion) doltVersion {
	newest := releases[0]
	for _, r := range releases[1:] {
		if newest.less(r) {
			newest = r
		}
	}
	return newest
}

// doltImageTag is the tag of an image reference: the text after the last colon
// of its last path element, before any digest. A reference with no tag is not
// ok.
func doltImageTag(ref string) (string, bool) {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	last := ref[strings.LastIndex(ref, "/")+1:]
	i := strings.LastIndex(last, ":")
	if i < 0 {
		return "", false
	}
	return last[i+1:], true
}

// doltImageCheck is the Dolt image check's state: the one item it holds and
// when it next asks. The item is written on the heartbeat goroutine and read by
// the attention tick, so a mutex guards it; next is touched by run alone.
//
// The fields after item are seams. A nil one is the production read, and a
// test replaces it so no test starts docker or reaches the registry.
type doltImageCheck struct {
	mu   sync.Mutex
	item *attention.Item
	next time.Time

	// container names the town's Dolt container: "" for a town whose Dolt runs
	// as a host process. Nil reads dolt.external_container.
	container func(townRoot string) (string, error)
	// inspect runs docker. Nil is templates.RunDockerInspect.
	inspect templates.ContainerDocker
	// tagsURL is the registry's tag listing. "" is Docker Hub's.
	tagsURL string
	// client makes the registry requests. Nil is a client bounded by
	// doltImageRequestTimeout.
	client *http.Client
}

// run makes the check when it is due at now, and does nothing otherwise. It
// reports each failed read through logf, once per run.
//
// A read that fails leaves the item as it was: a registry or docker that does
// not answer says nothing about whether the image is behind, so the item is
// neither raised nor cleared by it.
func (c *doltImageCheck) run(ctx context.Context, townRoot string, now time.Time, logf func(string, ...any)) {
	if now.Before(c.next) {
		return
	}
	c.next = now.Add(doltImageCheckEvery)
	container, err := c.containerName(townRoot)
	switch {
	case err != nil:
		logf("dolt image check: reading dolt.external_container: %v", err)
		return
	case container == "":
		// A host-process Dolt has no container image to compare.
		c.set(nil)
		return
	}
	running, err := c.runningVersion(ctx, container)
	if err != nil {
		logf("dolt image check: reading the image of container %s: %v", container, err)
		return
	}
	releases, err := c.newestRelease(ctx)
	if err != nil {
		logf("dolt image check: reading the newest release from the registry: %v", err)
		return
	}
	newest := doltNewest(releases)
	if !doltImageBehind(running, releases) {
		c.set(nil)
		return
	}
	c.set(&attention.Item{
		Key:      doltImageKey,
		Kind:     attention.KindDoltImageBehind,
		Severity: attention.SeverityLow,
		Summary: fmt.Sprintf("the Dolt container %s runs dolt-sql-server %s, and the newest release is %s: pull the newer image before the next Dolt restart",
			container, running.tag, newest.tag),
	})
}

// set replaces the held item; nil clears it.
func (c *doltImageCheck) set(it *attention.Item) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.item = it
}

// observed is the item the check holds, for the attention queue.
func (c *doltImageCheck) observed() []attention.Item {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.item == nil {
		return nil
	}
	return []attention.Item{*c.item}
}

// containerName is the town's Dolt container, "" when the town runs Dolt as a
// host process. A setting that does not parse is an error.
func (c *doltImageCheck) containerName(townRoot string) (string, error) {
	if c.container != nil {
		return c.container(townRoot)
	}
	return doltserver.DefaultConfig(townRoot).ExternalContainerSetting()
}

// runningVersion is the release the container's image is tagged with. The
// name is passed as one argv element and must not read as a flag.
func (c *doltImageCheck) runningVersion(ctx context.Context, container string) (doltVersion, error) {
	if strings.HasPrefix(container, "-") {
		return doltVersion{}, errors.New("dolt.external_container starts with '-', which docker would read as a flag")
	}
	inspect := c.inspect
	if inspect == nil {
		inspect = templates.RunDockerInspect
	}
	ctx, cancel := context.WithTimeout(ctx, templates.ContainerInspectTimeout)
	defer cancel()
	out, err := inspect(ctx, []string{"docker", "inspect", "-f", "{{.Config.Image}}", container})
	if err != nil {
		return doltVersion{}, err
	}
	tag, ok := doltImageTag(strings.TrimSpace(string(out)))
	if !ok {
		return doltVersion{}, errors.New("the container's image reference carries no tag")
	}
	v, ok := parseDoltVersion(tag)
	if !ok {
		return doltVersion{}, errors.New("the container's image is not tagged with a plain major.minor.patch release")
	}
	return v, nil
}

// newestRelease reads every page of the registry's tag listing and returns
// every plain release in it.
func (c *doltImageCheck) newestRelease(ctx context.Context) ([]doltVersion, error) {
	var tags []string
	for page := 1; ; page++ {
		if page > doltImageTagsMaxPages {
			return nil, fmt.Errorf("the tag listing runs past %d pages", doltImageTagsMaxPages)
		}
		p, err := c.tagsPage(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, r := range p.Results {
			tags = append(tags, r.Name)
		}
		if p.Next == "" {
			break
		}
	}
	return doltReleases(tags)
}

// doltTagsPage is one page of the registry's tag listing. Next is empty on the
// last page.
type doltTagsPage struct {
	Next    string `json:"next"`
	Results []struct {
		Name string `json:"name"`
	} `json:"results"`
}

// tagsPage reads one page of the tag listing.
func (c *doltImageCheck) tagsPage(ctx context.Context, page int) (doltTagsPage, error) {
	base := c.tagsURL
	if base == "" {
		base = doltImageTagsURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s?page_size=%d&page=%d", base, doltImageTagsPageSize, page), nil)
	if err != nil {
		return doltTagsPage{}, err
	}
	client := c.client
	if client == nil {
		client = &http.Client{Timeout: doltImageRequestTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return doltTagsPage{}, fmt.Errorf("asking the registry for tags: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return doltTagsPage{}, fmt.Errorf("the registry answered %s for the tag listing", resp.Status)
	}
	var p doltTagsPage
	if err := json.NewDecoder(io.LimitReader(resp.Body, doltImageTagsMaxBytes)).Decode(&p); err != nil {
		return doltTagsPage{}, fmt.Errorf("reading the registry's tag listing: %w", err)
	}
	return p, nil
}

// checkDoltImage is the Dolt image check's heartbeat step. It reads the
// container and the registry and never changes either, so it is not a
// lifecycle step: an E-stopped town still gets the report.
func (d *Daemon) checkDoltImage() {
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, doltImageCheckBudget)
	defer cancel()
	d.doltImage.run(ctx, d.config.TownRoot, d.clk().Now(), d.logger.Printf)
}

// collectDoltImage returns the item the Dolt image check holds.
func (s *attentionSources) collectDoltImage(context.Context) ([]attention.Item, error) {
	if s.doltImage == nil {
		return nil, nil
	}
	return s.doltImage(), nil
}
