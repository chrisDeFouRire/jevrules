package validator

import (
	"context"
	"regexp"
	"strings"
	"sync/atomic"
)

// Mock is an offline validator for tests and dry runs (`--validator mock`).
// A rule is "violated" when the content contains `JEVRULES-VIOLATES:<id>` or
// matches one of the rule's Patterns.
type Mock struct {
	Patterns map[string][]*regexp.Regexp
	Calls    atomic.Int64
}

func (m *Mock) Name() string  { return "mock" }
func (m *Mock) Model() string { return "mock" }

func (m *Mock) Validate(_ context.Context, req Request) (Result, error) {
	m.Calls.Add(1)
	res := Result{Probabilities: map[string]float64{}, Model: "mock", InputTokens: EstimateTokens(req.State, req.Questions)}
	res.Choices = map[string]map[string]float64{}
	for id, q := range req.Questions {
		if q.Raw { // pinpoint: pick the line carrying the marker
			probs := map[string]float64{"none": 1}
			if crit, ok := q.Criteria.(map[string]any); ok {
				for k, v := range crit {
					if s, _ := v.(string); k != "none" && strings.Contains(s, "JEVRULES-VIOLATES:"+id) {
						probs = map[string]float64{k: 1, "none": 0}
					}
				}
			}
			res.Choices[id] = probs
			continue
		}
		p := 0.05
		if strings.Contains(req.State.Content, "JEVRULES-VIOLATES:"+id) {
			p = 0.95
		}
		for _, re := range m.Patterns[id] {
			if re.MatchString(req.State.Content) {
				p = 0.95
			}
		}
		res.Probabilities[id] = p
	}
	return res, nil
}
