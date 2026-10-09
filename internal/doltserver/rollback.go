package doltserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Backup represents a discovered migration backup directory.
type Backup struct {
	// Path is the absolute path to the backup directory.
	Path string

	// Timestamp is the YYYYMMDD-HHMMSS suffix from the directory name.
	Timestamp string

	// Metadata is the parsed backup metadata, if available.
	Metadata map[string]interface{}
}

// FindBackups finds all migration backup directories in the town root.
// Returns them sorted newest-first by timestamp suffix.
func FindBackups(townRoot string) ([]Backup, error) {
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return nil, fmt.Errorf("reading town root: %w", err)
	}

	const prefix = "migration-backup-"
	var backups []Backup

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}

		timestamp := strings.TrimPrefix(name, prefix)
		if timestamp == "" {
			continue
		}

		b := Backup{
			Path:      filepath.Join(townRoot, name),
			Timestamp: timestamp,
		}

		// Load metadata if present
		metaPath := filepath.Join(b.Path, "metadata.json")
		if data, err := os.ReadFile(metaPath); err == nil {
			var meta map[string]interface{}
			if err := json.Unmarshal(data, &meta); err == nil {
				b.Metadata = meta
			}
		}

		backups = append(backups, b)
	}

	// Sort newest-first by timestamp string (lexicographic works for YYYYMMDD-HHMMSS)
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].Timestamp > backups[j].Timestamp
	})

	return backups, nil
}

// RollbackResult tracks what was restored during rollback.
type RollbackResult struct {
	BackupPath    string
	RestoredTown  bool
	RestoredRigs  []string
	SkippedRigs   []string
	MetadataReset []string
}

// RestoreFromBackup restores .beads directories from a migration backup.
// The backup directory is expected to have the structure created by the
// migration formula's backup step:
//
//	migration-backup-TIMESTAMP/
//	├── town-beads/          → restored to <townRoot>/.beads
//	└── <rigname>-beads/     → restored to <townRoot>/<rigname>/.beads
//
// It also handles the test-backup structure:
//
//	.migration-test-backup/
//	├── town-beads/          → restored to <townRoot>/.beads
//	└── rigs/<rigname>/.beads → restored to <townRoot>/<rigname>/.beads
//
// For each restored .beads directory, the existing one is removed first.
// This resets metadata.json to the pre-migration state since the backup
// contains the original metadata.json files.
func RestoreFromBackup(townRoot, backupPath string) (*RollbackResult, error) {
	// Verify backup directory exists
	info, err := os.Stat(backupPath)
	if err != nil {
		return nil, fmt.Errorf("backup not found: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("backup path is not a directory: %s", backupPath)
	}

	result := &RollbackResult{
		BackupPath: backupPath,
	}

	// Restore town-level beads
	townBackup := filepath.Join(backupPath, "town-beads")
	if _, err := os.Stat(townBackup); err == nil {
		townBeads := filepath.Join(townRoot, ".beads")
		if err := replaceDir(townBeads, townBackup); err != nil {
			return result, fmt.Errorf("restoring town beads: %w", err)
		}
		result.RestoredTown = true
		result.MetadataReset = append(result.MetadataReset, "town (.beads)")
	}

	// Restore rig-level beads: try formula-style first (<rigname>-beads/)
	entries, err := os.ReadDir(backupPath)
	if err != nil {
		return result, fmt.Errorf("reading backup directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()

		// Skip known non-rig directories
		if name == "town-beads" || name == "rigs" {
			continue
		}

		// Formula-style backup: <rigname>-beads/
		if strings.HasSuffix(name, "-beads") {
			rigName := strings.TrimSuffix(name, "-beads")
			// An entry named exactly "-beads" trims to the empty rig name,
			// which filepath.Join would resolve to the town's own .beads:
			// refuse to restore it over the town. (G7)
			if rigName == "" {
				result.SkippedRigs = append(result.SkippedRigs, name)
				continue
			}
			rigBeads := filepath.Join(townRoot, rigName, ".beads")
			rigBackup := filepath.Join(backupPath, name)
			if err := replaceDir(rigBeads, rigBackup); err != nil {
				result.SkippedRigs = append(result.SkippedRigs, rigName)
				continue
			}
			result.RestoredRigs = append(result.RestoredRigs, rigName)
			result.MetadataReset = append(result.MetadataReset, rigName)
		}
	}

	// Also check test-backup style: rigs/<rigname>/.beads
	rigsDir := filepath.Join(backupPath, "rigs")
	if rigEntries, err := os.ReadDir(rigsDir); err == nil {
		for _, entry := range rigEntries {
			if !entry.IsDir() {
				continue
			}
			rigName := entry.Name()
			rigBackupBeads := filepath.Join(rigsDir, rigName, ".beads")
			if _, err := os.Stat(rigBackupBeads); err != nil {
				continue
			}
			rigBeads := filepath.Join(townRoot, rigName, ".beads")
			if err := replaceDir(rigBeads, rigBackupBeads); err != nil {
				result.SkippedRigs = append(result.SkippedRigs, rigName)
				continue
			}
			result.RestoredRigs = append(result.RestoredRigs, rigName)
			result.MetadataReset = append(result.MetadataReset, rigName)
		}
	}

	return result, nil
}

// replaceDir replaces dst with a copy of src.
//
// The copy is staged beside dst and moved into place only once it is complete,
// so a copy that fails partway leaves the existing dst intact (G7).
func replaceDir(dst, src string) error {
	// Verify source exists
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("source not found: %w", err)
	}

	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Stage the copy in the destination's directory so the swap below is a
	// rename within one filesystem.
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(dst)+".restore-*")
	if err != nil {
		return fmt.Errorf("staging directory for %s: %w", dst, err)
	}
	staged := filepath.Join(stage, filepath.Base(dst))
	if err := copyDir(staged, src); err != nil {
		_ = os.RemoveAll(stage)
		return fmt.Errorf("copying %s to %s: %w", src, dst, err)
	}

	// The existing directory is at risk only now: move it aside, swap the
	// staged copy in, and put the old one back if the swap fails.
	previous := filepath.Join(stage, "previous")
	havePrevious := false
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, previous); err != nil {
			_ = os.RemoveAll(stage)
			return fmt.Errorf("setting aside existing %s: %w", dst, err)
		}
		havePrevious = true
	}
	if err := os.Rename(staged, dst); err != nil {
		if havePrevious {
			if backErr := os.Rename(previous, dst); backErr != nil {
				// Keep the stage: the displaced directory is still inside it.
				return fmt.Errorf("installing %s: %w (the displaced directory is preserved at %s)", dst, err, previous)
			}
		}
		_ = os.RemoveAll(stage)
		return fmt.Errorf("installing %s: %w", dst, err)
	}
	_ = os.RemoveAll(stage) // the displaced directory, if there was one

	return nil
}

// copyDir recursively copies a directory tree.
func copyDir(dst, src string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := copyDir(dstPath, srcPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(dstPath, srcPath); err != nil {
				return err
			}
		}
	}

	return nil
}

// copyFile copies a single file preserving permissions.
func copyFile(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, srcInfo.Mode())
}
