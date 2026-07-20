// Package bench implements the UX/performance benchmark harness behind
// cmd/hermes-bench: gateway client, scenario runners, latency statistics,
// budget gating and report rendering. See bench/README.md for the scenario
// matrix and budget file format.
package bench

import "sort"

// Summary holds latency percentiles over a scenario's samples, in ms.
type Summary struct {
	Count int     `json:"count"`
	MinMS float64 `json:"minMS"`
	P50MS float64 `json:"p50MS"`
	P90MS float64 `json:"p90MS"`
	P95MS float64 `json:"p95MS"`
	MaxMS float64 `json:"maxMS"`
}

// Summarize computes min/p50/p95/max with linear interpolation between
// ranks. An empty input yields a zero Summary (Count 0).
func Summarize(samplesMS []float64) Summary {
	if len(samplesMS) == 0 {
		return Summary{}
	}
	s := append([]float64(nil), samplesMS...)
	sort.Float64s(s)
	return Summary{
		Count: len(s),
		MinMS: s[0],
		P50MS: percentile(s, 50),
		P90MS: percentile(s, 90),
		P95MS: percentile(s, 95),
		MaxMS: s[len(s)-1],
	}
}

func percentile(sorted []float64, p float64) float64 {
	rank := p / 100 * float64(len(sorted)-1)
	lo := int(rank)
	if lo+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[lo+1]-sorted[lo])
}
