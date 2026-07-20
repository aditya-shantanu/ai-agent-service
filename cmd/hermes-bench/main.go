// hermes-bench measures the user-facing latency of the platform's two
// critical moments — first contact (new agent) and coming back (resume) —
// and compares the cost-optimized lifecycle against an always-alive
// baseline agent. Run via `make bench` / `make bench-check` (kind) or
// `make bench-gke`; see bench/README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/adityashantanu/ai-agent-service/internal/bench"
)

func main() {
	var (
		gateway        = flag.String("gateway", "", "gateway base URL, e.g. http://localhost:18080 (required)")
		adminToken     = flag.String("admin-token", os.Getenv("BENCH_ADMIN_TOKEN"), "admin bearer token (or env BENCH_ADMIN_TOKEN)")
		envName        = flag.String("env", "", "environment name: kind | gke (required; picks budgets and defaults)")
		scenarios      = flag.String("scenarios", "baseline,resume,warm,cold", "comma-separated: baseline,resume,warm,cold")
		iterations     = flag.Int("iterations", 0, "override per-scenario iteration defaults (0 = defaults)")
		ttft           = flag.Bool("ttft", false, "add streamed chat time-to-first-token scenarios (costs LLM credits)")
		check          = flag.Bool("check", false, "evaluate latency budgets; exit 1 on violation")
		budgetFile     = flag.String("budget-file", "", "budget YAML (default bench/budgets-<env>.yaml)")
		jsonOut        = flag.String("json-out", "", "snapshot path (default bench/results/<env>-<timestamp>.json, \"-\" = none)")
		userPrefix     = flag.String("user-prefix", "bench", "user ID prefix for benchmark users")
		allowPoolDrain = flag.Bool("allow-pool-drain", false, "allow the cold scenario to scale the warm pool to 0 (degrades real signups while draining)")
		kubeContext    = flag.String("kube-context", "", "kubeconfig context for pool control (empty = current)")
		namespace      = flag.String("namespace", "hermes-users", "namespace of the SandboxWarmPool")
		poolName       = flag.String("pool", "hermes-pool", "SandboxWarmPool name")
		suspendSettle  = flag.Duration("suspend-settle", 10*time.Second, "wait after state flips Suspended (pod termination is async)")
		requestTimeout = flag.Duration("request-timeout", 150*time.Second, "per-request timeout (must exceed provision/wake holds)")
		noKube         = flag.Bool("no-kube", false, "never touch the Kubernetes API (cold skipped, fixed restock waits)")
	)
	flag.Parse()

	if *gateway == "" || *envName == "" || *adminToken == "" {
		fmt.Fprintln(os.Stderr, "hermes-bench: -gateway, -env and -admin-token (or BENCH_ADMIN_TOKEN) are required")
		flag.Usage()
		os.Exit(2)
	}
	if *envName != "kind" && *envName != "gke" {
		fmt.Fprintln(os.Stderr, "hermes-bench: -env must be kind or gke")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+format+"\n", args...)
	}

	restockWait := 60 * time.Second
	if *envName == "gke" {
		restockWait = 120 * time.Second
	}

	env := &bench.Env{
		Client:        bench.NewClient(*gateway, *adminToken, *requestTimeout),
		EnvName:       *envName,
		UserPrefix:    *userPrefix,
		SuspendSettle: *suspendSettle,
		RestockWait:   restockWait,
		ProbeGap:      2 * time.Second,
		Iterations:    *iterations,
		Logf:          logf,
	}
	if !*noKube {
		pool, err := bench.NewPoolControl(*kubeContext, *namespace, *poolName)
		if err != nil {
			logf("no kube access (%v) — cold scenario will be skipped, warm uses fixed restock waits", err)
		} else {
			env.Pool = pool
		}
	}

	var list []bench.Scenario
	want := map[string]bool{}
	for _, s := range strings.Split(*scenarios, ",") {
		want[strings.TrimSpace(s)] = true
	}
	if want["baseline"] {
		list = append(list, bench.Baseline{})
	}
	if want["resume"] {
		list = append(list, bench.Resume{})
	}
	if want["warm"] {
		list = append(list, bench.NewAgentWarm{})
	}
	if want["cold"] {
		list = append(list, bench.NewAgentCold{AllowDrain: *allowPoolDrain})
	}
	if *ttft {
		if want["baseline"] {
			list = append(list, bench.BaselineTTFT{})
		}
		if want["resume"] {
			list = append(list, bench.ResumeTTFT{})
		}
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "hermes-bench: no scenarios selected")
		os.Exit(2)
	}

	result := &bench.RunResult{
		Env:       *envName,
		Gateway:   *gateway,
		StartedAt: time.Now().UTC(),
		GitCommit: bench.GitCommit(),
	}
	result.Scenarios = bench.RunAll(ctx, env, list)
	result.FinishedAt = time.Now().UTC()
	result.ComputeComparisons()

	exit := 0
	if *check {
		bf := *budgetFile
		if bf == "" {
			bf = "bench/budgets-" + *envName + ".yaml"
		}
		budget, err := bench.LoadBudget(bf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hermes-bench: load budget: %v\n", err)
			os.Exit(2)
		}
		violations, warnings := budget.Evaluate(result)
		result.Check = &bench.CheckResult{Enabled: true, BudgetFile: bf, Violations: violations, Warnings: warnings}
		if len(violations) > 0 {
			exit = 1
		}
	}

	out := *jsonOut
	if out == "" {
		out = fmt.Sprintf("bench/results/%s-%s.json", *envName, result.StartedAt.Format("20060102T150405Z"))
	}
	wroteSnapshot := false
	if out != "-" {
		if err := result.WriteJSON(out); err != nil {
			fmt.Fprintf(os.Stderr, "hermes-bench: write snapshot: %v\n", err)
			exit = 2
		} else {
			wroteSnapshot = true
		}
	}

	result.RenderHuman(os.Stdout)
	if wroteSnapshot {
		fmt.Printf("  Snapshot: %s\n", out)
	}

	// A run interrupted by Ctrl-C should not masquerade as a clean pass.
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "hermes-bench: interrupted")
		if exit == 0 {
			exit = 2
		}
	}
	os.Exit(exit)
}
