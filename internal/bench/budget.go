package bench

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// ScenarioBudget bounds one scenario's summary (ms). Zero fields are not
// enforced.
type ScenarioBudget struct {
	P50 float64 `json:"p50,omitempty"`
	P90 float64 `json:"p90,omitempty"`
	P95 float64 `json:"p95,omitempty"`
	Max float64 `json:"max,omitempty"`
}

// BudgetComparisons bounds the derived deltas.
type BudgetComparisons struct {
	// SuspendUXTaxP50Max caps resume-suspended p50 minus baseline p50 —
	// THE number cost optimizations get measured against.
	SuspendUXTaxP50Max float64 `json:"suspendUXTaxP50Max,omitempty"`
}

// Budget is the per-environment latency contract
// (benchmarks/budgets-<env>.yaml).
type Budget struct {
	Env string `json:"env"`
	// AllowedWakeErrors caps 503+Retry-After events during resume
	// scenarios; any other error event is always a violation.
	AllowedWakeErrors int `json:"allowedWakeErrors"`
	// Required scenarios turn a skip into a violation instead of a warning.
	Required    []string                  `json:"required,omitempty"`
	Scenarios   map[string]ScenarioBudget `json:"scenarios"`
	Comparisons BudgetComparisons         `json:"comparisons,omitempty"`
}

// LoadBudget reads and strictly parses a budget YAML file — unknown fields
// are rejected so a typo can't silently disable a limit.
func LoadBudget(path string) (*Budget, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Budget
	if err := yaml.UnmarshalStrict(raw, &b); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &b, nil
}

// Violation is one budget breach.
type Violation struct {
	Scenario string  `json:"scenario"`
	Metric   string  `json:"metric"`
	ActualMS float64 `json:"actualMS,omitempty"`
	LimitMS  float64 `json:"limitMS,omitempty"`
	Detail   string  `json:"detail,omitempty"`
}

func (v Violation) String() string {
	if v.Detail != "" {
		return fmt.Sprintf("%s: %s", v.Scenario, v.Detail)
	}
	return fmt.Sprintf("%s: %s %.0fms exceeds budget %.0fms", v.Scenario, v.Metric, v.ActualMS, v.LimitMS)
}

// Evaluate checks a run against the budget. Violations gate (exit 1);
// warnings (unbudgeted or skipped-but-not-required scenarios) do not.
func (b *Budget) Evaluate(r *RunResult) (violations []Violation, warnings []string) {
	required := map[string]bool{}
	for _, name := range b.Required {
		required[name] = true
	}

	for name, sb := range b.Scenarios {
		sr, ok := r.Scenarios[name]
		if !ok || sr.Skipped {
			reason := "did not run"
			if ok {
				reason = "skipped: " + sr.SkipReason
			}
			if required[name] {
				violations = append(violations, Violation{Scenario: name, Detail: "required scenario " + reason})
			} else {
				warnings = append(warnings, fmt.Sprintf("%s: budgeted scenario %s", name, reason))
			}
			continue
		}
		if sr.Failed {
			violations = append(violations, Violation{Scenario: name, Detail: "failed: " + sr.FailReason})
			continue
		}
		if sr.Summary.Count == 0 {
			violations = append(violations, Violation{Scenario: name, Detail: "produced no successful samples"})
			continue
		}
		checks := []struct {
			metric string
			actual float64
			limit  float64
		}{
			{"p50", sr.Summary.P50MS, sb.P50},
			{"p90", sr.Summary.P90MS, sb.P90},
			{"p95", sr.Summary.P95MS, sb.P95},
			{"max", sr.Summary.MaxMS, sb.Max},
		}
		for _, c := range checks {
			if c.limit > 0 && c.actual > c.limit {
				violations = append(violations, Violation{Scenario: name, Metric: c.metric, ActualMS: c.actual, LimitMS: c.limit})
			}
		}
	}

	// UX-correctness: error events. 503+Retry-After during a wake is a
	// documented behavior and merely counted; everything else is a bug.
	for name, sr := range r.Scenarios {
		wakeErrors := 0
		for _, e := range sr.Errors {
			if e.Status == 503 && e.RetryAfter != "" && strings.HasPrefix(name, "resume-") {
				wakeErrors++
				continue
			}
			violations = append(violations, Violation{Scenario: name,
				Detail: fmt.Sprintf("error event (iteration %d, HTTP %d): %s", e.Iteration, e.Status, e.Detail)})
		}
		if wakeErrors > b.AllowedWakeErrors {
			violations = append(violations, Violation{Scenario: name,
				Detail: fmt.Sprintf("%d wake 503s exceed allowedWakeErrors=%d", wakeErrors, b.AllowedWakeErrors)})
		}
	}

	if limit := b.Comparisons.SuspendUXTaxP50Max; limit > 0 {
		if c, ok := r.Comparisons[ComparisonSuspendTax]; ok && c.P50 > limit {
			violations = append(violations, Violation{Scenario: ComparisonSuspendTax, Metric: "p50", ActualMS: c.P50, LimitMS: limit})
		}
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Scenario != violations[j].Scenario {
			return violations[i].Scenario < violations[j].Scenario
		}
		return violations[i].Metric < violations[j].Metric
	})
	sort.Strings(warnings)
	return violations, warnings
}
