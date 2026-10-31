package dashboard

// The nightly Forgejo size report: one line per repository per run, the record
// the monthly export charges storage from (gt-zg3qu). Forgejo reports a
// repository's size in kilobytes on the same repository list the panel already
// reads, so the report needs no separate size endpoint.
//
// It has no scheduler yet: its caller is the bead that runs it nightly beside
// the tracker size job (gt-oujm5).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

const (
	// sizeTSLayout is the report's timestamp: UTC, second precision, trailing Z.
	sizeTSLayout = "2006-01-02T15:04:05Z"
	// sizeDayLayout is the day a file holds, in UTC. A day file is one run's
	// snapshot of the town, so a rerun replaces it rather than adding a second
	// set of lines to a record the monthly export charges from.
	sizeDayLayout = "2006-01-02"
)

// ForgejoSizeRow is one line of the nightly Forgejo size report.
type ForgejoSizeRow struct {
	// TS is when the row was written, in UTC.
	TS string `json:"ts"`
	// Workspace is the workspace the repository's rig belongs to
	// (internal/workspaceid).
	Workspace string `json:"workspace"`
	// Repo is the repository's owner/name.
	Repo string `json:"repo"`
	// SizeKB is the size Forgejo reported, and nil when the reply carried none:
	// an unmeasured size is not a zero-byte repository.
	SizeKB *int64 `json:"size_kb"`
}

// ForgejoSizeWriter writes the nightly size report to
// <town>/.runtime/metrics/forgejo-sizes/<UTC date>.jsonl.
type ForgejoSizeWriter struct {
	api      forgejoFeeds
	townRoot string
	now      func() time.Time
	// openTemp opens the run's temporary file exclusively. It is a field so a
	// test drives the write and close failures a real disk will not offer on
	// demand.
	openTemp func(path string) (io.WriteCloser, error)
	// forRig is the workspace map's lookup for a rig prefix. It is a field so a
	// test drives a mapping failure the real map cannot produce: its only error
	// is the empty prefix, which workspaceByRig skips rather than reports.
	forRig func(ids *workspaceid.Map, prefix string) (string, error)
}

// NewForgejoSizeWriter builds a writer over api for the town at townRoot.
func NewForgejoSizeWriter(api forgejoFeeds, townRoot string) *ForgejoSizeWriter {
	return &ForgejoSizeWriter{
		api:      api,
		townRoot: townRoot,
		now:      time.Now,
		openTemp: openTempFile,
		forRig:   rigWorkspace,
	}
}

// rigWorkspace is the workspace map's answer for a rig prefix.
func rigWorkspace(ids *workspaceid.Map, prefix string) (string, error) {
	return ids.For(prefix)
}

// openTempFile creates path exclusively, so a second writer racing this one
// fails instead of interleaving. writeRows clears a dead run's leftover first.
func openTempFile(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // G302: the report is non-sensitive operational data
}

// ForgejoSizesDir is <town>/.runtime/metrics/forgejo-sizes, the directory the
// nightly report's day files live in.
func ForgejoSizesDir(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "metrics", "forgejo-sizes")
}

// Write reads the viewer's repositories and writes one line per repository to
// the day's file. The day file is one run's snapshot: a run replaces whatever
// the day held, so a rerun of a failed night produces a single set of lines,
// never a second set the monthly export would charge twice.
//
// A failed read or mapping returns an error before anything is written, and
// the day file keeps what it held. A failed write or close returns an error and
// leaves no day file from that run: the rows are written to a temporary file
// beside it and renamed over the day file only once every row is on disk.
func (w *ForgejoSizeWriter) Write(ctx context.Context) ([]ForgejoSizeRow, error) {
	if w.townRoot == "" {
		return nil, fmt.Errorf("forgejo sizes: no town root to write under")
	}
	repos, err := w.api.ListUserRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("forgejo sizes: listing repositories: %w", err)
	}
	byRepo, err := w.workspaceByRepo()
	if err != nil {
		return nil, err
	}
	now := w.now().UTC()
	rows := make([]ForgejoSizeRow, 0, len(repos))
	for _, repo := range repos {
		workspace, err := repoWorkspace(byRepo, repo.FullName)
		if err != nil {
			return nil, err
		}
		rows = append(rows, ForgejoSizeRow{
			TS:        now.Format(sizeTSLayout),
			Workspace: workspace,
			Repo:      repo.FullName,
			SizeKB:    repo.SizeKB,
		})
	}
	if err := w.writeRows(rows, now); err != nil {
		return nil, err
	}
	return rows, nil
}

// workspaceByRepo maps each registered rig's repository, as owner/name, to the
// rig's workspace id: the rig's beads prefix from the town registry gives the
// workspace (internal/workspaceid), and the registry's git_url gives the
// repository the rig lives in. Keying by owner and name keeps a repository of
// the same name under another owner, which the viewer can also see, from being
// billed to this town's workspace. A rig registered without a beads prefix, or
// without a git_url that names an owner and a repository, is left out, so its
// repository is named as unmapped rather than charged to the town's default
// workspace, which belongs to another customer's bill.
func (w *ForgejoSizeWriter) workspaceByRepo() (map[string]string, error) {
	rigsPath := filepath.Join(w.townRoot, "mayor", "rigs.json")
	rigs, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		return nil, fmt.Errorf("forgejo sizes: reading the rig registry: %w", err)
	}
	ids, err := workspaceid.Load(w.townRoot)
	if err != nil {
		return nil, fmt.Errorf("forgejo sizes: %w", err)
	}
	out := make(map[string]string, len(rigs.Rigs))
	owner := make(map[string]string, len(rigs.Rigs))
	for name, entry := range rigs.Rigs {
		var prefix string
		if entry.BeadsConfig != nil {
			prefix = strings.TrimSuffix(entry.BeadsConfig.Prefix, "-")
		}
		// For answers ErrEmptyRigPrefix for a rig the registry holds without a
		// beads prefix: it names no workspace, so that rig alone is left out of
		// the map and its repository is refused by name in repoWorkspace. Any
		// other mapping error is the run's failure, never a rig quietly dropped
		// from the bill.
		id, err := w.forRig(ids, prefix)
		if err != nil {
			if errors.Is(err, workspaceid.ErrEmptyRigPrefix) {
				continue
			}
			return nil, fmt.Errorf("forgejo sizes: mapping rig %s: %w", name, err)
		}
		key, ok := repoKey(entry.GitURL)
		if !ok {
			continue
		}
		if other, dup := owner[key]; dup {
			return nil, fmt.Errorf("forgejo sizes: rigs %s and %s both name repository %s", other, name, strconv.Quote(key))
		}
		owner[key] = name
		out[key] = id
	}
	return out, nil
}

// repoKey is the lower-cased owner/name a git URL names: the last two path
// segments, with a trailing .git dropped. It reads an http(s) URL and the
// scp-like git@host:owner/name.git form. ok is false when the URL names no owner
// and repository. Forgejo treats owner and repository names case-insensitively.
func repoKey(gitURL string) (string, bool) {
	path := gitURL
	if u, err := url.Parse(gitURL); err == nil && u.Scheme != "" && u.Host != "" {
		path = u.Path
	} else if _, after, found := strings.Cut(gitURL, ":"); found {
		path = after
	}
	var segs []string
	for _, s := range strings.Split(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) < 2 {
		return "", false
	}
	return strings.ToLower(segs[len(segs)-2] + "/" + segs[len(segs)-1]), true
}

// repoWorkspace is the workspace a repository belongs to: the registry names
// each rig's repository as owner/name, and the rig's workspace is the registry's
// answer for it. A repository whose owner and name match no registered rig is an
// error rather than a guess at which customer it bills.
func repoWorkspace(byRepo map[string]string, fullName string) (string, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || name == "" {
		return "", fmt.Errorf("forgejo sizes: %w: %s", errRepoName, strconv.Quote(fullName))
	}
	workspace, ok := byRepo[strings.ToLower(fullName)]
	if !ok {
		return "", fmt.Errorf("forgejo sizes: repo %s matches no registered rig's owner and name with a beads prefix", strconv.Quote(fullName))
	}
	return workspace, nil
}

// writeRows writes the rows, one JSON line each, to a temporary file beside the
// day's file and renames it over the day's file once the whole set is on disk.
// The rename is what makes a run all-or-nothing: a reader sees the previous
// snapshot or this run's complete one, never a half-written report.
func (w *ForgejoSizeWriter) writeRows(rows []ForgejoSizeRow, now time.Time) error {
	dir := ForgejoSizesDir(w.townRoot)
	path := filepath.Join(dir, now.Format(sizeDayLayout)+".jsonl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("forgejo sizes: creating %s: %w", dir, err)
	}
	tmp := path + ".tmp"
	// A run killed mid-write leaves its temporary file behind, and the exclusive
	// create below would refuse every later run. The job has one writer at a
	// time, so a temporary file found here is a dead run's: drop it.
	removeTemp(tmp)
	f, err := w.openTemp(tmp)
	if err != nil {
		return fmt.Errorf("forgejo sizes: opening %s: %w", tmp, err)
	}
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			_ = f.Close()
			removeTemp(tmp)
			return fmt.Errorf("forgejo sizes: writing %s: %w", tmp, err)
		}
	}
	if err := f.Close(); err != nil {
		removeTemp(tmp)
		return fmt.Errorf("forgejo sizes: closing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		removeTemp(tmp)
		return fmt.Errorf("forgejo sizes: replacing %s: %w", path, err)
	}
	return nil
}

// removeTemp drops a temporary file after a failed run, so the next run's
// exclusive create is not refused by this one's leftovers. A removal that fails
// is not reported over the failure that caused it.
func removeTemp(path string) {
	_ = os.Remove(path)
}
