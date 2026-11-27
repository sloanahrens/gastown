package hostprobe

import "time"

// GoCacheTrim is when this host last trimmed its Go build cache, published in
// the go_cache_trim field of probes.json.
type GoCacheTrim struct {
	// LastGood is the last successful trim; zero is not a trim at the epoch.
	LastGood time.Time `json:"last_good"`
}
