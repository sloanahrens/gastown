package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// skeletonRoot is the directory the archive extracts into, relative to /sandbox.
const skeletonRoot = "gt"

// redacted replaces every string the scrubber treats as a credential.
const redacted = "REDACTED"

// secretKey matches a settings key whose values are credentials, in any case.
var secretKey = regexp.MustCompile(`(?i)token|key|secret|password`)

// SkeletonSpec names the town to render and the sandbox-side values that
// replace the host's.
type SkeletonSpec struct {
	TownRoot         string // town root on the host
	Rig              string // rig directory name under the town root, e.g. "gastown"
	ControlPlaneHost string // host the sandbox reaches Dolt at, e.g. "host.docker.internal"
	SandboxRepoURL   string // rig Forgejo URL the sandbox pushes to and clones from
}

// RenderSkeleton returns the town skeleton a sandbox needs for gt and bd, as a
// gzipped tar with the top-level directory gt/.
func RenderSkeleton(s SkeletonSpec) ([]byte, error) {
	entries, err := skeletonEntries(s)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}
		if e.dir {
			hdr.Mode = 0o755
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("sandbox: write %s: %w", e.name, err)
		}
		if e.dir {
			continue
		}
		if _, err := tw.Write(e.data); err != nil {
			return nil, fmt.Errorf("sandbox: write %s: %w", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("sandbox: close archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("sandbox: close gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// ScrubSettings returns settings JSON with the top-level "secrets" key removed
// and every string under a credential-named key replaced with REDACTED.
func ScrubSettings(raw []byte) ([]byte, error) {
	doc, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("sandbox: scrub settings: %w", err)
	}
	return encodeJSON(scrubDocument(doc))
}

// scrubDocument drops the secrets block and scrubs every remaining value.
func scrubDocument(doc map[string]any) map[string]any {
	delete(doc, "secrets")
	for k, v := range doc {
		doc[k] = scrubValue(k, v, false)
	}
	return doc
}

// scrubValue redacts strings under a credential-named key, at any depth;
// after the first such key every string below it is a credential too.
func scrubValue(key string, v any, underSecret bool) any {
	secret := underSecret || secretKey.MatchString(key)
	switch val := v.(type) {
	case string:
		if secret {
			return redacted
		}
		return val
	case map[string]any:
		for k, child := range val {
			val[k] = scrubValue(k, child, secret)
		}
		return val
	case []any:
		for i, child := range val {
			val[i] = scrubValue("", child, secret)
		}
		return val
	default:
		return v
	}
}

func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode object: %w", err)
	}
	if doc == nil {
		return nil, errors.New("decode object: not a JSON object")
	}
	return doc, nil
}

func encodeJSON(v any) ([]byte, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode object: %w", err)
	}
	return append(out, '\n'), nil
}

// skeletonEntry is one archive member; a directory has no data.
type skeletonEntry struct {
	name string
	dir  bool
	data []byte
}

func skeletonEntries(s SkeletonSpec) ([]skeletonEntry, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	entries := []skeletonEntry{{name: skeletonRoot + "/", dir: true}}
	add := func(rel string, data []byte) {
		entries = append(entries, skeletonEntry{name: path.Join(skeletonRoot, rel), data: data})
	}

	// Copied verbatim: nothing in these files names the host.
	for _, rel := range []string{
		"mayor/town.json",
		".beads/routes.jsonl",
		s.Rig + "/metadata.json",
		s.Rig + "/mayor/rig/.beads/config.yaml",
	} {
		data, err := s.read(rel)
		if err != nil {
			return nil, err
		}
		add(rel, data)
	}

	// Both beads metadata files point at the control plane's Dolt server.
	for _, rel := range []string{".beads/metadata.json", s.Rig + "/mayor/rig/.beads/metadata.json"} {
		raw, err := s.read(rel)
		if err != nil {
			return nil, err
		}
		doc, err := decodeObject(raw)
		if err != nil {
			return nil, fmt.Errorf("sandbox: %s: %w", rel, err)
		}
		doc["dolt_server_host"] = s.ControlPlaneHost
		out, err := encodeJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("sandbox: %s: %w", rel, err)
		}
		add(rel, out)
	}

	townSettings, err := s.read("settings/config.json")
	if err != nil {
		return nil, err
	}
	scrubbed, err := ScrubSettings(townSettings)
	if err != nil {
		return nil, fmt.Errorf("sandbox: settings/config.json: %w", err)
	}
	add("settings/config.json", scrubbed)

	// The rig's settings are scrubbed and then pointed at the sandbox-side
	// remote; a rig without settings still gets the merge-queue remote, so a
	// push from inside the sandbox has a target.
	rigSettings := s.Rig + "/settings/config.json"
	raw, err := s.read(rigSettings)
	var rigDoc map[string]any
	switch {
	case errors.Is(err, fs.ErrNotExist):
		rigDoc = map[string]any{}
	case err != nil:
		return nil, err
	default:
		rigDoc, err = decodeObject(raw)
		if err != nil {
			return nil, fmt.Errorf("sandbox: %s: %w", rigSettings, err)
		}
	}
	setNested(scrubDocument(rigDoc), []string{"merge_queue", "forgejo", "remote_url"}, s.SandboxRepoURL)
	out, err := encodeJSON(rigDoc)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %s: %w", rigSettings, err)
	}
	add(rigSettings, out)

	for _, rel := range []string{"directives", s.Rig + "/directives"} {
		if err := s.addTree(&entries, rel); err != nil {
			return nil, err
		}
	}

	// The bare rig repo is written as files rather than by running git; gt
	// reads its remotes from it, and the sandbox only ever pushes.
	repo := s.Rig + "/.repo.git"
	for _, dir := range []string{"objects/info", "refs/heads", "refs/tags"} {
		entries = append(entries, skeletonEntry{name: path.Join(skeletonRoot, repo, dir) + "/", dir: true})
	}
	add(repo+"/HEAD", []byte("ref: refs/heads/main\n"))
	add(repo+"/config", []byte(bareRepoConfig(s.SandboxRepoURL)))

	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, nil
}

// read reads a slash-separated path under the town root.
func (s SkeletonSpec) read(rel string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.TownRoot, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("sandbox: read %s: %w", rel, err)
	}
	return data, nil
}

// addTree adds every regular file under rel, which may be absent.
func (s SkeletonSpec) addTree(entries *[]skeletonEntry, rel string) error {
	root := filepath.Join(s.TownRoot, filepath.FromSlash(rel))
	info, err := os.Stat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sandbox: read %s: %w", rel, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("sandbox: %s is not a directory", rel)
	}
	var files []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Read outside the walk callback: a read inside it is a symlink TOCTOU
	// that gosec's G122 rejects (gt-2929c).
	for _, p := range files {
		sub, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		*entries = append(*entries, skeletonEntry{
			name: path.Join(skeletonRoot, rel, filepath.ToSlash(sub)),
			data: data,
		})
	}
	return nil
}

// bareRepoConfig is the config a bare clone needs to resolve its origin.
func bareRepoConfig(originURL string) string {
	return "[core]\n" +
		"\trepositoryformatversion = 0\n" +
		"\tfilemode = true\n" +
		"\tbare = true\n" +
		"[remote \"origin\"]\n" +
		"\turl = " + originURL + "\n" +
		"\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
}

// setNested sets a string at a key path, creating the objects above it.
func setNested(doc map[string]any, keys []string, value string) {
	for _, k := range keys[:len(keys)-1] {
		child, ok := doc[k].(map[string]any)
		if !ok {
			child = map[string]any{}
			doc[k] = child
		}
		doc = child
	}
	doc[keys[len(keys)-1]] = value
}

func (s SkeletonSpec) validate() error {
	switch {
	case s.TownRoot == "":
		return errors.New("sandbox: skeleton spec has no TownRoot")
	case s.Rig == "" || s.Rig == "." || s.Rig == ".." || strings.ContainsAny(s.Rig, `/\`):
		return fmt.Errorf("sandbox: rig %q is not a single path element", s.Rig)
	case s.ControlPlaneHost == "" || strings.ContainsAny(s.ControlPlaneHost, " \t\r\n/\\"):
		return fmt.Errorf("sandbox: control-plane host %q is not a host name", s.ControlPlaneHost)
	case s.SandboxRepoURL == "" || strings.ContainsAny(s.SandboxRepoURL, "\r\n\x00"):
		return fmt.Errorf("sandbox: sandbox repo URL %q is not a git URL", s.SandboxRepoURL)
	}
	return nil
}
