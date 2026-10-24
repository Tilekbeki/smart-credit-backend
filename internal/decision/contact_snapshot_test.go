package decision

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestContactVersionMetadataPreservesHistoricalCreditPolicy(t *testing.T) {
	f := newFixture()
	input := f.input(t)
	baseline, err := NewEngine().Evaluate(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []any{nil, float64(1), float64(99)} {
		var snapshot map[string]any
		if err := json.Unmarshal(input.FinancialSnapshot, &snapshot); err != nil {
			t.Fatal(err)
		}
		profile := snapshot["company"].(map[string]any)["profile"].(map[string]any)
		if version == nil {
			delete(profile, "version")
		} else {
			profile["version"] = version
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		changed := input
		changed.FinancialSnapshot = raw
		actual, err := NewEngine().Evaluate(changed)
		if err != nil || !reflect.DeepEqual(actual, baseline) {
			t.Fatalf("contact metadata changed credit policy, version=%v err=%v", version, err)
		}
	}
}
