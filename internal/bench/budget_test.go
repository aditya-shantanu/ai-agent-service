package bench_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aditya-shantanu/ai-agent-service/internal/bench"
)

const budgetYAML = `env: kind
allowedWakeErrors: 0
required:
  - resume-suspended
scenarios:
  resume-suspended:      { p50: 8000, max: 20000 }
  baseline-always-alive: { p50: 250, p95: 1000 }
  new-agent-cold:        { p50: 30000 }
comparisons:
  suspendUXTaxP50Max: 10000
`

func loadTestBudget(t *testing.T) *bench.Budget {
	t.Helper()
	path := filepath.Join(t.TempDir(), "budgets.yaml")
	if err := os.WriteFile(path, []byte(budgetYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := bench.LoadBudget(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func run(scenarios map[string]*bench.ScenarioResult) *bench.RunResult {
	r := &bench.RunResult{Env: "kind", Scenarios: scenarios}
	r.ComputeComparisons()
	return r
}

func result(samples ...float64) *bench.ScenarioResult {
	return &bench.ScenarioResult{SamplesMS: samples, Summary: bench.Summarize(samples), Errors: []bench.ErrorEvent{}}
}

func TestBudgetLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("env: kind\nscenariosss: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := bench.LoadBudget(path); err == nil {
		t.Fatal("unknown field must fail (typo protection)")
	}
}

func TestBudgetEvaluatePass(t *testing.T) {
	b := loadTestBudget(t)
	r := run(map[string]*bench.ScenarioResult{
		bench.ScenarioResume:   result(4000, 4200, 4100),
		bench.ScenarioBaseline: result(140, 150, 160),
	})
	viols, warns := b.Evaluate(r)
	if len(viols) != 0 {
		t.Fatalf("violations: %v", viols)
	}
	// new-agent-cold budgeted but did not run -> warning, not violation.
	if len(warns) != 1 || !strings.Contains(warns[0], "new-agent-cold") {
		t.Errorf("warnings = %v", warns)
	}
}

func TestBudgetEvaluateLatencyViolations(t *testing.T) {
	b := loadTestBudget(t)
	r := run(map[string]*bench.ScenarioResult{
		bench.ScenarioResume:   result(9000, 25000), // p50 9000>8000... p50 = 17000 actually
		bench.ScenarioBaseline: result(300, 1200),   // p50 750>250, p95>1000
	})
	viols, _ := b.Evaluate(r)
	metrics := map[string]bool{}
	for _, v := range viols {
		metrics[v.Scenario+"/"+v.Metric] = true
	}
	for _, want := range []string{
		"resume-suspended/p50", "resume-suspended/max",
		"baseline-always-alive/p50", "baseline-always-alive/p95",
	} {
		if !metrics[want] {
			t.Errorf("missing violation %s in %v", want, viols)
		}
	}
}

func TestBudgetEvaluateRequiredSkip(t *testing.T) {
	b := loadTestBudget(t)
	skipped := &bench.ScenarioResult{Skipped: true, SkipReason: "no kube", Errors: []bench.ErrorEvent{}}
	r := run(map[string]*bench.ScenarioResult{
		bench.ScenarioResume:   skipped,
		bench.ScenarioBaseline: result(100),
	})
	viols, _ := b.Evaluate(r)
	found := false
	for _, v := range viols {
		if v.Scenario == bench.ScenarioResume && strings.Contains(v.Detail, "required") {
			found = true
		}
	}
	if !found {
		t.Errorf("required skipped scenario must violate: %v", viols)
	}
}

func TestBudgetEvaluateErrorEvents(t *testing.T) {
	b := loadTestBudget(t)

	// Wake 503 WITH Retry-After in a resume scenario: counted, and with
	// allowedWakeErrors=0 one event is already a violation — but not an
	// "error event" violation.
	resume := result(4000, 4100, 4300)
	resume.Errors = append(resume.Errors, bench.ErrorEvent{Iteration: 1, Status: 503, RetryAfter: "10"})
	r := run(map[string]*bench.ScenarioResult{
		bench.ScenarioResume:   resume,
		bench.ScenarioBaseline: result(100),
	})
	viols, _ := b.Evaluate(r)
	if len(viols) != 1 || !strings.Contains(viols[0].Detail, "allowedWakeErrors") {
		t.Errorf("want single allowedWakeErrors violation, got %v", viols)
	}

	// 503 WITHOUT Retry-After is a correctness bug -> plain error violation.
	resume2 := result(4000, 4100, 4300)
	resume2.Errors = append(resume2.Errors, bench.ErrorEvent{Iteration: 1, Status: 503})
	r2 := run(map[string]*bench.ScenarioResult{
		bench.ScenarioResume:   resume2,
		bench.ScenarioBaseline: result(100),
	})
	viols2, _ := b.Evaluate(r2)
	if len(viols2) != 1 || !strings.Contains(viols2[0].Detail, "error event") {
		t.Errorf("503 without Retry-After must be an error-event violation, got %v", viols2)
	}

	// Any error in a non-resume scenario is a violation.
	base := result(100, 110)
	base.Errors = append(base.Errors, bench.ErrorEvent{Iteration: 3, Status: 502, Detail: "bad gateway"})
	r3 := run(map[string]*bench.ScenarioResult{
		bench.ScenarioResume:   result(4000),
		bench.ScenarioBaseline: base,
	})
	viols3, _ := b.Evaluate(r3)
	if len(viols3) != 1 || viols3[0].Scenario != bench.ScenarioBaseline {
		t.Errorf("baseline 502 must violate, got %v", viols3)
	}
}

func TestBudgetEvaluateComparison(t *testing.T) {
	b := loadTestBudget(t)
	r := run(map[string]*bench.ScenarioResult{
		bench.ScenarioResume:   result(15000, 15100, 15200), // tax = 15100-150 > 10000
		bench.ScenarioBaseline: result(140, 150, 160),
	})
	viols, _ := b.Evaluate(r)
	found := false
	for _, v := range viols {
		if v.Scenario == bench.ComparisonSuspendTax {
			found = true
			// resume p50 within its own budget? 15100 > 8000 also fires; both fine.
		}
	}
	if !found {
		t.Errorf("suspend tax must violate: %v", viols)
	}
}
