// Package bdsnapshot defines the checked-in bd command surface
// (internal/cmdtree/bd-command-tree.json) and reduces raw
// `bd capabilities --json` output to it. It is separate from cmdtree so the
// generator can run before the snapshot it writes exists.
package bdsnapshot

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Snapshot is the checked-in bd command surface.
type Snapshot struct {
	Source          string    `json:"source"`
	Version         string    `json:"version"`
	Commit          string    `json:"commit"`
	ContractVersion int       `json:"contract_version"`
	PersistentFlags []string  `json:"persistent_flags"`
	Commands        []Command `json:"commands"`
}

// Command is one bd command: its path without the binary name ("mol
// wisp list"), aliases, and its own (non-persistent) flag names.
type Command struct {
	Path    string   `json:"path"`
	Aliases []string `json:"aliases,omitempty"`
	Hidden  bool     `json:"hidden,omitempty"`
	Flags   []string `json:"flags,omitempty"`
	// SubcommandOnly marks a parent that is not runnable itself: a word
	// after it can only be one of its subcommands. Runnable parents (bd mol
	// wisp <proto-id>, bd dep <issue-id>) and leaves leave it false.
	SubcommandOnly bool `json:"subcommand_only,omitempty"`
}

// capabilities mirrors the fields of `bd capabilities --json` the snapshot keeps.
type capabilities struct {
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	ContractVersion int    `json:"contract_version"`
	Commands        []struct {
		Path    string   `json:"path"`
		Aliases []string `json:"aliases"`
		Hidden  bool     `json:"hidden"`
		Flags   []struct {
			Name       string `json:"name"`
			Persistent bool   `json:"persistent"`
		} `json:"flags"`
	} `json:"commands"`
}

// Reduce turns raw `bd capabilities --json` output into a
// Snapshot: persistent flags listed once, each command keeping only its
// own flag names. source records where the binary was built from.
func Reduce(raw []byte, source string) (Snapshot, error) {
	var c capabilities
	if err := json.Unmarshal(raw, &c); err != nil {
		return Snapshot{}, fmt.Errorf("parse bd capabilities: %w", err)
	}
	if len(c.Commands) == 0 {
		return Snapshot{}, fmt.Errorf("bd capabilities lists no commands")
	}
	snap := Snapshot{Source: source, Version: c.Version, Commit: c.Commit, ContractVersion: c.ContractVersion}
	persistent := map[string]bool{}
	for _, cmd := range c.Commands {
		bc := Command{Path: cmd.Path, Aliases: cmd.Aliases, Hidden: cmd.Hidden}
		for _, f := range cmd.Flags {
			if f.Persistent {
				persistent[f.Name] = true
			} else {
				bc.Flags = append(bc.Flags, f.Name)
			}
		}
		snap.Commands = append(snap.Commands, bc)
	}
	for name := range persistent {
		snap.PersistentFlags = append(snap.PersistentFlags, name)
	}
	sort.Strings(snap.PersistentFlags)
	sort.Slice(snap.Commands, func(i, j int) bool { return snap.Commands[i].Path < snap.Commands[j].Path })
	return snap, nil
}

// SubcommandOnly reads a command's --help output. cobra prints one usage line
// per way to call the command: "<path> [args] [flags]" when it is runnable and
// "<path> [command]" when it has subcommands. A parent whose only usage line is
// the [command] one cannot take positional arguments.
func SubcommandOnly(help string) (bool, error) {
	lines := strings.Split(help, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "Usage:" {
			continue
		}
		var usage []string
		for _, u := range lines[i+1:] {
			if strings.TrimSpace(u) == "" {
				break
			}
			usage = append(usage, strings.TrimSpace(u))
		}
		if len(usage) == 0 {
			return false, fmt.Errorf("empty Usage section")
		}
		for _, u := range usage {
			if !strings.HasSuffix(u, "[command]") {
				return false, nil
			}
		}
		return true, nil
	}
	return false, fmt.Errorf("no Usage section in help output")
}

// MarkSubcommandOnly sets SubcommandOnly on every command that has children,
// asking help for each parent's --help output.
func MarkSubcommandOnly(snap *Snapshot, help func(path string) (string, error)) error {
	parents := map[string]bool{}
	for _, c := range snap.Commands {
		if i := strings.LastIndex(c.Path, " "); i > 0 {
			parents[c.Path[:i]] = true
		}
	}
	for i := range snap.Commands {
		c := &snap.Commands[i]
		if !parents[c.Path] {
			continue
		}
		out, err := help(c.Path)
		if err != nil {
			return fmt.Errorf("bd %s --help: %w", c.Path, err)
		}
		only, err := SubcommandOnly(out)
		if err != nil {
			return fmt.Errorf("bd %s --help: %w", c.Path, err)
		}
		c.SubcommandOnly = only
	}
	return nil
}
