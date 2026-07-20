package bench_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adityashantanu/ai-agent-service/internal/bench"
)

func TestComputeComparisons(t *testing.T) {
	r := run(map[string]*bench.ScenarioResult{
		bench.ScenarioResume:   result(4000, 4100, 4200),
		bench.ScenarioBaseline: result(100, 150, 200),
		bench.ScenarioNewWarm:  result(2000),
		bench.ScenarioNewCold:  result(30000),
	})
	tax, ok := r.Comparisons[bench.ComparisonSuspendTax]
	if !ok || tax.P50 != 4100-150 {
		t.Errorf("suspend tax = %+v", tax)
	}
	cold, ok := r.Comparisons[bench.ComparisonColdTax]
	if !ok || cold.P50 != 28000 {
		t.Errorf("cold tax = %+v", cold)
	}
	if _, ok := r.Comparisons[bench.ComparisonTTFTTax]; ok {
		t.Error("ttft tax must be absent when ttft scenarios did not run")
	}
}

func TestComputeComparisonsSkipsEmpty(t *testing.T) {
	r := run(map[string]*bench.ScenarioResult{
		bench.ScenarioResume:   result(), // no samples
		bench.ScenarioBaseline: result(100),
	})
	if _, ok := r.Comparisons[bench.ComparisonSuspendTax]; ok {
		t.Error("comparison against an empty summary must be skipped")
	}
}

func TestRenderHuman(t *testing.T) {
	r := run(map[string]*bench.ScenarioResult{
		bench.ScenarioResume:   result(3980, 4310, 4400),
		bench.ScenarioBaseline: result(142, 301, 420),
		bench.ScenarioNewCold:  {Skipped: true, SkipReason: "pool drain not allowed", Errors: []bench.ErrorEvent{}},
	})
	r.Check = &bench.CheckResult{Enabled: true, BudgetFile: "bench/budgets-kind.yaml", Violations: []bench.Violation{}}

	var sb strings.Builder
	r.RenderHuman(&sb)
	out := sb.String()
	for _, want := range []string{
		"Suspension UX tax",
		"baseline-always-alive",
		"resume-suspended",
		"SKIPPED (pool drain not allowed)",
		"Budget check: OK",
		"Error events: 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func TestWriteJSONRoundTrip(t *testing.T) {
	r := run(map[string]*bench.ScenarioResult{
		bench.ScenarioBaseline: result(100, 200),
	})
	path := filepath.Join(t.TempDir(), "sub", "snap.json")
	if err := r.WriteJSON(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back bench.RunResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Scenarios[bench.ScenarioBaseline].Summary.P50MS != 150 {
		t.Errorf("round trip p50 = %v", back.Scenarios[bench.ScenarioBaseline].Summary.P50MS)
	}
}
