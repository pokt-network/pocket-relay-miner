package observability

import "github.com/prometheus/client_golang/prometheus"

// EagerCounterChildren creates a CounterVec's child series before anything
// increments them.
//
// WHY, and it is the whole reason this function exists rather than a comment
// repeated at each call site: a *Vec with no children exports NOTHING. Until
// the first Inc, the series is not absent-and-zero, it is absent. An operator
// alerting on rate(...[5m]) gets no data, and "no data" reads as "I am not
// measuring this" -- which is indistinguishable from "this never happened".
//
// That is worst for exactly the metrics that matter most: the ones that count a
// failure. A counter whose whole purpose is to make a silent failure audible is
// itself silent until the failure occurs, so the dashboard cannot tell a
// healthy fleet from an uninstrumented one.
//
// Only for single-label vectors, which is what the callers have; a vector with
// more labels needs its combinations enumerated deliberately, not swept up by a
// helper.
func EagerCounterChildren(vec *prometheus.CounterVec, labelValues ...string) {
	for _, v := range labelValues {
		vec.WithLabelValues(v)
	}
}
