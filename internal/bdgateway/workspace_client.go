package bdgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/townconfig"
)

// This file is the gt side of the gateway workspace: it builds the document
// PUT /v1/workspace takes from the town copy gt runs in, sends it with the
// caller's bearer token, and gives every writer of the town copy one
// nil-by-default pusher. The design is docs/design/bd-container.md, "The
// workspace" (option B), and the plan's G8.
//
// The tree is the P0 proof tree (workspace_proof_integration_test.go), which is
// the layout G7's store was written against: the gateway's /ws root mirrors the
// town root, so `.beads/routes.jsonl` and a rig's `<rig>/mayor/rig/.beads/` are
// the same relative paths on both sides. Only the two files that carry a host
// path are rewritten: the redirect, to the absolute `/ws/<rel>` form the proof
// showed a real bd following, and a routes path, to its town-relative form when
// it was absolute.

const (
	// WorkspacePath is the endpoint G6 mounts the workspace store on.
	WorkspacePath = "/v1/workspace"
	// WorkspaceURLEnv names the gateway's base URL for `gt bd workspace push`,
	// the same name gt-wrap's bdgateway target reads (G9).
	WorkspaceURLEnv = "BD_GATEWAY_URL"
	// WorkspaceTokenEnv names the file holding the pusher's bearer token, the
	// same name gt-wrap's bdgateway target reads (G9).
	WorkspaceTokenEnv = "BD_GATEWAY_TOKEN_FILE"
	// workspaceRootPath is the tree's own root inside the bd container. A
	// redirect pushed by gt must name where bd will see the target, never where
	// gt saw it, so the rewrite uses this prefix.
	workspaceRootPath = "/ws"
	// routesFileName is beads.RoutesFileName. It is repeated rather than
	// imported because this package's document builder is what internal/beads'
	// routes writer calls back into, and an import would be a cycle.
	routesFileName = "routes.jsonl"

	// pushTimeout bounds one push, both as the client's timeout and as the
	// deadline around the request, so a gateway that accepts and never answers
	// cannot hold a writer open.
	pushTimeout = 30 * time.Second
	// pushResponseLimit bounds the gateway's answer read for its manifest hash
	// or its refusal: one small JSON object either way.
	pushResponseLimit = 64 << 10
	// tokenFileLimit bounds the token file read. A gateway token is one short
	// line; a file past this is not one, and reading it whole would spool a
	// mistaken path into memory.
	tokenFileLimit = 4 << 10
)

// The pusher's named refusals. Callers match on the error rather than on
// message text; none carries a token or part of one.
var (
	// ErrNoGatewayURL is GatewayPusher.Push's refusal of a pusher with no URL:
	// there is nowhere to send the workspace.
	ErrNoGatewayURL = errors.New("bdgateway: no gateway url")
	// ErrNoTokenFile is GatewayPusher.Push's refusal of a pusher with no token
	// file: the gateway would answer 401 for every push.
	ErrNoTokenFile = errors.New("bdgateway: no token file")
	// ErrGatewayRefused is a push the gateway answered with a status other than
	// 200. The error names the gateway's code and message.
	ErrGatewayRefused = errors.New("bdgateway: the gateway refused the workspace")
	// ErrNoManifest is a 200 answer that does not name the manifest hash it
	// stored, so the caller cannot tell which tree the gateway serves.
	ErrNoManifest = errors.New("bdgateway: the gateway's answer names no manifest")
	// ErrManifestDiffers is a 200 answer whose hash is not the one the caller
	// sent: the gateway stored a tree other than the one pushed.
	ErrManifestDiffers = errors.New("bdgateway: the gateway stored a different manifest")
)

// HTTPDoer sends one request and returns its response. *http.Client is one; a
// test scripts one, so a unit test opens no socket (docs/testing.md,
// no-network).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// workspaceRoute is one line of routes.jsonl. It is beads.Route's wire form;
// see routesFileName for why it is not that type here.
type workspaceRoute struct {
	Prefix  string `json:"prefix"`
	Path    string `json:"path"`
	Backend string `json:"backend,omitempty"`
}

// BuildWorkspaceDocument reads the town copy under townRoot and returns the
// document PUT /v1/workspace takes: the town's routes.jsonl, each registered
// rig's redirect, and each rig's beads metadata files, with every path
// rewritten into the gateway's workspace form. It runs the document through the
// gateway's own validation and hash, so a document it returns is one the
// gateway accepts and carries the manifest the gateway computes.
//
// A town that cannot be read whole is refused rather than pushed in part: bd
// would route prefixes to a workspace holding no database for them, and the
// refusal names the rig or file at fault.
func BuildWorkspaceDocument(townRoot string) (WorkspaceDocument, error) {
	if townRoot == "" || !filepath.IsAbs(townRoot) {
		return WorkspaceDocument{}, fmt.Errorf("bdgateway: the town root %q must be an absolute path", townRoot)
	}
	town, err := townconfig.Load(townRoot)
	if err != nil {
		return WorkspaceDocument{}, fmt.Errorf("bdgateway: reading the town at %s: %w", townRoot, err)
	}
	files := map[string]string{}

	// The town's own beads directory: routes.jsonl and the hq metadata files.
	// bd's call with no rig reads exactly this tree.
	townBeads := filepath.Join(townRoot, ".beads")
	if err := addBeadsFiles(files, townBeads, ".beads", true); err != nil {
		return WorkspaceDocument{}, err
	}
	// The route table: one line per route, with every path bd resolves inside
	// the container made relative to the workspace root.
	routes, err := rewrittenRoutes(filepath.Join(townBeads, routesFileName), townRoot)
	if err != nil {
		return WorkspaceDocument{}, err
	}
	files[path.Join(".beads", routesFileName)] = routes

	for _, rig := range town.RigNames() {
		if err := addRigFiles(files, townRoot, rig); err != nil {
			return WorkspaceDocument{}, err
		}
	}

	doc := WorkspaceDocument{ManifestSHA: WorkspaceManifest(files), Files: files}
	// The gateway's rules decide what it accepts, so they decide what is sent:
	// a document that fails here would be refused after a round trip.
	if _, err := validatedManifest(doc); err != nil {
		return WorkspaceDocument{}, err
	}
	return doc, nil
}

// addRigFiles adds one rig's redirect and beads metadata files. The rig's beads
// directory is the one gt itself reads: the redirect's target when the rig has
// a redirect, otherwise the rig's own .beads. The files keep the path they have
// in the town (the workspace mirrors the town), and the redirect is rewritten to
// the absolute path bd will resolve inside the container.
func addRigFiles(files map[string]string, townRoot, rig string) error {
	// The registry is the town's file, but its names reach the filesystem and
	// the document's paths: one path element, or the read and the key it makes
	// would land outside the town.
	if !filepath.IsLocal(rig) || strings.ContainsRune(rig, filepath.Separator) {
		return fmt.Errorf("bdgateway: rig %q is not one path element", shown(rig))
	}
	rigDir := filepath.Join(townRoot, rig)
	redirectPath := filepath.Join(rigDir, ".beads", "redirect")
	data, err := os.ReadFile(redirectPath) //nolint:gosec // G304: path under the town root
	switch {
	case errors.Is(err, os.ErrNotExist):
		beadsDir := filepath.Join(rigDir, ".beads")
		if !holdsBeadsMetadata(beadsDir) {
			return fmt.Errorf("bdgateway: rig %q has neither a redirect nor a beads metadata.json under %s", rig, filepath.Join(rig, ".beads"))
		}
		return addBeadsFiles(files, beadsDir, path.Join(rig, ".beads"), false)
	case err != nil:
		return fmt.Errorf("bdgateway: reading rig %q's redirect: %w", rig, err)
	}
	target, err := redirectTarget(townRoot, rig, string(data))
	if err != nil {
		return err
	}
	files[path.Join(rig, ".beads", "redirect")] = workspaceRootPath + "/" + target + "\n"
	return addBeadsFiles(files, filepath.Join(townRoot, filepath.FromSlash(target)), target, false)
}

// redirectTarget resolves a redirect file's content to the town-relative slash
// path it names, the form the document keys use. A relative redirect is
// resolved from the rig directory, as gt resolves it (internal/beads,
// ResolveBeadsDir); an absolute one must be inside the town. Anything that
// leaves the town, or does not name a .beads directory, is refused: the
// workspace holds no tree outside the town, and bd reads no other file name.
func redirectTarget(townRoot, rig, content string) (string, error) {
	raw := strings.TrimSpace(content)
	if raw == "" {
		return "", fmt.Errorf("bdgateway: rig %q's redirect is empty", rig)
	}
	var resolved string
	if filepath.IsAbs(raw) {
		resolved = filepath.Clean(raw)
	} else {
		resolved = filepath.Join(townRoot, rig, filepath.FromSlash(raw))
	}
	rel, err := townRelative(townRoot, resolved)
	if err != nil {
		return "", fmt.Errorf("bdgateway: rig %q's redirect %q: %w", rig, shown(raw), err)
	}
	if path.Base(rel) != ".beads" {
		return "", fmt.Errorf("bdgateway: rig %q's redirect %q does not name a .beads directory", rig, shown(raw))
	}
	return rel, nil
}

// addBeadsFiles adds the metadata files a beads directory holds, keyed under
// prefix. metadata.json is required when requireMetadata is set — it is the file
// that names bd's database — and optional otherwise, while config.yaml and
// dolt-server.port are always optional: a rig may have no config, and a town
// with no Dolt endpoint on disk has no port file (the design removes it then).
func addBeadsFiles(files map[string]string, beadsDir, prefix string, requireMetadata bool) error {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	metadata, err := os.ReadFile(metadataPath) //nolint:gosec // G304: path under the town root
	switch {
	case errors.Is(err, os.ErrNotExist):
		if requireMetadata {
			return fmt.Errorf("bdgateway: %s holds no metadata.json, so bd has no database to open", beadsDir)
		}
		return nil
	case err != nil:
		return fmt.Errorf("bdgateway: reading %s: %w", metadataPath, err)
	}
	// bd's own bytes: the gateway hashes and writes them unchanged. gt writes
	// database settings into metadata.json, no path, so the paths the design
	// rewrites are the redirect's and the route's.
	files[path.Join(prefix, "metadata.json")] = string(metadata)
	for _, name := range []string{"config.yaml", "dolt-server.port"} {
		data, err := os.ReadFile(filepath.Join(beadsDir, name)) //nolint:gosec // G304: path under the town root
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("bdgateway: reading %s: %w", filepath.Join(beadsDir, name), err)
		}
		files[path.Join(prefix, name)] = string(data)
	}
	return nil
}

// holdsBeadsMetadata reports whether dir holds the metadata.json that makes it
// bd's workspace for the rig.
func holdsBeadsMetadata(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "metadata.json"))
	return err == nil && info.Mode().IsRegular()
}

// rewrittenRoutes reads a town's routes.jsonl and returns it in the workspace
// form: one JSON object per route, in the file's order, with each path made
// town-relative. A path the town copy already expresses relative to the town
// root is unchanged, because the workspace mirrors the town root; an absolute
// path inside the town — which bd resolves but the container never sees at that
// location — is rewritten. Blank lines and comments are dropped, since bd reads
// only route lines; a line that is neither is refused rather than dropped, so a
// route gt cannot read does not silently vanish from the workspace.
func rewrittenRoutes(routesPath, townRoot string) (string, error) {
	data, err := os.ReadFile(routesPath) //nolint:gosec // G304: path under the town root
	if err != nil {
		return "", fmt.Errorf("bdgateway: reading %s: %w", routesPath, err)
	}
	var out strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var route workspaceRoute
		if err := json.Unmarshal([]byte(line), &route); err != nil {
			return "", fmt.Errorf("bdgateway: %s holds a line that is not a route: %s: %w", routesPath, shown(line), err)
		}
		if route.Prefix == "" || route.Path == "" {
			return "", fmt.Errorf("bdgateway: %s holds a route with no prefix or path: %s", routesPath, shown(line))
		}
		route.Path, err = workspaceRoutePath(townRoot, route.Path)
		if err != nil {
			return "", fmt.Errorf("bdgateway: route %q in %s: %w", shown(route.Prefix), routesPath, err)
		}
		// A route is three strings, so marshaling it cannot fail.
		encoded, _ := json.Marshal(route)
		out.Write(encoded)
		out.WriteByte('\n')
	}
	return out.String(), nil
}

// workspaceRoutePath rewrites one route path into the town-relative slash form
// the workspace uses. An absolute path must be inside the town; a relative one
// is cleaned and must not climb out of it. The town's own route is "." and stays
// so, because /ws/.beads is the town's beads directory.
func workspaceRoutePath(townRoot, routePath string) (string, error) {
	if filepath.IsAbs(routePath) {
		return townRelative(townRoot, filepath.Clean(routePath))
	}
	cleaned := path.Clean(filepath.ToSlash(routePath))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return "", fmt.Errorf("path %q leaves the town", shown(routePath))
	}
	return cleaned, nil
}

// townRelative returns p relative to townRoot in slash form, or an error when p
// is not inside the town. A path naming the town root itself is "." — the form
// bd's own route for the town's beads uses.
func townRelative(townRoot, p string) (string, error) {
	root, clean := filepath.Clean(townRoot), filepath.Clean(p)
	if clean == root {
		return ".", nil
	}
	rel, inside := strings.CutPrefix(clean, root+string(filepath.Separator))
	if !inside || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the town", shown(p))
	}
	return filepath.ToSlash(rel), nil
}

// WorkspacePusher is what a writer of the town copy calls after it changes the
// files the gateway serves (EnsureMetadata, WriteRoutes), so the bd container
// sees a route flip or a rig's new metadata at once. Push rebuilds the whole
// document from townRoot and returns the manifest hash the gateway stored.
// docs/design/bd-container.md, "The workspace".
type WorkspacePusher interface {
	Push(ctx context.Context, townRoot string) (string, error)
}

// pusher is the installed pusher, nil until a town with a gateway installs one
// at daemon start (G8b): every test and every Mac-only path then runs exactly as
// before.
var pusher WorkspacePusher

// PushWorkspace pushes the town's workspace through the installed pusher and
// returns the manifest hash the gateway stored. With no pusher installed it is
// a no-op that succeeds with no hash, which is the state of every test and of a
// town without a gateway: the files were written, and there is nothing to send
// them to.
func PushWorkspace(ctx context.Context, townRoot string) (string, error) {
	if pusher == nil {
		return "", nil
	}
	return pusher.Push(ctx, townRoot)
}

// GatewayPusher is the WorkspacePusher that sends the document to a running bd
// gateway with its bearer token.
type GatewayPusher struct {
	// URL is the gateway's base URL, such as http://host.docker.internal:7440.
	URL string
	// TokenFile is the file holding the pusher's bearer token. It is read on
	// every push, so a rotated token needs no restart.
	TokenFile string
	// HTTP sends the request; nil uses an http.Client bounded by pushTimeout.
	HTTP HTTPDoer
}

// Push builds the town's document and sends it with PUT /v1/workspace. It
// returns the manifest hash the gateway answered with, which must be the one the
// document carried: a gateway that stored another tree says so rather than
// letting the caller believe its push landed.
func (g GatewayPusher) Push(ctx context.Context, townRoot string) (string, error) {
	doc, err := BuildWorkspaceDocument(townRoot)
	if err != nil {
		return "", err
	}
	endpoint, err := g.endpoint()
	if err != nil {
		return "", err
	}
	token, err := readTokenFile(g.TokenFile)
	if err != nil {
		return "", err
	}
	// A document holds strings and a hash, so marshaling it cannot fail.
	body, _ := json.Marshal(doc)
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	// endpoint parsed and checked the URL and the method is fixed, so building
	// the request cannot fail; a branch for an error that cannot happen would
	// be dead code (gtwrap's bdgateway target builds its request the same way).
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.doer().Do(req)
	if err != nil {
		return "", fmt.Errorf("bdgateway: the gateway at %s did not answer, so the workspace was not pushed: %w", shown(g.URL), err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, pushResponseLimit))
	if err != nil {
		return "", fmt.Errorf("bdgateway: reading the gateway's answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", g.refusal(resp.StatusCode, data)
	}
	var result workspacePushResult
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("%w: the body is not a workspace answer: %w", ErrNoManifest, err)
	}
	if result.ManifestSHA == "" {
		return "", fmt.Errorf("%w: the body carries no manifest_sha", ErrNoManifest)
	}
	if result.ManifestSHA != doc.ManifestSHA {
		return "", fmt.Errorf("%w: sent %s, the gateway stored %s", ErrManifestDiffers, doc.ManifestSHA, shown(result.ManifestSHA))
	}
	return result.ManifestSHA, nil
}

// endpoint is the URL the push goes to, refusing a URL that is not http(s) with
// a host, or that carries a query or fragment the endpoint does not take.
func (g GatewayPusher) endpoint() (string, error) {
	if g.URL == "" {
		return "", fmt.Errorf("%w: set --gateway-url or %s", ErrNoGatewayURL, WorkspaceURLEnv)
	}
	u, err := url.Parse(g.URL)
	if err != nil {
		return "", fmt.Errorf("bdgateway: the gateway url %q does not parse: %w", shown(g.URL), err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("bdgateway: the gateway url %q is not an http or https URL with a host", shown(g.URL))
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("bdgateway: the gateway url %q carries a query or fragment, which the workspace endpoint does not take", shown(g.URL))
	}
	return strings.TrimSuffix(g.URL, "/") + WorkspacePath, nil
}

// doer is the HTTP client a push uses: the injected one, or the real client
// bounded by pushTimeout when a caller named none.
func (g GatewayPusher) doer() HTTPDoer {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: pushTimeout}
}

// refusal turns a non-200 answer into the one-line error the caller sees. The
// gateway's JSON code and message are quoted so the operator learns why; a body
// that is not that shape is named by its status alone, since captured output can
// hold anything.
func (g GatewayPusher) refusal(status int, data []byte) error {
	var payload ErrorPayload
	if err := json.Unmarshal(data, &payload); err == nil && payload.Code != "" {
		return fmt.Errorf("%w with HTTP %d: %s: %s", ErrGatewayRefused, status, payload.Code, payload.Message)
	}
	return fmt.Errorf("%w with HTTP %d, so the workspace was not stored", ErrGatewayRefused, status)
}

// readTokenFile reads the bearer token from path: the file's content without
// surrounding whitespace, refused when it is empty or is not one short line. The
// token never reaches an error or a log.
func readTokenFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%w: set --token-file or %s", ErrNoTokenFile, WorkspaceTokenEnv)
	}
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the token file
	if err != nil {
		return "", fmt.Errorf("bdgateway: opening the token file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, tokenFileLimit+1))
	if err != nil {
		return "", fmt.Errorf("bdgateway: reading the token file %s: %w", path, err)
	}
	if len(data) > tokenFileLimit {
		return "", fmt.Errorf("bdgateway: the token file %s is over %d bytes, so it does not hold one token", path, tokenFileLimit)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("%w: %s", ErrEmptyToken, path)
	}
	return token, nil
}
