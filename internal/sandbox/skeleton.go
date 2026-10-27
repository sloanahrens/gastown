package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/config"
)

// skeletonRoot is the directory the archive extracts into, relative to /sandbox.
const skeletonRoot = "gt"

// redacted replaces every value the scrubber treats as a credential.
const redacted = "REDACTED"

// credentialKeyWords name a credential on their own, so a key carrying one of
// them holds credentials at any depth. The set is wider than the plan's four
// names because a scrubbed skeleton must not leak a credential whose key says
// "credential" or "bearer" instead of "token".
var credentialKeyWords = map[string]bool{
	"auth":          true,
	"authorization": true,
	"bearer":        true,
	"cookie":        true,
	"cookies":       true,
	"credential":    true,
	"credentials":   true,
	"dsn":           true,
	"jwt":           true,
	"key":           true,
	"keys":          true,
	"oauth":         true,
	"passphrase":    true,
	"passwd":        true,
	"password":      true,
	"secret":        true,
	"secrets":       true,
	"signature":     true,
	"token":         true,
	"tokens":        true,
}

// credentialKeySuffixes catch a run-together key ("apikey", "authtoken") whose
// words never got separated.
var credentialKeySuffixes = []string{"token", "key", "secret", "password"}

// Value shapes that are credentials whatever key carries them. This is defense
// in depth under the key rules, not a replacement for them: a value that is not
// shaped like a known credential and sits under an innocent-looking key is
// caught only by the key rules (gt-2929c).
var (
	userInfoURL   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://[^/@\s]+@`)
	userInfoPart  = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)
	jwtShape      = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*$`)
	knownKeyShape = regexp.MustCompile(`^(?:sk-[A-Za-z0-9_-]{16,}|sk_[A-Za-z0-9_]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|dckr_pat_[A-Za-z0-9_-]{20,}|hvs\.[A-Za-z0-9_-]{20,})$`)
	pemKeyShape   = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	bareToken     = regexp.MustCompile(`^[A-Za-z0-9_+=-]{32,}$`)
	hexToken      = regexp.MustCompile(`^[0-9a-f]{32,}$`)
	uuidShape     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Text scrubbing patterns. The assignment pattern keeps the key and replaces
// the value, so a YAML or INI file stays readable after scrubbing.
var (
	textUserInfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]+@`)
	textBearer   = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	textAssign   = regexp.MustCompile(`^(\s*(?:-\s+)?["']?([A-Za-z0-9_.-]+)["']?\s*[:=]\s*)(\S.*)$`)
	textToken    = regexp.MustCompile(`[A-Za-z0-9_+=-]{32,}`)
)

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
			if path.Base(strings.TrimSuffix(e.name, "/")) == ".beads" {
				hdr.Mode = 0o700 // bd warns on every call about a looser .beads
			}
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

// ScrubSettings returns settings JSON with every credential block removed and
// every value under a credential-named key replaced with REDACTED.
func ScrubSettings(raw []byte) ([]byte, error) {
	doc, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("sandbox: scrub settings: %w", err)
	}
	return encodeJSON(scrubDocument(doc))
}

// scrubDocument drops the credential blocks at the top level and scrubs every
// remaining value. A top-level block is dropped whole because its contents are
// credentials by construction, so keeping redacted placeholders only invites a
// caller to think the block is still usable.
func scrubDocument(doc map[string]any) map[string]any {
	for _, k := range []string{"secrets", "secret", "credentials", "credential"} {
		delete(doc, k)
	}
	for k, v := range doc {
		doc[k] = scrubValue(k, v, false)
	}
	return doc
}

// scrubValue redacts every value under a credential-named key, at any depth;
// after the first such key everything below it is a credential too. Values not
// under one are redacted only when they are shaped like a credential.
func scrubValue(key string, v any, underSecret bool) any {
	// A price per billion tokens is a number gt must read back as a number;
	// "tokens" in its name is not a credential. A string here is still redacted.
	if n, ok := v.(json.Number); ok && key == "usd_per_billion_tokens" && !underSecret {
		return n
	}
	secret := underSecret || credentialName(key)
	switch val := v.(type) {
	case string:
		if secret {
			return redacted
		}
		if stripped := stripUserInfo(val); stripped != val {
			return stripped
		}
		if looksLikeCredential(val) {
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
		// A number or a bool under a credential key is key material or a
		// switch somebody believed hid it; neither belongs in the skeleton.
		if secret {
			return redacted
		}
		return v
	}
}

// credentialName reports whether a settings key names a credential, however it
// separates its words: api_token, APIKey, db.password, private-key, apikey.
// Every marker config.IsSecretKey matches is a credential word here too; the
// parity test in skeleton_test.go holds that, without substring matches.
func credentialName(key string) bool {
	words := keyWords(key)
	for _, w := range words {
		if credentialKeyWords[w] {
			return true
		}
	}
	runTogether := strings.Join(words, "")
	for _, suffix := range credentialKeySuffixes {
		if strings.HasSuffix(runTogether, suffix) {
			return true
		}
	}
	return false
}

// keyWords splits a key on every non-alphanumeric rune and at camelCase
// boundaries, returning lower-case words.
func keyWords(key string) []string {
	var words []string
	chunks := strings.FieldsFunc(key, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, chunk := range chunks {
		runes := []rune(chunk)
		start := 0
		for i := 1; i < len(runes); i++ {
			prev, cur := runes[i-1], runes[i]
			boundary := unicode.IsLower(prev) && unicode.IsUpper(cur)
			if !boundary && unicode.IsUpper(prev) && unicode.IsUpper(cur) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
				boundary = true
			}
			if boundary {
				words = append(words, strings.ToLower(string(runes[start:i])))
				start = i
			}
		}
		words = append(words, strings.ToLower(string(runes[start:])))
	}
	return words
}

// stripUserInfo removes the credentials from a URL and keeps the target, so a
// copied registry URL still points at the right host without the password.
func stripUserInfo(s string) string {
	if !userInfoURL.MatchString(s) {
		return s
	}
	return userInfoPart.ReplaceAllString(s, "${1}"+redacted+"@")
}

// looksLikeCredential reports a value shaped like a credential, whatever key
// carries it.
func looksLikeCredential(s string) bool {
	if userInfoURL.MatchString(s) || jwtShape.MatchString(s) || knownKeyShape.MatchString(s) || pemKeyShape.MatchString(s) {
		return true
	}
	if hexToken.MatchString(s) {
		return true
	}
	if !bareToken.MatchString(s) {
		return false
	}
	var upper, lower, digits int
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper++
		case r >= 'a' && r <= 'z':
			lower++
		case r >= '0' && r <= '9':
			digits++
		}
	}
	if upper > 0 && lower > 0 && digits > 0 {
		return true
	}
	// An all-lower-case key: many digits are the giveaway that a hyphenated
	// slug or a host name is not one.
	return upper == 0 && lower > 0 && digits >= 8
}

// scrubText redacts credentials from a plain-text file: URL userinfo, bearer
// tokens, the value of a credential-named assignment, and bare tokens.
func scrubText(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		lines[i] = scrubTextLine(line)
	}
	return []byte(strings.Join(lines, "\n"))
}

// scrubTextLine scrubs one line; the assignment rewrite keeps the key so a
// scrubbed file still shows which credential it used to carry.
func scrubTextLine(line string) string {
	line = textUserInfo.ReplaceAllString(line, "${1}"+redacted+"@")
	line = textBearer.ReplaceAllString(line, "${1}"+redacted)
	if m := textAssign.FindStringSubmatch(line); m != nil && credentialName(m[2]) {
		return m[1] + redacted
	}
	return textToken.ReplaceAllStringFunc(line, func(word string) string {
		if looksLikeCredential(word) {
			return redacted
		}
		return word
	})
}

// sanitizeFile returns the bytes to ship for a file read from the host town.
// Every archive member that is not generated goes through here, so no file is
// copied verbatim (gt-2929c): JSON and JSONL documents are decoded and scrubbed,
// and every other file is scrubbed as text.
func sanitizeFile(rel string, data []byte) ([]byte, error) {
	switch path.Ext(rel) {
	case ".json":
		doc, err := decodeObject(data)
		if err != nil {
			return nil, err
		}
		return encodeJSON(scrubDocument(doc))
	case ".jsonl":
		return scrubJSONLines(data)
	default:
		return scrubText(data), nil
	}
}

// scrubJSONLines scrubs each object of a JSON-lines file, one line per object.
func scrubJSONLines(data []byte) ([]byte, error) {
	body := strings.TrimSuffix(string(data), "\n")
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		doc, err := decodeObject([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		out, err := json.Marshal(scrubDocument(doc))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		lines[i] = string(out)
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode object: %w", err)
	}
	// Trailing content means the file is not the document it looks like; taking
	// the first object would ship the rest of it unscanned.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode object: trailing data")
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

// townReader reads the host town through an os.Root, so a path that is or
// becomes a symlink pointing out of the town fails the read instead of shipping
// a host file to the sandbox (gt-2929c).
type townReader struct {
	root *os.Root
}

func (r townReader) read(rel string) ([]byte, error) {
	data, err := r.root.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("sandbox: read %s: %w", rel, err)
	}
	return data, nil
}

func (r townReader) readObject(rel string) (map[string]any, error) {
	raw, err := r.read(rel)
	if err != nil {
		return nil, err
	}
	doc, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %s: %w", rel, err)
	}
	return doc, nil
}

// rigBeadsConfig returns the town-relative path and the contents of the rig's
// beads config.yaml: the first of the rig's beads directories that holds one.
func rigBeadsConfig(town townReader, rig string) (string, []byte, error) {
	for _, dir := range config.RigBeadsDirs(rig) {
		rel := dir + "/config.yaml"
		if data, err := town.read(rel); err == nil {
			return rel, data, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("sandbox: rig %s has no beads config.yaml in %s", rig, strings.Join(config.RigBeadsDirs(rig), " or "))
}

func skeletonEntries(s SkeletonSpec) ([]skeletonEntry, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.TownRoot)
	if err != nil {
		return nil, fmt.Errorf("sandbox: open town root %s: %w", s.TownRoot, err)
	}
	defer func() { _ = root.Close() }()
	town := townReader{root: root}

	// The beads directories are listed so they extract 0700; left implicit,
	// tar makes them 0755 and bd warns on every call.
	entries := []skeletonEntry{
		{name: skeletonRoot + "/", dir: true},
		{name: path.Join(skeletonRoot, ".beads") + "/", dir: true},
		{name: path.Join(skeletonRoot, s.Rig, ".beads") + "/", dir: true},
		{name: path.Join(skeletonRoot, s.Rig, "mayor/rig/.beads") + "/", dir: true},
	}
	add := func(rel string, data []byte) {
		entries = append(entries, skeletonEntry{name: path.Join(skeletonRoot, rel), data: data})
	}

	// The rig's .beads holds only a pointer at the rig's shared beads, as on the
	// host. Without it gt prime, which re-runs the redirect setup for the
	// polecat's worktree, points the worktree at this otherwise empty directory
	// and bd finds no workspace. The pointer names the rig's tracked workspace,
	// the first of the directories gastown looks in on the host.
	workspace, err := filepath.Rel(s.Rig, filepath.FromSlash(config.RigBeadsDirs(s.Rig)[0]))
	if err != nil {
		return nil, fmt.Errorf("sandbox: rig redirect target for %s: %w", s.Rig, err)
	}
	add(s.Rig+"/.beads/redirect", []byte(filepath.ToSlash(workspace)+"\n"))

	// gt reads these files as they are; sanitizeFile is what keeps a token or a
	// credential-carrying URL out of them. The rig's metadata.json is optional:
	// some rigs keep theirs under .beads/, and host polecats run without it.
	rigMetadata := s.Rig + "/metadata.json"
	for _, rel := range []string{
		"mayor/town.json",
		".beads/routes.jsonl",
		rigMetadata,
	} {
		raw, err := town.read(rel)
		if errors.Is(err, fs.ErrNotExist) && rel == rigMetadata {
			continue
		}
		if err != nil {
			return nil, err
		}
		data, err := sanitizeFile(rel, raw)
		if err != nil {
			return nil, fmt.Errorf("sandbox: %s: %w", rel, err)
		}
		add(rel, data)
	}

	// The rig's beads config sits in the tracked workspace or, for a rig whose
	// mayor/rig/.beads holds only metadata.json and a redirect (devops, fractals,
	// beaver, agentlab), in <rig>/.beads. Either way it lands at the tracked
	// workspace, which is where the sandbox's bd looks; a rig with neither stops
	// the render, since bd would run with no prefix (gt-z7ut1).
	configRel, rawConfig, err := rigBeadsConfig(town, s.Rig)
	if err != nil {
		return nil, err
	}
	beadsConfig, err := sanitizeFile(configRel, rawConfig)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %s: %w", configRel, err)
	}
	add(s.Rig+"/mayor/rig/.beads/config.yaml", beadsConfig)

	// Both beads metadata files point at the control plane's Dolt server.
	for _, rel := range []string{".beads/metadata.json", s.Rig + "/mayor/rig/.beads/metadata.json"} {
		doc, err := town.readObject(rel)
		if err != nil {
			return nil, err
		}
		// bd compares project_id with the database's and refuses to connect on
		// a mismatch. It is an identity, not a credential, but its UUID shape
		// reads as a token, so keep the original string. Only a UUID is kept:
		// any other string under that key could be a credential.
		projectID, hasProjectID := doc["project_id"].(string)
		doc = scrubDocument(doc)
		if hasProjectID {
			if !uuidShape.MatchString(projectID) {
				return nil, fmt.Errorf("sandbox: %s: project_id is not a UUID", rel)
			}
			doc["project_id"] = projectID
		}
		doc["dolt_server_host"] = s.ControlPlaneHost
		out, err := encodeJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("sandbox: %s: %w", rel, err)
		}
		add(rel, out)
	}

	townSettings, err := town.read("settings/config.json")
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
	raw, err := town.read(rigSettings)
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

	for _, rel := range config.DirectiveTreeRels(s.Rig) {
		if err := town.addTree(&entries, rel); err != nil {
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

// addTree adds every regular file under rel, which may be absent. A symlinked
// or non-directory rel is an error, and a symlink under it is skipped: without
// following it the renderer cannot tell a file from a directory, and following
// it would ship a target the town layout never named (gt-2929c).
func (r townReader) addTree(entries *[]skeletonEntry, rel string) error {
	info, err := r.root.Lstat(filepath.FromSlash(rel))
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
	err = fs.WalkDir(r.root.FS(), rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox: walk %s: %w", rel, err)
	}
	// Read through the root, not the walk's path: the root refuses a path that
	// has become a symlink to somewhere outside the town.
	for _, p := range files {
		raw, err := r.read(p)
		if err != nil {
			return err
		}
		data, err := sanitizeFile(p, raw)
		if err != nil {
			return fmt.Errorf("sandbox: %s: %w", p, err)
		}
		*entries = append(*entries, skeletonEntry{
			name: path.Join(skeletonRoot, rel, strings.TrimPrefix(p, rel+"/")),
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
