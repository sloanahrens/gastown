package dashboard

// The nightly Forgejo size report: one line per repository per run, the record
// the monthly export charges storage from (gt-zg3qu). Forgejo reports a
// repository's size in kilobytes on the same list call the panel already makes,
// so the report costs no second request.
//
// It has no scheduler yet: its caller is the bead that runs it nightly beside
// the tracker size job (gt-oujm5).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

const (
	// sizeTSLayout is the report's timestamp: UTC, second precision, trailing Z,
	// the shape the resource sampler's rows already use.
	sizeTSLayout = "2006-01-02T15:04:05Z"
	// sizeDayLayout is the day a file holds, in UTC: one night's run appends to
	// that day's file, so a re-run adds a second set of lines rather than
	// dropping the first.
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

// forgejoRepos is the part of the Forgejo client the size writer calls. The
// size rides the repository list itself, so one call answers both.
type forgejoRepos interface {
	ListUserRepos(ctx context.Context) ([]forgejo.Repository, error)
}

// ForgejoSizeWriter appends the nightly size report to
// <town>/.runtime/metrics/forgejo-sizes/<UTC date>.jsonl.
type ForgejoSizeWriter struct {
	api      forgejoRepos
	townRoot string
	now      func() time.Time
}

// NewForgejoSizeWriter builds a writer over api for the town at townRoot.
func NewForgejoSizeWriter(api forgejoRepos, townRoot string) *ForgejoSizeWriter {
	return &ForgejoSizeWriter{api: api, townRoot: townRoot, now: time.Now}
}

// ForgejoSizesDir is <town>/.runtime/metrics/forgejo-sizes, the directory the
// nightly report's day files live in.
func ForgejoSizesDir(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "metrics", "forgejo-sizes")
}

// Write reads the viewer's repositories and appends one line per repository to
// the day's file. A failed read, mapping or write returns an error and leaves
// the file as it was: a partial report would be read as a complete one.
func (w *ForgejoSizeWriter) Write(ctx context.Context) ([]ForgejoSizeRow, error) {
	if w.townRoot == "" {
		return nil, fmt.Errorf("forgejo sizes: no town root to write under")
	}
	repos, err := w.api.ListUserRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("forgejo sizes: listing repositories: %w", err)
	}
	byRig, err := w.workspaceByRig()
	if err != nil {
		return nil, err
	}
	now := w.now().UTC()
	rows := make([]ForgejoSizeRow, 0, len(repos))
	for _, repo := range repos {
		workspace, err := repoWorkspace(byRig, repo.FullName)
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
	if err := w.appendRows(rows, now); err != nil {
		return nil, err
	}
	return rows, nil
}

// workspaceByRig maps each registered rig's name to its workspace id: the rig's
// beads prefix from the town registry gives the workspace (internal/workspaceid).
// A rig registered without a prefix is left out, so a repository of its is
// named as unmapped rather than charged to the town's default workspace, which
// belongs to another customer's bill.
func (w *ForgejoSizeWriter) workspaceByRig() (map[string]string, error) {
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
	for name, entry := range rigs.Rigs {
		if entry.BeadsConfig == nil {
			continue
		}
		prefix := strings.TrimSuffix(entry.BeadsConfig.Prefix, "-")
		if prefix == "" {
			continue
		}
		id, err := ids.For(prefix)
		if err != nil {
			return nil, fmt.Errorf("forgejo sizes: rig %q: %w", name, err)
		}
		out[name] = id
	}
	return out, nil
}

// repoWorkspace is the workspace a repository belongs to: the repository's base
// name names its rig (Forgejo serves a town's repositories as <owner>/<rig>),
// and the rig's workspace is the registry's answer for it. A repository that
// names no registered rig is an error rather than a guess at which customer it
// bills.
func repoWorkspace(byRig map[string]string, fullName string) (string, error) {
	_, name, ok := strings.Cut(fullName, "/")
	if !ok || name == "" {
		return "", fmt.Errorf("forgejo sizes: %w: %s", errRepoName, strconv.Quote(fullName))
	}
	workspace, ok := byRig[name]
	if !ok {
		return "", fmt.Errorf("forgejo sizes: repo %s names no registered rig with a beads prefix", strconv.Quote(fullName))
	}
	return workspace, nil
}

// appendRows writes the rows, one JSON line each, to the day's file.
func (w *ForgejoSizeWriter) appendRows(rows []ForgejoSizeRow, now time.Time) error {
	dir := ForgejoSizesDir(w.townRoot)
	path := filepath.Join(dir, now.Format(sizeDayLayout)+".jsonl")
	var buf bytes.Buffer
	for _, row := range rows {
		line, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("forgejo sizes: encoding a row: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("forgejo sizes: creating %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G302: the report is non-sensitive operational data
	if err != nil {
		return fmt.Errorf("forgejo sizes: opening %s: %w", path, err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return fmt.Errorf("forgejo sizes: writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("forgejo sizes: closing %s: %w", path, err)
	}
	return nil
}
