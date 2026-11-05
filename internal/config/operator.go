package config

// LoadOperatorConfig reads the operator's identities from a town's settings
// (operator.names, gt-ywuip). An empty town root, an absent settings file or
// an absent operator section is nil settings and no error: nothing is
// configured. A settings file that is there and does not parse is an error, so
// a caller can fail closed rather than read it as "no identities".
func LoadOperatorConfig(townRoot string) (*OperatorConfig, error) {
	if townRoot == "" {
		return nil, nil
	}
	ts, err := LoadTownSettings(TownSettingsPath(townRoot))
	if err != nil {
		return nil, err
	}
	if ts == nil {
		return nil, nil
	}
	return ts.Operator, nil
}
