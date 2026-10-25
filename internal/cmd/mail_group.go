package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Group command flags
var (
	groupJSON    bool
	groupMembers []string
)

var mailGroupCmd = &cobra.Command{
	Use:   "group",
	Short: "Manage mail groups",
	Long: `Create and manage mail distribution groups.

Groups are named collections of addresses used for mail distribution.
Members can be:
  - Direct addresses (gastown/crew/max)
  - Patterns (*/witness, gastown/*)
  - Other group names (nested groups)

Examples:
  gt mail group list                              # List all groups
  gt mail group show ops-team                     # Show group members
  gt mail group create ops-team gastown/witness gastown/crew/max
  gt mail group add ops-team deacon/
  gt mail group remove ops-team gastown/witness
  gt mail group delete ops-team`,
	RunE: requireSubcommand,
}

var groupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all groups",
	Long:  "List all mail distribution groups.",
	Args:  cobra.NoArgs,
	RunE:  runGroupList,
}

var groupShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show group details",
	Long:  "Display the members and metadata for a group.",
	Args:  cobra.ExactArgs(1),
	RunE:  runGroupShow,
}

var groupCreateCmd = &cobra.Command{
	Use:   "create <name> [members...]",
	Short: "Create a new group",
	Long: `Create a new mail distribution group.

Members can be specified as positional arguments or with --member flags.

Examples:
  gt mail group create ops-team gastown/witness gastown/crew/max
  gt mail group create ops-team --member gastown/witness --member gastown/crew/max`,
	Args: cobra.MinimumNArgs(1),
	RunE: runGroupCreate,
}

var groupAddCmd = &cobra.Command{
	Use:   "add <name> <member>",
	Short: "Add member to group",
	Long:  "Add a new member to an existing group.",
	Args:  cobra.ExactArgs(2),
	RunE:  runGroupAdd,
}

var groupRemoveCmd = &cobra.Command{
	Use:   "remove <name> <member>",
	Short: "Remove member from group",
	Long:  "Remove a member from an existing group.",
	Args:  cobra.ExactArgs(2),
	RunE:  runGroupRemove,
}

var groupDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a group",
	Long:  "Permanently delete a mail distribution group.",
	Args:  cobra.ExactArgs(1),
	RunE:  runGroupDelete,
}

func init() {
	// List flags
	groupListCmd.Flags().BoolVar(&groupJSON, "json", false, "Output as JSON")

	// Show flags
	groupShowCmd.Flags().BoolVar(&groupJSON, "json", false, "Output as JSON")

	// Create flags
	groupCreateCmd.Flags().StringArrayVar(&groupMembers, "member", nil, "Member to add (repeatable)")

	// Add subcommands
	mailGroupCmd.AddCommand(groupListCmd)
	mailGroupCmd.AddCommand(groupShowCmd)
	mailGroupCmd.AddCommand(groupCreateCmd)
	mailGroupCmd.AddCommand(groupAddCmd)
	mailGroupCmd.AddCommand(groupRemoveCmd)
	mailGroupCmd.AddCommand(groupDeleteCmd)

	mailCmd.AddCommand(mailGroupCmd)
}

func runGroupList(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	b := beads.New(townRoot)
	groups, err := beads.ListGroupBeads(b)
	if err != nil {
		return fmt.Errorf("listing groups: %w", err)
	}

	if groupJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(groups)
	}

	if len(groups) == 0 {
		fmt.Println("No groups defined.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tMEMBERS\tCREATED BY")
	for name, fields := range groups {
		memberCount := len(fields.Members)
		memberStr := fmt.Sprintf("%d member(s)", memberCount)
		if memberCount <= 3 {
			memberStr = strings.Join(fields.Members, ", ")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, memberStr, fields.CreatedBy)
	}
	return w.Flush()
}

func runGroupShow(cmd *cobra.Command, args []string) error {
	name := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	b := beads.New(townRoot)
	_, fields, err := beads.GetGroupByName(b, name)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return fmt.Errorf("group not found: %s", name)
		}
		return fmt.Errorf("getting group: %w", err)
	}

	if groupJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(fields)
	}

	fmt.Printf("Group: %s\n", fields.Name)
	fmt.Printf("Created by: %s\n", fields.CreatedBy)
	if fields.CreatedAt != "" {
		fmt.Printf("Created at: %s\n", fields.CreatedAt)
	}
	fmt.Println()
	fmt.Println("Members:")
	if len(fields.Members) == 0 {
		fmt.Println("  (no members)")
	} else {
		for _, m := range fields.Members {
			fmt.Printf("  - %s\n", m)
		}
	}
	return nil
}

func runGroupCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	members := args[1:] // Positional members

	// Add --member flag values
	members = append(members, groupMembers...)

	if !isValidGroupName(name) {
		return fmt.Errorf("invalid group name %q: must be lowercase alphanumeric with dashes/underscores, starting with a letter or digit, at most %d characters", name, beads.MaxGroupNameLength)
	}

	// Validate member patterns
	for _, m := range members {
		if !isValidMemberPattern(m) {
			return fmt.Errorf("invalid member pattern %q: %w", m, memberPatternError(m))
		}
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Detect creator
	createdBy := os.Getenv("BD_ACTOR")
	if createdBy == "" {
		createdBy = "unknown"
	}

	b := beads.New(townRoot)

	// Check if group already exists
	existing, _, err := beads.GetGroupByName(b, name)
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return err
	}
	if existing != nil {
		return fmt.Errorf("group already exists: %s", name)
	}

	_, err = beads.CreateGroupBead(b, name, &beads.GroupFields{
		Members:   members,
		CreatedBy: createdBy,
	})
	if err != nil {
		return fmt.Errorf("creating group: %w", err)
	}

	fmt.Printf("Created group %q with %d member(s)\n", name, len(members))
	return nil
}

func runGroupAdd(cmd *cobra.Command, args []string) error {
	name := args[0]
	member := args[1]

	if !isValidMemberPattern(member) {
		return fmt.Errorf("invalid member pattern %q: %w", member, memberPatternError(member))
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	b := beads.New(townRoot)
	if _, err := beads.AddGroupMember(b, name, member); err != nil {
		return fmt.Errorf("adding member: %w", err)
	}

	fmt.Printf("Added %q to group %q\n", member, name)
	return nil
}

func runGroupRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	member := args[1]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	b := beads.New(townRoot)
	if _, err := beads.RemoveGroupMember(b, name, member); err != nil {
		return fmt.Errorf("removing member: %w", err)
	}

	fmt.Printf("Removed %q from group %q\n", member, name)
	return nil
}

func runGroupDelete(cmd *cobra.Command, args []string) error {
	name := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	b := beads.New(townRoot)

	// Check if group exists
	_, _, err = beads.GetGroupByName(b, name)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return fmt.Errorf("group not found: %s", name)
		}
		return err
	}

	if err := beads.DeleteGroupBead(b, name); err != nil {
		return fmt.Errorf("deleting group: %w", err)
	}

	fmt.Printf("Deleted group %q\n", name)
	return nil
}

// isValidGroupName reports whether name is a group name the group store
// accepts; beads.ValidateGroupName owns the rule.
func isValidGroupName(name string) bool {
	return beads.ValidateGroupName(name) == nil
}

// isValidMemberPattern checks if a member pattern is syntactically valid.
// Valid patterns include:
// - Direct addresses: gastown/crew/max, deacon/
// - Wildcards: */witness, gastown/*, gastown/crew/*
// - Special patterns: @town, @crew, @witnesses
// - Group, queue, and channel names: ops-team
func isValidMemberPattern(pattern string) bool {
	return memberPatternError(pattern) == nil
}

// memberPatternError says why pattern cannot be a group member, so a rejected
// pattern names the rule it broke instead of only repeating the token.
func memberPatternError(pattern string) error {
	if pattern == "" {
		return errors.New("must not be empty")
	}

	// @ patterns are valid
	if strings.HasPrefix(pattern, "@") {
		if len(pattern) == 1 {
			return errors.New("a special pattern needs a name after @")
		}
		return nil
	}

	// Path patterns with wildcards
	if strings.Contains(pattern, "/") {
		// Every segment must be non-empty, except a trailing slash on a prefix.
		parts := strings.Split(pattern, "/")
		for _, p := range parts {
			if p == "" && pattern[len(pattern)-1] != '/' {
				return errors.New("path has an empty segment")
			}
		}
		return nil
	}

	// A bare name is a group, queue, or channel reference, never an agent
	// address. Groups and channels share the group rule, so a name outside it
	// can only be a queue; the group commands refuse it rather than store a
	// member the resolver may skip (gt-s4sen).
	if err := beads.ValidateGroupName(pattern); err != nil {
		return fmt.Errorf("a bare name must be a group, queue, or channel name: %w", err)
	}
	return nil
}
