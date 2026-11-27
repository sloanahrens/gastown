package openshellprov

import "fmt"

// MissingSeatProviders returns, in the order given, the names whose provider
// forgejo-seat-NAME the gateway does not list. A name CreateSeatProviders would
// refuse is missing and is returned as given, never rewritten. One provider list
// answers every name, and a failed list returns an error and no names.
func (c Client) MissingSeatProviders(names []string) ([]string, error) {
	stdout, _, err := c.Run.Run(openshellBinary, c.args("provider", "list"), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: openshell provider list failed, so which seats have no provider is unknown", Name)
	}
	var missing []string
	for _, name := range names {
		if !seatNameValid(name) || !firstColumnIs(stdout, seatProviderPrefix+name) {
			missing = append(missing, name)
		}
	}
	return missing, nil
}
