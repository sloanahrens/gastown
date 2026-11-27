package attention

import "fmt"

// gatewayUnreachableGrowth is how many calls must fail to reach the gateway's
// database between two reads before the operator is woken: a single failure is
// ordinary, a burst is not (gt-g7rdh4).
const gatewayUnreachableGrowth = 10

// GatewayUnreachableItems returns one medium bd-gateway-unreachable item naming
// the growth when cur is at least gatewayUnreachableGrowth above prev, and none
// when either reading is nil, the counter did not grow, or it fell because a
// gateway restart resets it (gt-g7rdh4).
func GatewayUnreachableItems(prev, cur *uint64) []Item {
	if prev == nil || cur == nil || *cur < *prev {
		return nil
	}
	growth := *cur - *prev
	if growth < gatewayUnreachableGrowth {
		return nil
	}
	return []Item{{
		Key:      RuleBDGatewayUnreachable,
		Kind:     KindBDGatewayUnreachable,
		Severity: SeverityMedium,
		Summary:  fmt.Sprintf("bd gateway: %d calls could not reach its database since the previous read (exit 25)", growth),
	}}
}
