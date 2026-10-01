// Package commands provisions Gas Town's slash commands into a workspace's
// .claude/commands/ directory.
package commands

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed bodies/*.md
var bodiesFS embed.FS

// Field represents a frontmatter key-value pair.
type Field struct {
	Key   string
	Value string
}

// Command defines a slash command and its Claude Code frontmatter.
type Command struct {
	Name        string
	Description string
	Fields      []Field
}

// Commands is the registry of available commands.
var Commands = []Command{
	{
		Name:        "done",
		Description: "Signal work complete and submit the branch for landing",
		Fields: []Field{
			{"allowed-tools", "Bash(gt done:*), Bash(git status:*), Bash(git log:*), Bash(git add:*), Bash(git commit:*), Bash(git push:*), Bash(bd close:*)"},
			{"argument-hint", "[--status COMPLETED|ESCALATED|DEFERRED] [--target <branch>]"},
		},
	},
	{
		Name:        "handoff",
		Description: "Hand off to fresh session, work continues from hook",
		Fields: []Field{
			{"allowed-tools", "Bash(gt handoff:*)"},
			{"argument-hint", "[message]"},
		},
	},
	{
		Name:        "review",
		Description: "Review code changes with structured grading (A-F)",
		Fields: []Field{
			{"allowed-tools", "Bash(git diff:*), Bash(git rev-parse:*), Bash(gh pr diff:*)"},
			{"argument-hint", "[--staged | --branch | --pr <url>]"},
		},
	},
}

// BuildCommand assembles frontmatter + body for a command.
func BuildCommand(cmd Command) (string, error) {
	body, err := bodiesFS.ReadFile("bodies/" + cmd.Name + ".md")
	if err != nil {
		return "", fmt.Errorf("reading body: %w", err)
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("description: %s\n", cmd.Description))
	for _, f := range cmd.Fields {
		b.WriteString(fmt.Sprintf("%s: %s\n", f.Key, f.Value))
	}
	b.WriteString("---\n\n")
	b.Write(body)

	return b.String(), nil
}

// commandsDir is where Claude Code reads a workspace's slash commands.
func commandsDir(workspacePath string) string {
	return filepath.Join(workspacePath, ".claude", "commands")
}

// Provision writes the commands into workspacePath/.claude/commands/.
// An existing command file is left alone (no overwrite).
func Provision(workspacePath string) error {
	dir := commandsDir(workspacePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating dir: %w", err)
	}

	for _, cmd := range Commands {
		path := filepath.Join(dir, cmd.Name+".md")

		// Don't overwrite existing
		if _, err := os.Stat(path); err == nil {
			continue
		}

		content, err := BuildCommand(cmd)
		if err != nil {
			return fmt.Errorf("building %s: %w", cmd.Name, err)
		}

		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("writing %s: %w", cmd.Name, err)
		}
	}

	return nil
}

// Missing returns the commands missing from workspacePath/.claude/commands/.
func Missing(workspacePath string) []string {
	dir := commandsDir(workspacePath)
	var missing []string

	for _, cmd := range Commands {
		path := filepath.Join(dir, cmd.Name+".md")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			missing = append(missing, cmd.Name)
		}
	}

	return missing
}

// FindByName returns the command with the given name, or nil if not found.
func FindByName(name string) *Command {
	for i := range Commands {
		if Commands[i].Name == name {
			return &Commands[i]
		}
	}
	return nil
}

// Names returns the names of all registered commands.
func Names() []string {
	names := make([]string, len(Commands))
	for i, cmd := range Commands {
		names[i] = cmd.Name
	}
	return names
}
