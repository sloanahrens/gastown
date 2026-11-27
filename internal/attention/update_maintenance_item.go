package attention

import "fmt"

// UpdateMaintenanceItem returns no item for an empty outcome or "done", and
// otherwise one info item keyed update-maintenance:<outcome> whose summary
// names the outcome (gt-1mqpcb).
func UpdateMaintenanceItem(outcome string) (Item, bool) {
	if outcome == "" || outcome == "done" {
		return Item{}, false
	}
	return Item{
		Key:      "update-maintenance:" + outcome,
		Kind:     KindUpdateMaintenance,
		Severity: SeverityLow,
		Summary:  fmt.Sprintf("maintenance before the update roll did not finish: %s", outcome),
	}, true
}
