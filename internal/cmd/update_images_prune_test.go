package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/update"
)

// pruneTestVersion is one package version the fake packages listing serves, in
// the shape the reader reads.
type pruneTestVersion struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Metadata  struct {
		ManifestDigest string `json:"manifest_digest"`
	} `json:"metadata"`
}

// pruneTestTag is one tag of an image in the fake listing.
type pruneTestTag struct {
	name   string
	digest string
	pushed time.Time
}

// pruneTestRegistry is the registry an injected HTTP client answers for. It
// serves the packages listing from tags, and it records every request, so a test
// sees what the command sent.
type pruneTestRegistry struct {
	tags    map[string][]pruneTestTag
	status  map[string]int
	methods []string
	paths   []string
}

// RoundTrip answers a packages listing request for the image its q names.
func (f *pruneTestRegistry) RoundTrip(req *http.Request) (*http.Response, error) {
	f.methods = append(f.methods, req.Method)
	f.paths = append(f.paths, req.URL.Path)
	image := req.URL.Query().Get("q")
	if code := f.status[image]; code != 0 {
		return pruneTestResponse(req, code, nil, 0), nil
	}
	var items []pruneTestVersion
	if req.URL.Query().Get("page") == "1" {
		for _, t := range f.tags[image] {
			var v pruneTestVersion
			v.Name, v.Version, v.CreatedAt = image, t.name, t.pushed
			v.Metadata.ManifestDigest = t.digest
			items = append(items, v)
		}
	}
	body, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return pruneTestResponse(req, http.StatusOK, body, len(f.tags[image])), nil
}

// pruneTestResponse answers one request with code, body and the listing total.
func pruneTestResponse(req *http.Request, code int, body []byte, total int) *http.Response {
	h := http.Header{}
	h.Set("X-Total-Count", strconv.Itoa(total))
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(bytes.NewReader(body)), Request: req}
}

// pruneTestLedger is the digest install's record the command reads its pairs
// from.
type pruneTestLedger struct {
	rec update.ImageRecord
	err error
}

// Read returns the record, or the error the test set.
func (l pruneTestLedger) Read() (update.ImageRecord, error) { return l.rec, l.err }

// pruneTestName and pruneTestDigest are the tag name and manifest digest of the
// n-th tag a fixture makes.
func pruneTestName(n int) string   { return fmt.Sprintf("%012x", n) }
func pruneTestDigest(n int) string { return fmt.Sprintf("sha256:%064x", n) }

// pruneTestRef is the pair ref the record holds for image at digest n.
func pruneTestRef(image string, n int) string {
	return "forgejo.test/" + testUpdateImagesOwner + "/" + image + "@" + pruneTestDigest(n)
}

// pruneTestRecord is a record whose installed and previous pairs name digests no
// listed tag carries.
func pruneTestRecord() update.ImageRecord {
	pair := func(base int) update.ImagePair {
		return update.ImagePair{
			Daemon:  pruneTestRef(imagevalues.DaemonImage, base),
			Polecat: pruneTestRef(imagevalues.PolecatImage, base+1),
			Bd:      pruneTestRef(imagevalues.BdImage, base+2),
			Commit:  testUpdateImagesSHA,
		}
	}
	return update.ImageRecord{Current: pair(900), Previous: pair(910)}
}

// pruneTestPopulated is a registry whose daemon image has 22 tags, one an hour
// apart, and whose bd image has one. The polecat image has no tags.
func pruneTestPopulated() *pruneTestRegistry {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var daemon []pruneTestTag
	for i := 0; i < 22; i++ {
		n := i + 1
		daemon = append(daemon, pruneTestTag{name: pruneTestName(n), digest: pruneTestDigest(n), pushed: base.Add(time.Duration(i) * time.Hour)})
	}
	return &pruneTestRegistry{tags: map[string][]pruneTestTag{
		imagevalues.DaemonImage: daemon,
		imagevalues.BdImage:     {{name: pruneTestName(100), digest: pruneTestDigest(100), pushed: base}},
	}}
}

// pruneTestSeams wires the command over reg, with the record, the green refs and
// the planner the production build uses unless a test replaces one.
func pruneTestSeams(reg *pruneTestRegistry) updateImagesSeams {
	return updateImagesSeams{
		FindTownRoot: func() (string, error) { return "town", nil },
		Registry: func(string) (imagevalues.Registry, error) {
			return imagevalues.Registry{Scheme: "https", Host: "forgejo.test", Owner: testUpdateImagesOwner}, nil
		},
		Credentials: func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
		HTTPClient:  &http.Client{Transport: reg},
		PruneLedger: func(string) update.PruneLedger { return pruneTestLedger{rec: pruneTestRecord()} },
		PruneGreen: func(context.Context, string) (map[string]bool, error) {
			return map[string]bool{pruneTestName(1): true}, nil
		},
		PlanPrune: update.PlanPrune,
	}
}

// runUpdateImagesPruneCmd runs `gt update images prune` over seams and returns
// what it printed to stdout and its error. Stderr is kept apart, so a refusal's
// message is not read as a plan line.
func runUpdateImagesPruneCmd(t *testing.T, seams updateImagesSeams) (string, error) {
	t.Helper()
	c := newUpdateImagesCmd(seams)
	var out, errOut bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs([]string{"prune"})
	err := c.Execute()
	return out.String(), err
}

// TestUpdateImagesPruneFailsClosedOnAnUnreadableInput: a reader error or a
// planner refusal exits non-zero and prints no line, so nothing is listed for
// deletion.
func TestUpdateImagesPruneFailsClosedOnAnUnreadableInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(s *updateImagesSeams, reg *pruneTestRegistry)
	}{
		{"ledger unreadable", func(s *updateImagesSeams, _ *pruneTestRegistry) {
			s.PruneLedger = func(string) update.PruneLedger { return pruneTestLedger{err: errors.New("record unreadable")} }
		}},
		{"registry listing refused", func(_ *updateImagesSeams, reg *pruneTestRegistry) {
			reg.status = map[string]int{imagevalues.PolecatImage: http.StatusInternalServerError}
		}},
		{"green refs unreadable", func(s *updateImagesSeams, _ *pruneTestRegistry) {
			s.PruneGreen = func(context.Context, string) (map[string]bool, error) {
				return nil, errors.New("branch listing failed")
			}
		}},
		{"planner refuses", func(s *updateImagesSeams, _ *pruneTestRegistry) {
			s.PlanPrune = func(update.PruneInput) (update.PrunePlan, error) {
				return update.PrunePlan{}, fmt.Errorf("%w: test refusal", update.ErrPruneRefused)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := pruneTestPopulated()
			seams := pruneTestSeams(reg)
			tc.setup(&seams, reg)
			out, err := runUpdateImagesPruneCmd(t, seams)
			if err == nil {
				t.Fatal("prune exited zero over an unreadable input")
			}
			if out != "" {
				t.Fatalf("prune printed a plan after a refusal:\n%s", out)
			}
		})
	}
}

// TestUpdateImagesPruneSendsOnlyReads: with no flag the command makes only GET
// requests to the registry and has no flag that could delete.
func TestUpdateImagesPruneSendsOnlyReads(t *testing.T) {
	t.Parallel()
	reg := pruneTestPopulated()
	seams := pruneTestSeams(reg)
	if _, err := runUpdateImagesPruneCmd(t, seams); err != nil {
		t.Fatalf("prune over readable inputs: %v", err)
	}
	if len(reg.methods) == 0 {
		t.Fatal("prune made no registry read")
	}
	for i, m := range reg.methods {
		if m != http.MethodGet {
			t.Fatalf("request %d is %s %s, want only GET reads", i, m, reg.paths[i])
		}
	}
	if flags := newUpdateImagesPruneCmd(seams).Flags(); flags.HasFlags() {
		t.Fatal("prune has a flag, so it is not the read-only plan")
	}
}

// TestUpdateImagesPrunePrintsEachImagesKeepAndDeleteLines: each image's keep and
// delete lines carry their reasons, and the counts follow each image and the
// total, from the injected readers.
func TestUpdateImagesPrunePrintsEachImagesKeepAndDeleteLines(t *testing.T) {
	t.Parallel()
	seams := pruneTestSeams(pruneTestPopulated())
	out, err := runUpdateImagesPruneCmd(t, seams)
	if err != nil {
		t.Fatalf("prune over readable inputs: %v", err)
	}

	var want strings.Builder
	// The daemon's newest 20 tags are kept by rank; the 21st is kept by its green
	// ref; the oldest, the 22nd, is the one delete candidate.
	for n := 22; n >= 3; n-- {
		fmt.Fprintf(&want, "keep   %s:%s %s: one of the newest 20 tags\n", imagevalues.DaemonImage, pruneTestName(n), pruneTestDigest(n))
	}
	fmt.Fprintf(&want, "keep   %s:%s %s: green/%s ref\n", imagevalues.DaemonImage, pruneTestName(1), pruneTestDigest(1), pruneTestName(1))
	fmt.Fprintf(&want, "delete %s:%s %s: pushed 2026-11-01T01:00:00Z, rank 21 of 22 by push time; not in the installed pair, the previous pair, or a green ref, and not covered by the newest-tags rule (keep newest 20)\n",
		imagevalues.DaemonImage, pruneTestName(2), pruneTestDigest(2))
	fmt.Fprintf(&want, "%s: keep 21, delete 1\n", imagevalues.DaemonImage)
	fmt.Fprintf(&want, "%s: keep 0, delete 0\n", imagevalues.PolecatImage)
	fmt.Fprintf(&want, "keep   %s:%s %s: one of the newest 20 tags\n", imagevalues.BdImage, pruneTestName(100), pruneTestDigest(100))
	fmt.Fprintf(&want, "%s: keep 1, delete 0\n", imagevalues.BdImage)
	fmt.Fprintf(&want, "total: keep 22, delete 1; nothing is deleted by this command\n")

	if out != want.String() {
		t.Fatalf("prune output:\n%s\nwant:\n%s", out, want.String())
	}
}

// TestUpdateImagesPruneRefusesAnUnwiredSeam: a prune with any seam unwired
// refuses before it reads anything, so it never prints a plan over a read it
// did not make.
func TestUpdateImagesPruneRefusesAnUnwiredSeam(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		unset func(s *updateImagesSeams)
	}{
		{"town lookup", func(s *updateImagesSeams) { s.FindTownRoot = nil }},
		{"registry", func(s *updateImagesSeams) { s.Registry = nil }},
		{"credentials", func(s *updateImagesSeams) { s.Credentials = nil }},
		{"HTTP client", func(s *updateImagesSeams) { s.HTTPClient = nil }},
		{"digest install record", func(s *updateImagesSeams) { s.PruneLedger = nil }},
		{"green refs reader", func(s *updateImagesSeams) { s.PruneGreen = nil }},
		{"retention planner", func(s *updateImagesSeams) { s.PlanPrune = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := pruneTestPopulated()
			seams := pruneTestSeams(reg)
			tc.unset(&seams)
			out, err := runUpdateImagesPruneCmd(t, seams)
			if err == nil {
				t.Fatal("prune exited zero with a seam unwired")
			}
			if out != "" || len(reg.methods) != 0 {
				t.Fatalf("prune read or printed with a seam unwired: %d requests, output %q", len(reg.methods), out)
			}
		})
	}
}

// TestUpdateImagesPruneRefusesAnUnreadableTownOrRegistry: a town the command
// cannot find, or a registry it cannot address, refuses before any read.
func TestUpdateImagesPruneRefusesAnUnreadableTownOrRegistry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(s *updateImagesSeams)
	}{
		{"town root not found", func(s *updateImagesSeams) {
			s.FindTownRoot = func() (string, error) { return "", errors.New("no town here") }
		}},
		{"registry not resolved", func(s *updateImagesSeams) {
			s.Registry = func(string) (imagevalues.Registry, error) {
				return imagevalues.Registry{}, errors.New("no registry rig")
			}
		}},
		{"registry owner unusable", func(s *updateImagesSeams) {
			s.Registry = func(string) (imagevalues.Registry, error) {
				return imagevalues.Registry{Scheme: "https", Host: "forgejo.test", Owner: "Bad Owner"}, nil
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := pruneTestPopulated()
			seams := pruneTestSeams(reg)
			tc.setup(&seams)
			out, err := runUpdateImagesPruneCmd(t, seams)
			if err == nil {
				t.Fatal("prune exited zero over an unreadable town or registry")
			}
			if out != "" || len(reg.methods) != 0 {
				t.Fatalf("prune read or printed over an unreadable town or registry: %d requests, output %q", len(reg.methods), out)
			}
		})
	}
}

// TestUpdateImagesPruneGreenRefsRefuseAnUnreadableRemote: a remote the green
// refs cannot be read from, or a viewer token that is not readable, refuses
// with no set of refs, since a set missing its refs would let a green image be
// listed for delete.
func TestUpdateImagesPruneGreenRefsRefuseAnUnreadableRemote(t *testing.T) {
	t.Parallel()
	absentToken := filepath.Join(t.TempDir(), "viewer-token")
	cases := []struct {
		name   string
		remote string
	}{
		{"remote is not a URL", "https://forgejo.test/\x7f/gastown"},
		{"remote has no scheme and host", ""},
		{"remote names no repository", "https://forgejo.test/onlyone"},
		{"viewer token unreadable", "https://forgejo.test/town/gastown.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refs, err := pruneGreenRefs(context.Background(), "gastown", tc.remote, absentToken)
			if err == nil {
				t.Fatal("green refs read from an unreadable remote or token")
			}
			if refs != nil {
				t.Fatalf("green refs returned with an error: %v", refs)
			}
		})
	}
}

// TestUpdateImagesPruneGreenRefsNeedAnImageRig: a town that names no image rig
// has no repository to read green refs from, and the read refuses.
func TestUpdateImagesPruneGreenRefsNeedAnImageRig(t *testing.T) {
	t.Parallel()
	refs, err := productionPruneGreen(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("green refs read from a town with no image rig")
	}
	if refs != nil {
		t.Fatalf("green refs returned with an error: %v", refs)
	}
}
