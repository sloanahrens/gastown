package rigaudit

import "fmt"

// mergeQueueKeys are the merge_queue settings keys landing needs in each rig's
// own settings. A rig missing either one half-works at landing.
var mergeQueueKeys = []string{"forgejo", "post_land_command"}

// CheckSettings reports each unparked rig whose settings cannot be loaded, and
// each merge_queue key that is missing or empty in the settings that load. A
// rig whose settings fail to load gets that one problem and no key check.
// load is called once per rig and reads that rig's settings.
func CheckSettings(unparked []string, load func(rig string) (map[string]any, error)) []Problem {
	var problems []Problem
	for _, rig := range unparked {
		settings, err := load(rig)
		if err != nil {
			problems = append(problems, Problem{"settings", fmt.Sprintf("%s: cannot read its settings: %v", rig, err)})
			continue
		}
		// A merge_queue that is not a map reads as empty, so its keys are reported missing.
		queue, _ := settings["merge_queue"].(map[string]any)
		for _, key := range mergeQueueKeys {
			if s, ok := queue[key].(string); !ok || s == "" {
				problems = append(problems, Problem{"settings", fmt.Sprintf("%s: merge_queue.%s is missing or empty", rig, key)})
			}
		}
	}
	return problems
}
