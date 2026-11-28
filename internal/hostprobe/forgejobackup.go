package hostprobe

import "time"

// ForgejoBackup is when this host last completed a Forgejo dump, published in
// the forgejo_backup field of probes.json.
type ForgejoBackup struct {
	// LastGood is the last complete dump; zero is not a dump at the epoch.
	LastGood time.Time `json:"last_good"`
}
