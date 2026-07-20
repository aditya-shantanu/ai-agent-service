package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

// Comparison keys in RunResult.Comparisons.
const (
	ComparisonSuspendTax = "suspendUXTaxMS" // resume p50/p95 minus baseline
	ComparisonColdTax    = "coldCreateTaxMS"
	ComparisonTTFTTax    = "ttftSuspendTaxMS"
)

// ComparisonMS is a derived delta between two scenario summaries.
type ComparisonMS struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95,omitempty"`
}

// CheckResult records the budget evaluation of a run.
type CheckResult struct {
	Enabled    bool        `json:"enabled"`
	BudgetFile string      `json:"budgetFile,omitempty"`
	Violations []Violation `json:"violations"`
	Warnings   []string    `json:"warnings,omitempty"`
}

// RunResult is the full benchmark outcome; serialized as the JSON snapshot.
type RunResult struct {
	Env         string                     `json:"env"`
	Gateway     string                     `json:"gateway"`
	StartedAt   time.Time                  `json:"startedAt"`
	FinishedAt  time.Time                  `json:"finishedAt"`
	GitCommit   string                     `json:"gitCommit,omitempty"`
	Scenarios   map[string]*ScenarioResult `json:"scenarios"`
	Comparisons map[string]ComparisonMS    `json:"comparisons,omitempty"`
	Check       *CheckResult               `json:"check,omitempty"`
}

// ComputeComparisons derives the deltas the whole exercise exists for:
// what does the cost-optimized lifecycle cost the user versus an
// always-alive agent?
func (r *RunResult) ComputeComparisons() {
	r.Comparisons = map[string]ComparisonMS{}
	delta := func(key, a, b string) {
		sa, oka := r.Scenarios[a]
		sb, okb := r.Scenarios[b]
		if !oka || !okb || sa.Summary.Count == 0 || sb.Summary.Count == 0 {
			return
		}
		r.Comparisons[key] = ComparisonMS{
			P50: sa.Summary.P50MS - sb.Summary.P50MS,
			P95: sa.Summary.P95MS - sb.Summary.P95MS,
		}
	}
	delta(ComparisonSuspendTax, ScenarioResume, ScenarioBaseline)
	delta(ComparisonColdTax, ScenarioNewCold, ScenarioNewWarm)
	delta(ComparisonTTFTTax, ScenarioResumeTTFT, ScenarioBaselineTTFT)
}

// GitCommit returns the vcs revision baked into the binary, if any.
func GitCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}

// WriteJSON writes the snapshot, creating parent directories.
func (r *RunResult) WriteJSON(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func fmtMS(v float64) string {
	switch {
	case v >= 10000:
		return fmt.Sprintf("%.1fs", v/1000)
	case v >= 1000:
		return fmt.Sprintf("%.2fs", v/1000)
	default:
		return fmt.Sprintf("%.0fms", v)
	}
}

// RenderHuman writes the operator-facing report: per-scenario percentiles
// followed by the baseline-vs-optimized comparison block.
func (r *RunResult) RenderHuman(w io.Writer) {
	fmt.Fprintf(w, "\n== UX benchmark: cost-optimized vs always-alive (%s) ==\n", r.Env)
	fmt.Fprintf(w, "  %-24s %9s %9s %9s %9s %9s %4s\n", "SCENARIO", "MIN", "P50", "P90", "P95", "MAX", "N")
	for _, name := range scenarioOrder {
		sr, ok := r.Scenarios[name]
		if !ok {
			continue
		}
		switch {
		case sr.Skipped:
			fmt.Fprintf(w, "  %-24s SKIPPED (%s)\n", name, sr.SkipReason)
		case sr.Failed:
			fmt.Fprintf(w, "  %-24s FAILED (%s)\n", name, sr.FailReason)
		case sr.Summary.Count == 0:
			fmt.Fprintf(w, "  %-24s no successful samples\n", name)
		default:
			s := sr.Summary
			fmt.Fprintf(w, "  %-24s %9s %9s %9s %9s %9s %4d\n",
				name, fmtMS(s.MinMS), fmtMS(s.P50MS), fmtMS(s.P90MS), fmtMS(s.P95MS), fmtMS(s.MaxMS), s.Count)
		}
		for key, vals := range sr.Extra {
			if len(vals) > 0 {
				es := Summarize(vals)
				fmt.Fprintf(w, "  %-24s %9s %9s %9s %9s %9s %4d\n",
					"  · "+key, fmtMS(es.MinMS), fmtMS(es.P50MS), fmtMS(es.P90MS), fmtMS(es.P95MS), fmtMS(es.MaxMS), es.Count)
			}
		}
	}

	fmt.Fprintln(w)
	if c, ok := r.Comparisons[ComparisonSuspendTax]; ok {
		fmt.Fprintf(w, "  Suspension UX tax (resume p50 - baseline p50): +%s (p95 +%s)\n", fmtMS(c.P50), fmtMS(c.P95))
	}
	if c, ok := r.Comparisons[ComparisonColdTax]; ok {
		fmt.Fprintf(w, "  Cold-create tax   (cold p50 - warm p50):       +%s\n", fmtMS(c.P50))
	}
	if c, ok := r.Comparisons[ComparisonTTFTTax]; ok {
		fmt.Fprintf(w, "  TTFT suspend tax  (resume p50 - baseline p50): +%s\n", fmtMS(c.P50))
	}

	errs := 0
	for _, sr := range r.Scenarios {
		errs += len(sr.Errors)
	}
	fmt.Fprintf(w, "  Error events: %d\n", errs)
	if errs > 0 {
		for _, name := range scenarioOrder {
			if sr, ok := r.Scenarios[name]; ok {
				for _, e := range sr.Errors {
					fmt.Fprintf(w, "    %s iter %d: HTTP %d %s\n", name, e.Iteration, e.Status, e.Detail)
				}
			}
		}
	}

	if r.Check != nil && r.Check.Enabled {
		for _, warn := range r.Check.Warnings {
			fmt.Fprintf(w, "  WARN: %s\n", warn)
		}
		if len(r.Check.Violations) == 0 {
			fmt.Fprintf(w, "  Budget check: OK (%s)\n", r.Check.BudgetFile)
		} else {
			fmt.Fprintf(w, "  Budget check: %d VIOLATION(S) (%s)\n", len(r.Check.Violations), r.Check.BudgetFile)
			for _, v := range r.Check.Violations {
				fmt.Fprintf(w, "    FAIL %s\n", v)
			}
		}
	}
}
