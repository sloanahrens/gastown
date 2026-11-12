package landworker

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// pushContextSuffix ends every commit status context a workflow posts on a push.
const pushContextSuffix = " (push)"

// emptyTriggerWhy names an on: trigger with no events: it fires on no push.
const emptyTriggerWhy = "has no push trigger: the `on:` trigger is empty"

// PostVerdict says how far a rig's workflows post a push-to-main status context.
// The values rise from weakest to strongest.
type PostVerdict int

const (
	// DoesNotPost means no workflow posts the context on a push to main.
	DoesNotPost PostVerdict = iota
	// PostsForSomeCommits means a workflow posts the context only for pushes whose
	// commits touch a paths or paths-ignore filter.
	PostsForSomeCommits
	// PostsOnEveryPush means a workflow posts the context on every push to main.
	PostsOnEveryPush
)

// WorkflowsPostContext reports whether the workflow files post the commit-status
// context on a push to main. files maps each workflow's path to its bytes. The
// verdict is the strongest any file gives. why names that file when one posts;
// when none does, it gives each file's miss so the health field can say what to fix.
func WorkflowsPostContext(files map[string][]byte, context string) (verdict PostVerdict, why string) {
	want, ok := parsePostContext(context)
	if !ok {
		return DoesNotPost, fmt.Sprintf("%q is not a status context of the form \"<name> / <job>%s\"", context, pushContextSuffix)
	}
	if len(files) == 0 {
		return DoesNotPost, fmt.Sprintf("no workflow files under .forgejo/workflows/, so none posts %q", context)
	}
	paths := make([]string, 0, len(files))
	for file := range files {
		paths = append(paths, file)
	}
	sort.Strings(paths)

	var reasons, malformed []string
	for _, file := range paths {
		v, reason, broken := workflowPostsOnPush(file, files[file], want)
		switch {
		case broken:
			malformed = append(malformed, reason)
		case v == DoesNotPost:
			reasons = append(reasons, reason)
		case v > verdict:
			verdict, why = v, reason
		}
	}
	if verdict != DoesNotPost {
		for _, m := range malformed {
			why += "; " + m
		}
		return verdict, why
	}
	reasons = append(reasons, malformed...)
	return DoesNotPost, fmt.Sprintf("no workflow posts %q: %s", context, strings.Join(reasons, "; "))
}

// postContext is a status context split into the workflow name and job key it is
// built from.
type postContext struct {
	full, name, job string
}

// parsePostContext splits "<name> / <job> (push)" at its last " / ". ok is false
// when the context has no push suffix or no separator.
func parsePostContext(context string) (postContext, bool) {
	trimmed, hasSuffix := strings.CutSuffix(context, pushContextSuffix)
	if !hasSuffix {
		return postContext{}, false
	}
	i := strings.LastIndex(trimmed, " / ")
	if i < 0 {
		return postContext{}, false
	}
	return postContext{full: context, name: trimmed[:i], job: trimmed[i+len(" / "):]}, true
}

// workflowPostsOnPush gives the verdict one workflow file reaches for the context.
// For DoesNotPost, reason is the file's miss; otherwise it says how the file posts.
// broken is set when the file is not readable YAML: that is a miss for this file only.
func workflowPostsOnPush(file string, data []byte, want postContext) (verdict PostVerdict, reason string, broken bool) {
	var doc struct {
		Name string               `yaml:"name"`
		On   yaml.Node            `yaml:"on"`
		Jobs map[string]yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return DoesNotPost, fmt.Sprintf("%s is not a readable workflow: %v", file, err), true
	}
	name := strings.TrimSpace(doc.Name)
	if name == "" {
		// A workflow with no name: is named by its file, without the extension.
		base := path.Base(file)
		name = strings.TrimSuffix(base, path.Ext(base))
	}
	if name != want.name {
		return DoesNotPost, fmt.Sprintf("%s is named %q, not %q", file, name, want.name), false
	}
	if _, ok := doc.Jobs[want.job]; !ok {
		return DoesNotPost, fmt.Sprintf("%s has no job %q (its jobs are %s)", file, want.job, quotedKeys(doc.Jobs)), false
	}
	v, detail := pushTriggerVerdict(&doc.On)
	switch v {
	case DoesNotPost:
		return DoesNotPost, fmt.Sprintf("%s %s", file, detail), false
	case PostsForSomeCommits:
		return v, fmt.Sprintf("%s posts %q only for commits touching the filtered paths (%s)", file, want.full, detail), false
	}
	return v, fmt.Sprintf("%s posts %q on every push to main", file, want.full), false
}

// pushTriggerVerdict reads an on: trigger. detail is the paths filter behind a
// PostsForSomeCommits verdict, and otherwise says why the trigger misses, in words
// that follow the file name.
func pushTriggerVerdict(on *yaml.Node) (verdict PostVerdict, detail string) {
	switch on.Kind {
	case 0:
		return DoesNotPost, "has no on: trigger"
	case yaml.ScalarNode:
		if isNull(on) {
			return DoesNotPost, emptyTriggerWhy
		}
		if on.Value == "push" {
			return PostsOnEveryPush, ""
		}
		return DoesNotPost, fmt.Sprintf("is not triggered by push (on: %s)", quotedList([]string{on.Value}))
	case yaml.SequenceNode:
		if len(on.Content) == 0 {
			return DoesNotPost, emptyTriggerWhy
		}
		var events []string
		for _, item := range on.Content {
			if item.Kind != yaml.ScalarNode {
				continue
			}
			if item.Value == "push" {
				return PostsOnEveryPush, ""
			}
			events = append(events, item.Value)
		}
		return DoesNotPost, fmt.Sprintf("is not triggered by push (on: %s)", quotedList(events))
	case yaml.MappingNode:
		if len(on.Content) == 0 {
			return DoesNotPost, emptyTriggerWhy
		}
		var events []string
		for i := 0; i+1 < len(on.Content); i += 2 {
			event := on.Content[i].Value
			if event == "push" {
				return pushFilterVerdict(on.Content[i+1])
			}
			events = append(events, event)
		}
		return DoesNotPost, fmt.Sprintf("is not triggered by push (on: %s)", quotedList(events))
	}
	return DoesNotPost, "has an on: trigger that is not an event name, a list or a map"
}

// pushFilterVerdict reads the value under push:. A push: with no value, or with no
// branch filter, fires on every branch, so main is selected; a paths filter then
// narrows which commits post.
func pushFilterVerdict(filter *yaml.Node) (verdict PostVerdict, detail string) {
	if isNull(filter) {
		return PostsOnEveryPush, ""
	}
	if filter.Kind != yaml.MappingNode {
		return DoesNotPost, "has a push: trigger that is not a map of filters"
	}
	filters := make(map[string]*yaml.Node, len(filter.Content)/2)
	for i := 0; i+1 < len(filter.Content); i += 2 {
		filters[filter.Content[i].Value] = filter.Content[i+1]
	}
	if ok, why := branchFilterSelectsMain(filters["branches"], filters["branches-ignore"]); !ok {
		return DoesNotPost, why
	}
	for _, key := range []string{"paths", "paths-ignore"} {
		if !absent(filters[key]) {
			return PostsForSomeCommits, fmt.Sprintf("%s: %s", key, quotedList(scalarValues(filters[key])))
		}
	}
	return PostsOnEveryPush, ""
}

// branchFilterSelectsMain applies a push branch filter to main with Actions rules.
// A pattern selects the branches it matches. A pattern starting with ! removes the
// branches it matches from those selected so far. A list holding only ! patterns
// selects every branch except those. branches-ignore then removes what it matches.
func branchFilterSelectsMain(branches, ignored *yaml.Node) (ok bool, why string) {
	selected := true
	if !absent(branches) {
		patterns := scalarValues(branches)
		selected = !hasPositivePattern(patterns)
		for _, p := range patterns {
			hit, err := path.Match(strings.TrimPrefix(p, "!"), "main")
			if err != nil {
				return false, fmt.Sprintf("has a push branch filter that is not a valid pattern: %v", err)
			}
			if !hit {
				continue
			}
			selected = !strings.HasPrefix(p, "!")
		}
		if !selected {
			return false, fmt.Sprintf("runs on push to branches %s, not main", quotedList(patterns))
		}
	}
	if !absent(ignored) {
		patterns := scalarValues(ignored)
		hit, err := anyPatternMatches(patterns, "main")
		if err != nil {
			return false, fmt.Sprintf("has a push branches-ignore filter that is not a valid pattern: %v", err)
		}
		if hit {
			return false, fmt.Sprintf("ignores main on push (branches-ignore: %s)", quotedList(patterns))
		}
	}
	return true, ""
}

// hasPositivePattern reports whether any pattern selects branches rather than
// removing them.
func hasPositivePattern(patterns []string) bool {
	for _, p := range patterns {
		if !strings.HasPrefix(p, "!") {
			return true
		}
	}
	return false
}

// absent reports whether a filter key is missing or has no value.
func absent(node *yaml.Node) bool {
	return node == nil || isNull(node)
}

// isNull reports whether a node is a bare null, such as an on: or push: with no value.
func isNull(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Tag == "!!null"
}

// scalarValues returns the strings a node holds: a scalar is one, a sequence
// is its scalar items.
func scalarValues(node *yaml.Node) []string {
	switch node.Kind {
	case yaml.ScalarNode:
		return []string{node.Value}
	case yaml.SequenceNode:
		var out []string
		for _, item := range node.Content {
			if item.Kind == yaml.ScalarNode {
				out = append(out, item.Value)
			}
		}
		return out
	}
	return nil
}

// anyPatternMatches reports whether a branch glob matches the branch. A malformed
// glob is an error, not a non-match, so the caller can name it.
func anyPatternMatches(patterns []string, branch string) (bool, error) {
	for _, p := range patterns {
		ok, err := path.Match(p, branch)
		if err != nil {
			return false, fmt.Errorf("%q: %w", p, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// quotedKeys lists a jobs map's keys in order, quoted, or "none" when empty.
func quotedKeys(jobs map[string]yaml.Node) string {
	keys := make([]string, 0, len(jobs))
	for key := range jobs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return quotedList(keys)
}

// quotedList quotes each name and joins them, or says "none" for an empty list.
func quotedList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}
