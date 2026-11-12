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

// WorkflowsPostContext reports whether one of the workflow files posts the
// commit-status context on a push to main. files maps each workflow's path to
// its bytes; when none does, why gives each file's miss so the health field can
// say what to fix.
func WorkflowsPostContext(files map[string][]byte, context string) (found bool, why string) {
	wantName, wantJob, ok := splitPostContext(context)
	if !ok {
		return false, fmt.Sprintf("%q is not a status context of the form \"<name> / <job>%s\"", context, pushContextSuffix)
	}
	if len(files) == 0 {
		return false, fmt.Sprintf("no workflow files under .forgejo/workflows/, so none posts %q", context)
	}
	paths := make([]string, 0, len(files))
	for file := range files {
		paths = append(paths, file)
	}
	sort.Strings(paths)

	var matches, reasons, malformed []string
	for _, file := range paths {
		posts, reason, broken := workflowPostsOnPush(file, files[file], wantName, wantJob)
		switch {
		case posts:
			matches = append(matches, file)
		case broken:
			malformed = append(malformed, reason)
		default:
			reasons = append(reasons, reason)
		}
	}
	if len(matches) > 0 {
		why = fmt.Sprintf("%s posts %q on a push to main", matches[0], context)
		for _, m := range malformed {
			why += "; " + m
		}
		return true, why
	}
	reasons = append(reasons, malformed...)
	return false, fmt.Sprintf("no workflow posts %q: %s", context, strings.Join(reasons, "; "))
}

// splitPostContext splits "<name> / <job> (push)" at its last " / ". ok is false
// when the context has no push suffix or no separator.
func splitPostContext(context string) (name, job string, ok bool) {
	trimmed, hasSuffix := strings.CutSuffix(context, pushContextSuffix)
	if !hasSuffix {
		return "", "", false
	}
	i := strings.LastIndex(trimmed, " / ")
	if i < 0 {
		return "", "", false
	}
	return trimmed[:i], trimmed[i+len(" / "):], true
}

// workflowPostsOnPush reports whether one workflow file posts the wanted context
// on a push to main. When it does not, reason says which part failed. broken is
// set when the file cannot be read as a workflow, such as malformed YAML: that
// is a miss for this file only.
func workflowPostsOnPush(file string, data []byte, wantName, wantJob string) (posts bool, reason string, broken bool) {
	var doc struct {
		Name string               `yaml:"name"`
		On   yaml.Node            `yaml:"on"`
		Jobs map[string]yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, fmt.Sprintf("%s is not a readable workflow: %v", file, err), true
	}
	name := strings.TrimSpace(doc.Name)
	if name == "" {
		// A workflow with no name: is named by its file, without the extension.
		base := path.Base(file)
		name = strings.TrimSuffix(base, path.Ext(base))
	}
	if name != wantName {
		return false, fmt.Sprintf("%s is named %q, not %q", file, name, wantName), false
	}
	if _, ok := doc.Jobs[wantJob]; !ok {
		return false, fmt.Sprintf("%s has no job %q (its jobs are %s)", file, wantJob, quotedKeys(doc.Jobs)), false
	}
	if ok, why := pushTriggerRunsOnMain(&doc.On); !ok {
		return false, fmt.Sprintf("%s %s", file, why), false
	}
	return true, "", false
}

// pushTriggerRunsOnMain reports whether the on: trigger fires on a push to main.
// why is the reason it does not, phrased to follow the file name.
func pushTriggerRunsOnMain(on *yaml.Node) (ok bool, why string) {
	switch on.Kind {
	case 0:
		return false, "has no on: trigger"
	case yaml.ScalarNode:
		if on.Value == "push" {
			return true, ""
		}
		return false, fmt.Sprintf("is not triggered by push (on: %s)", quotedList([]string{on.Value}))
	case yaml.SequenceNode:
		var events []string
		for _, item := range on.Content {
			if item.Kind != yaml.ScalarNode {
				continue
			}
			if item.Value == "push" {
				return true, ""
			}
			events = append(events, item.Value)
		}
		return false, fmt.Sprintf("is not triggered by push (on: %s)", quotedList(events))
	case yaml.MappingNode:
		var events []string
		for i := 0; i+1 < len(on.Content); i += 2 {
			event := on.Content[i].Value
			if event == "push" {
				return pushFilterRunsOnMain(on.Content[i+1])
			}
			events = append(events, event)
		}
		return false, fmt.Sprintf("is not triggered by push (on: %s)", quotedList(events))
	}
	return false, "has an on: trigger that is not an event name, a list or a map"
}

// pushFilterRunsOnMain checks the value under push: for a branch filter that
// leaves main out. A push: with no value, or with no branch filter, runs on
// every branch.
func pushFilterRunsOnMain(filter *yaml.Node) (ok bool, why string) {
	if filter.Kind == yaml.ScalarNode && filter.Tag == "!!null" {
		return true, ""
	}
	if filter.Kind != yaml.MappingNode {
		return false, "has a push: trigger that is not a map of filters"
	}
	for i := 0; i+1 < len(filter.Content); i += 2 {
		key, value := filter.Content[i].Value, filter.Content[i+1]
		switch key {
		case "branches":
			patterns := scalarValues(value)
			hit, err := anyPatternMatches(patterns, "main")
			if err != nil {
				return false, fmt.Sprintf("has a push branch filter that is not a valid pattern: %v", err)
			}
			if !hit {
				return false, fmt.Sprintf("runs on push to branches %s, not main", quotedList(patterns))
			}
		case "branches-ignore":
			hit, err := anyPatternMatches(scalarValues(value), "main")
			if err != nil {
				return false, fmt.Sprintf("has a push branches-ignore filter that is not a valid pattern: %v", err)
			}
			if hit {
				return false, "ignores push to main"
			}
		}
	}
	return true, ""
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
