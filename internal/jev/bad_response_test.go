package jev

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fp(v float64) *float64 { return &v }

// TestAsk_BadResponseNamesTheCheck feeds one malformed body per check word and
// gets that word back, with the question it failed and the number a range or
// sum check read.
func TestAsk_BadResponseNamesTheCheck(t *testing.T) {
	t.Parallel()
	choice := Question{Type: typeChoice, Instructions: "Which team?", Options: shoeCriteria}
	noul := Question{Type: typeNoul, True: "It is.", False: "It is not."}
	score := Question{Type: typeScore, Levels: []string{"low", "mid", "high"}}
	tests := []struct {
		name     string
		qs       map[string]Question
		body     string
		check    string
		question string
		value    *float64
	}{
		{"decode", map[string]Question{questionID: choice}, `<html>`, CheckDecode, "", nil},
		{"missing answer", map[string]Question{questionID: choice}, `{"answers":{}}`, CheckMissingAnswer, "", nil},
		{"wrong type", map[string]Question{questionID: choice}, `{"answers":{"q":{"type":"noul","noul":0.9}}}`, CheckWrongType, "", nil},
		{"choice not an option", map[string]Question{questionID: choice},
			`{"answers":{"q":{"type":"choice","choice":"refunds","probabilities":{"billing":0,"returns":1,"shipping":0},"confidence":1}}}`,
			CheckChoiceNotAnOption, "", nil},
		{"choice confidence above 1", map[string]Question{questionID: choice},
			`{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0,"returns":1,"shipping":0},"confidence":1.5}}}`,
			CheckConfidenceRange, "", fp(1.5)},
		{"choice probabilities short", map[string]Question{questionID: choice},
			`{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0,"returns":1},"confidence":1}}}`,
			CheckProbabilitiesCount, "", nil},
		{"choice probability for unasked option", map[string]Question{questionID: choice},
			`{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0,"returns":1,"refunds":0},"confidence":1}}}`,
			CheckProbabilitiesUnknownOption, "", nil},
		{"choice probability outside 0 to 1", map[string]Question{questionID: choice},
			`{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":1.5,"returns":0,"shipping":0},"confidence":1}}}`,
			CheckProbabilityRange, "", fp(1.5)},
		{"choice probabilities do not sum to 1", map[string]Question{questionID: choice},
			`{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0.5,"returns":0.5,"shipping":0.5},"confidence":1}}}`,
			CheckProbabilitiesSum, "", fp(1.5)},
		{"choice not most probable", map[string]Question{questionID: choice},
			`{"answers":{"q":{"type":"choice","choice":"billing","probabilities":{"billing":0.1,"returns":0.8,"shipping":0.1},"confidence":1}}}`,
			CheckNotMostProbable, "", nil},
		{"noul missing", map[string]Question{questionID: noul}, `{"answers":{"q":{"type":"noul"}}}`, CheckNoulMissing, "", nil},
		{"noul above 1", map[string]Question{questionID: noul}, `{"answers":{"q":{"type":"noul","noul":1.5}}}`, CheckNoulRange, "", fp(1.5)},
		{"score missing", map[string]Question{questionID: score},
			`{"answers":{"q":{"type":"score","confidence":0.5,"probabilities":{"low":0,"mid":1,"high":0},"legend":{"low":"a","mid":"b","high":"c"}}}}`,
			CheckScoreMissing, "", nil},
		{"score above the top level", map[string]Question{questionID: score},
			`{"answers":{"q":{"type":"score","score":3,"confidence":0.5,"probabilities":{"low":0,"mid":1,"high":0},"legend":{"low":"a","mid":"b","high":"c"}}}}`,
			CheckScoreRange, "", fp(3)},
		{"score confidence above 1", map[string]Question{questionID: score},
			`{"answers":{"q":{"type":"score","score":1,"confidence":2,"probabilities":{"low":0,"mid":1,"high":0},"legend":{"low":"a","mid":"b","high":"c"}}}}`,
			CheckConfidenceRange, "", fp(2)},
		{"score probabilities short", map[string]Question{questionID: score},
			`{"answers":{"q":{"type":"score","score":1,"confidence":0.5,"probabilities":{"mid":1},"legend":{"low":"a","mid":"b","high":"c"}}}}`,
			CheckProbabilitiesCount, "", nil},
		{"score legend short", map[string]Question{questionID: score},
			`{"answers":{"q":{"type":"score","score":1,"confidence":0.5,"probabilities":{"low":0,"mid":1,"high":0},"legend":{"low":"a","mid":"b"}}}}`,
			CheckLegendCount, "", nil},
		{"score legend label without probability", map[string]Question{questionID: score},
			`{"answers":{"q":{"type":"score","score":1,"confidence":0.5,"probabilities":{"low":0,"mid":1,"high":0},"legend":{"low":"a","mid":"b","zzz":"c"}}}}`,
			CheckLegendUnknownLabel, "", nil},
		{"score probability outside 0 to 1", map[string]Question{questionID: score},
			`{"answers":{"q":{"type":"score","score":1,"confidence":0.5,"probabilities":{"low":1.5,"mid":0,"high":0},"legend":{"low":"a","mid":"b","high":"c"}}}}`,
			CheckProbabilityRange, "", fp(1.5)},
		{"score probabilities do not sum to 1", map[string]Question{questionID: score},
			`{"answers":{"q":{"type":"score","score":1,"confidence":0.5,"probabilities":{"low":0.5,"mid":0.5,"high":0.5},"legend":{"low":"a","mid":"b","high":"c"}}}}`,
			CheckProbabilitiesSum, "", fp(1.5)},
		{"several questions name the one that failed", map[string]Question{"a": choice, "b": noul},
			`{"answers":{"a":{"type":"choice","choice":"returns","probabilities":{"billing":0,"returns":1,"shipping":0},"confidence":1},"b":{"type":"noul","noul":2}}}`,
			CheckNoulRange, "b", fp(2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, &respond{body: tt.body})

			_, err := c.Ask(context.Background(), shoeState, tt.qs)

			require.ErrorIs(t, err, ErrBadResponse)
			var bad *BadResponse
			require.ErrorAs(t, err, &bad)
			assert.Equal(t, tt.check, bad.Check)
			assert.Equal(t, tt.question, bad.Question)
			assert.Equal(t, tt.value, bad.Value)
		})
	}
}
