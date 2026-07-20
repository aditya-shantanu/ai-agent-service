package bench_test

import (
	"testing"

	"github.com/adityashantanu/ai-agent-service/internal/bench"
)

func TestSummarize(t *testing.T) {
	cases := []struct {
		name               string
		in                 []float64
		p50, p95, min, max float64
	}{
		{"empty", nil, 0, 0, 0, 0},
		{"single", []float64{42}, 42, 42, 42, 42},
		{"pair", []float64{10, 20}, 15, 19.5, 10, 20},
		{"odd", []float64{30, 10, 20}, 20, 29, 10, 30},
		{"all-equal", []float64{5, 5, 5, 5}, 5, 5, 5, 5},
		{"twenty", seq(1, 20), 10.5, 19.05, 1, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := bench.Summarize(c.in)
			if s.Count != len(c.in) {
				t.Errorf("count = %d", s.Count)
			}
			for _, chk := range []struct {
				name      string
				got, want float64
			}{{"p50", s.P50MS, c.p50}, {"p95", s.P95MS, c.p95}, {"min", s.MinMS, c.min}, {"max", s.MaxMS, c.max}} {
				if diff := chk.got - chk.want; diff > 0.0001 || diff < -0.0001 {
					t.Errorf("%s = %v, want %v", chk.name, chk.got, chk.want)
				}
			}
		})
	}
}

func TestSummarizeDoesNotMutateInput(t *testing.T) {
	in := []float64{3, 1, 2}
	bench.Summarize(in)
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Errorf("input mutated: %v", in)
	}
}

func seq(from, to int) []float64 {
	var out []float64
	for i := from; i <= to; i++ {
		out = append(out, float64(i))
	}
	return out
}
