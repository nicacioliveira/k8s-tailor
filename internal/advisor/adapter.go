package advisor

import (
	"encoding/json"
	"fmt"
)

// DecisionFromJSON builds a DecisionInput from the JSON encoding of a
// recommender.Recommendation (the tags match), so wiring code never needs the
// advisor to import the recommender/k8s packages:
//
//	raw, _ := json.Marshal(rec) // rec is recommender.Recommendation
//	in, err := advisor.DecisionFromJSON(raw)
//	review, err := adv.Review(ctx, in)
//
// Unknown fields (autoscaler, knative, pdbName, ...) are ignored on purpose:
// they never reach the model.
func DecisionFromJSON(raw []byte) (DecisionInput, error) {
	var in DecisionInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return in, nil
}

// DecisionsFromJSON ingests a JSON array of recommendations.
func DecisionsFromJSON(raw []byte) ([]DecisionInput, error) {
	var in []DecisionInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return in, nil
}
