package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Cron-wake annotations on the claim (see docs/cron-wake-design.md).
const (
	// AnnotationNextCronWake holds the RFC3339 time the cron waker should
	// resume this user's sandbox. Captured from cron/jobs.json at suspend.
	AnnotationNextCronWake = "hermes.ai-agent-service.dev/next-cron-wake"
	// AnnotationCronGraceUntil protects a cron-woken sandbox from the idle
	// sweeper until this RFC3339 time.
	AnnotationCronGraceUntil = "hermes.ai-agent-service.dev/cron-grace-until"
	// AnnotationLastWakeReason records why the sandbox last resumed:
	// "connect", "api", or "cron".
	AnnotationLastWakeReason = "hermes.ai-agent-service.dev/last-wake-reason"
)

const jobsJSONPath = "/opt/data/cron/jobs.json"

// hermesJob is the subset of a cron/jobs.json record the platform reads.
type hermesJob struct {
	ID        string          `json:"id"`
	Enabled   *bool           `json:"enabled"` // absent = enabled (Hermes semantics)
	NextRunAt string          `json:"next_run_at"`
	Schedule  json.RawMessage `json:"schedule"`
}

// EarliestCronRun parses a Hermes cron/jobs.json payload and returns the
// earliest next_run_at among enabled jobs. ok=false means "no enabled jobs
// with a next run" (not an error). Individual malformed jobs are skipped;
// only an unparseable document is an error.
func EarliestCronRun(jobsJSON []byte) (time.Time, bool, error) {
	if len(jobsJSON) == 0 {
		return time.Time{}, false, nil
	}
	// Top level is a dict {"jobs": [...]} (expected) or a bare list
	// (accepted by Hermes' own loader).
	var doc struct {
		Jobs []hermesJob `json:"jobs"`
	}
	var jobs []hermesJob
	if err := json.Unmarshal(jobsJSON, &doc); err == nil && doc.Jobs != nil {
		jobs = doc.Jobs
	} else if err := json.Unmarshal(jobsJSON, &jobs); err != nil {
		return time.Time{}, false, fmt.Errorf("jobs.json: unrecognized shape: %w", err)
	}

	var earliest time.Time
	found := false
	for _, j := range jobs {
		if j.Enabled != nil && !*j.Enabled {
			continue
		}
		if j.NextRunAt == "" {
			continue
		}
		t, err := parseHermesTime(j.NextRunAt)
		if err != nil {
			slog.Warn("cronpeek: skipping job with bad next_run_at", "job", j.ID, "value", j.NextRunAt)
			continue
		}
		if !found || t.Before(earliest) {
			earliest = t
			found = true
		}
	}
	return earliest, found, nil
}

// parseHermesTime accepts the ISO-8601 variants Hermes emits (tz-aware
// isoformat, with or without fractional seconds; naive timestamps are
// treated as UTC — the container runs UTC).
func parseHermesTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable timestamp %q", s)
}

// CaptureCronWake reads the user's jobs.json from the running pod and
// records (or clears) the next-cron-wake annotation on the claim.
// Best-effort by design: on any failure we log and return the error, but
// callers proceed with suspension — Hermes' boot catch-up turns a missed
// wake into a late job on the next resume (design decision: suspend-anyway).
func (l *Lifecycle) CaptureCronWake(ctx context.Context, ua *UserAgent) error {
	if l.Exec == nil {
		return nil
	}
	pod, err := l.Resolver.PodName(ctx, ua)
	if err != nil {
		return fmt.Errorf("resolve pod: %w", err)
	}
	stdout, _, err := l.Exec.Exec(ctx, l.Namespace, pod, "hermes",
		[]string{"sh", "-c", "cat " + jobsJSONPath + " 2>/dev/null || true"})
	if err != nil {
		return fmt.Errorf("read jobs.json: %w", err)
	}
	next, ok, err := EarliestCronRun([]byte(stdout))
	if err != nil {
		return err
	}
	if !ok {
		return l.SetClaimAnnotations(ctx, ua.UserID, map[string]*string{AnnotationNextCronWake: nil})
	}
	val := next.UTC().Format(time.RFC3339)
	return l.SetClaimAnnotations(ctx, ua.UserID, map[string]*string{AnnotationNextCronWake: &val})
}
