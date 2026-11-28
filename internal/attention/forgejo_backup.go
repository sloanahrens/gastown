package attention

import (
	"fmt"
	"time"
)

// forgejoBackupStaleAfter is the age past which the last verified dump raises
// the item: the 03:15 daily run plus margin (gt-q8nbzp).
const forgejoBackupStaleAfter = 26 * time.Hour

// ForgejoBackupItems returns one medium host-forgejo-backup item for a failed
// dump, carrying failure, for no verified dump, or for a last verified dump
// more than 26 hours before now (gt-q8nbzp).
func ForgejoBackupItems(failure string, lastGood, now time.Time) []Item {
	if failure != "" {
		return []Item{forgejoBackupItem(fmt.Sprintf(
			"Forgejo backup failed: %s; the newest dump may be truncated", failure))}
	}
	if lastGood.IsZero() {
		return []Item{forgejoBackupItem(
			"no verified Forgejo backup has been recorded")}
	}
	if age := now.Sub(lastGood); age > forgejoBackupStaleAfter {
		return []Item{forgejoBackupItem(fmt.Sprintf(
			"last verified Forgejo backup is %s old", age.Truncate(time.Minute)))}
	}
	return nil
}

// forgejoBackupItem is the one item every Forgejo backup condition raises,
// keyed by its rule (gt-q8nbzp).
func forgejoBackupItem(summary string) Item {
	return Item{
		Key:      RuleHostForgejoBackup,
		Kind:     KindHostForgejoBackup,
		Severity: SeverityMedium,
		Summary:  summary,
	}
}
