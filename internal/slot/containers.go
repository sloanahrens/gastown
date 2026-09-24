package slot

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// GateContainer is one running container the gate's `docker ps` listing
// matched, with the evidence the gate judges it on.
type GateContainer struct {
	ID    string
	Image string
	Name  string

	// Created is zero when docker reported no parseable start time. Classify
	// reads that as an unknowable age, not as a young container.
	Created time.Time

	// Labels is the container's label set, empty when it carries none.
	Labels map[string]string
}

// Display renders the container the way `gt slot status` and the gate's own
// log lines name it.
func (c GateContainer) Display() string {
	return c.Image + " " + c.Name
}

// Age returns how long the container has been running; ok is false when
// docker reported no start time for it.
func (c GateContainer) Age(now time.Time) (time.Duration, bool) {
	if c.Created.IsZero() {
		return 0, false
	}
	return now.Sub(c.Created), true
}

// IsReaper reports whether the container is a testcontainers ryuk reaper, the
// sidecar whose lifetime is its session's.
func (c GateContainer) IsReaper() bool {
	return strings.Contains(strings.ToLower(c.Image+" "+c.Name), "ryuk")
}

// SessionID returns the testcontainers session the container belongs to, or
// "" when its labels name none.
//
// The label key is searched for, not spelled out, because testcontainers
// clients and versions disagree on it ("org.testcontainers.sessionId",
// "org.testcontainers.session-id"): a key this gate failed to recognize would
// make a live session's containers look like debris.
func (c GateContainer) SessionID() string {
	for k, v := range c.Labels {
		if strings.Contains(strings.ToLower(k), "session") {
			return v
		}
	}
	return ""
}

// LabelSummary renders the labels the way a reap warning needs them: sorted,
// so two runs of the same command print the same evidence.
func (c GateContainer) LabelSummary() string {
	if len(c.Labels) == 0 {
		return "(none)"
	}
	pairs := make([]string, 0, len(c.Labels))
	for k, v := range c.Labels {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// dockerPSFormat asks docker for one JSON object per container. Image and
// name alone do not carry the age and the session the gate classifies on, and
// docker's column separator would not survive its own Labels field ("k=v,k=v"
// — a comma in a value shifts every column after it).
//
// Verified against Docker 29.8.0 on this host: CreatedAt arrives as
// "2026-09-21 20:42:23 -0500 CDT", not RFC 3339 (gt-ul1k).
const dockerPSFormat = "{{json .}}"

// dockerPSContainer is the subset of `docker ps --format {{json .}}` the gate
// reads.
type dockerPSContainer struct {
	ID        string `json:"ID"`
	Image     string `json:"Image"`
	Names     string `json:"Names"`
	CreatedAt string `json:"CreatedAt"`
	Labels    string `json:"Labels"`
}

// dockerCreatedAtLayouts are the layouts parseDockerCreatedAt accepts: the one
// docker renders today, then RFC 3339 for a docker that switches to it.
var dockerCreatedAtLayouts = []string{
	"2006-01-02 15:04:05 -0700 MST",
	time.RFC3339Nano,
}

// parseGateContainer turns one line of the gate's `docker ps` output into a
// GateContainer. A line that is not docker's JSON — a test stub, or output
// from an older binary's image/name format — yields image and name only, and
// therefore an unknown age, which Classify refuses to call debris.
func parseGateContainer(line string) GateContainer {
	line = strings.TrimSpace(line)
	var raw dockerPSContainer
	if err := json.Unmarshal([]byte(line), &raw); err == nil && raw.Image != "" {
		return GateContainer{
			ID:      raw.ID,
			Image:   raw.Image,
			Name:    raw.Names,
			Created: parseDockerCreatedAt(raw.CreatedAt),
			Labels:  parseDockerLabels(raw.Labels),
		}
	}
	image, name, _ := strings.Cut(line, " ")
	return GateContainer{Image: image, Name: strings.TrimSpace(name)}
}

// GateContainers lists the gate containers running right now, parsed. It is
// the same `docker ps` cross-check Status performs, in per-container form, for
// a caller that needs the individual containers rather than a busy/free
// verdict — gt done's container watch (gt-0ss4), which diffs the listing
// against the one it took when its slot-free run started to tell a container
// THAT RUN started from one that was already there.
//
// A non-nil error means the check could not be performed and must be read as
// "unknown", never as "no containers running". Goes through the same
// runningGateContainers var Acquire and Status use, so tests substitute it
// with SetContainerListerForTest.
func GateContainers() ([]GateContainer, error) {
	lines, err := runningGateContainers()
	if err != nil {
		return nil, err
	}
	containers := make([]GateContainer, 0, len(lines))
	for _, line := range lines {
		containers = append(containers, parseGateContainer(line))
	}
	return containers, nil
}

// parseDockerCreatedAt parses docker's start-time string, returning the zero
// time for anything it does not recognize.
func parseDockerCreatedAt(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range dockerCreatedAtLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseDockerLabels splits docker's comma-joined "k=v,k=v" label string. A
// comma inside a label value splits wrong; the gate reads session ids out of
// this map and nothing else, and a session id is a UUID, so the loss cannot
// turn a container the gate could judge into one it cannot.
func parseDockerLabels(s string) map[string]string {
	if s == "" {
		return nil
	}
	labels := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			continue
		}
		labels[key] = value
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

// Verdict is how the gate reads a running container's evidence.
type Verdict string

const (
	// VerdictLive means the container belongs to a suite that is still running.
	VerdictLive Verdict = "live"

	// VerdictDebris means the container outlived its suite: older than the
	// staleness window with no live ryuk reaper for its session.
	VerdictDebris Verdict = "debris"

	// VerdictUnknown means docker reported no start time, so the gate cannot
	// tell this container's debris from a live suite's.
	VerdictUnknown Verdict = "unknown"
)

// StaleContainerWindow is the age past which a gate container is a debris
// candidate. Declared as a var so tests can shorten it.
var StaleContainerWindow = 30 * time.Minute

// ContainerVerdict is one container's classification with the evidence behind
// it, so a log line, a reap, or a doctor warning can say why.
type ContainerVerdict struct {
	Container GateContainer
	Verdict   Verdict
	Reason    string
}

// Blocks reports whether the container still occupies the gate: a live suite
// does, and so does one whose age could not be established. Debris does not —
// that is the whole point of the classification (gt-ul1k).
func (v ContainerVerdict) Blocks() bool {
	return v.Verdict != VerdictDebris
}

// debrisWriter is where the gate's debris warnings go. Declared as a var so a
// test can read the evidence back rather than have it land on its own stderr.
var debrisWriter io.Writer = os.Stderr

// logDebris records one container the gate is walking past, with the age and
// the labels an operator needs to confirm the verdict before reaping — the
// gate grants on debris now, so the evidence for that call has to be on the
// record rather than inferred from a container someone removed by hand
// (gt-ul1k).
func logDebris(verdict ContainerVerdict) {
	fmt.Fprintf(debrisWriter, "gt slot: ignoring %s as stale debris (%s); labels: %s. Remove it with 'gt slot reap'.\n",
		verdict.Container.Display(), verdict.Reason, verdict.Container.LabelSummary())
}

// debrisLogger returns a logDebris that records each distinct container once.
// Acquire polls while a live suite runs, so a container this gate is walking
// past would otherwise be re-announced on every poll.
func debrisLogger() func(ContainerVerdict) {
	seen := map[string]bool{}
	return func(verdict ContainerVerdict) {
		key := verdict.Container.ID + "\x00" + verdict.Container.Display()
		if seen[key] {
			return
		}
		seen[key] = true
		logDebris(verdict)
	}
}

// Classify decides, per container, whether it still occupies the gate.
//
// A container is live while it is younger than window, and after that only if
// a ryuk reaper for its session is still running — a reaper's lifetime is its
// session's, which is why the town's debris criterion is "older than the
// window AND its reaper is gone" (gt-ul1k). A reaper older than the window is
// itself debris: it is the last thing to exit a session, so one that outlived
// its session leaked, and leaving it would reproduce exactly the deadlock this
// classification exists to clear.
func Classify(containers []GateContainer, now time.Time, window time.Duration) []ContainerVerdict {
	if window <= 0 {
		window = StaleContainerWindow
	}

	// liveReapers maps session id to the reaper keeping that session alive.
	// Only a young reaper counts: an old one is a leak, not a signal.
	liveReapers := make(map[string]string)
	for _, c := range containers {
		if !c.IsReaper() {
			continue
		}
		id := c.SessionID()
		if id == "" {
			continue
		}
		if age, ok := c.Age(now); ok && age < window {
			liveReapers[id] = c.Display()
		}
	}

	out := make([]ContainerVerdict, 0, len(containers))
	for _, c := range containers {
		age, ok := c.Age(now)
		switch {
		case !ok:
			out = append(out, ContainerVerdict{c, VerdictUnknown,
				"docker reported no start time, so its age is unverifiable"})
		case age < window:
			out = append(out, ContainerVerdict{c, VerdictLive,
				fmt.Sprintf("age %s is under the %s staleness window", age.Round(time.Second), window)})
		case c.IsReaper():
			out = append(out, ContainerVerdict{c, VerdictDebris,
				fmt.Sprintf("reaper age %s exceeds the %s staleness window, so the session it reaped for is over",
					age.Round(time.Second), window)})
		default:
			// An empty session id never maps to a reaper, so a container that
			// names no session falls straight through to debris.
			if reaper := liveReapers[c.SessionID()]; reaper != "" {
				out = append(out, ContainerVerdict{c, VerdictLive,
					fmt.Sprintf("reaper %s is still running for its session", reaper)})
				continue
			}
			out = append(out, ContainerVerdict{c, VerdictDebris,
				fmt.Sprintf("age %s exceeds the %s staleness window with no reaper running for its session",
					age.Round(time.Second), window)})
		}
	}
	return out
}
