package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// runtimeArchiveName is the staged archive holding the polecat's Claude
// settings and system prompt, which the agent command names by path.
const runtimeArchiveName = "polecat-runtime.tgz"

// buildRuntimeArchive returns a gzipped tar that extracts under /sandbox to
// gt/<rig>/polecats/.claude/{settings.json,system-prompt-<polecat>.md}: the
// files `claude --settings … --append-system-prompt-file …` reads. The
// settings are rendered for the sandbox; the system prompt has the host town
// root rewritten. A missing settings.json is an error, since the agent command
// names it; a missing system prompt is skipped.
func buildRuntimeArchive(spec SpawnSpec) ([]byte, error) {
	dir := filepath.Join(spec.TownRoot, spec.Rig, "polecats", ".claude")
	rawSettings, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return nil, fmt.Errorf("polecat settings: %w", err)
	}
	settings, err := renderSandboxSettings(rawSettings)
	if err != nil {
		return nil, err
	}
	files := []struct {
		name string
		data []byte
	}{{"settings.json", settings}}

	promptName := "system-prompt-" + spec.Polecat + ".md"
	prompt, err := os.ReadFile(filepath.Join(dir, promptName))
	switch {
	case err == nil:
		files = append(files, struct {
			name string
			data []byte
		}{promptName, []byte(RewriteTownRoot(string(prompt), spec.TownRoot, sandboxTownRoot))})
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("polecat system prompt: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		name := path.Join("gt", spec.Rig, "polecats", ".claude", f.name)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
