package idle

import (
	"context"
	"log/slog"
	"time"

	"github.com/adityashantanu/ai-agent-service/internal/sandbox"
)

// CronWaker resumes suspended sandboxes just before their next Hermes cron
// job is due (annotation captured at suspend time), grants a grace window so
// the idle sweeper doesn't kill the job, and nudges the in-pod scheduler via
// `hermes cron tick` for an immediate fire. See docs/cron-wake-design.md.
type CronWaker struct {
	Resolver  *sandbox.Resolver
	Lifecycle *sandbox.Lifecycle

	// Grace protects a cron-woken sandbox from idle suspension.
	Grace time.Duration
	// WakeTimeout bounds the resume wait.
	WakeTimeout time.Duration
	// Interval between waker sweeps (default 30s). Wakes fire for anything
	// due within the next interval, so jobs run at-or-slightly-before time.
	Interval time.Duration

	now func() time.Time // injectable for tests
}

func (w *CronWaker) interval() time.Duration {
	if w.Interval == 0 {
		return 30 * time.Second
	}
	return w.Interval
}

func (w *CronWaker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// Run blocks until ctx is done.
func (w *CronWaker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

// SweepOnce is a test hook running a single sweep synchronously.
func (w *CronWaker) SweepOnce(ctx context.Context) { w.sweep(ctx) }

func (w *CronWaker) sweep(ctx context.Context) {
	users, err := w.Resolver.List(ctx)
	if err != nil {
		slog.Error("cron waker: list users", "err", err)
		return
	}
	now := w.clock()
	for _, ua := range users {
		if ua.NextCronWake.IsZero() || ua.NextCronWake.After(now.Add(w.interval())) {
			continue
		}
		if ua.State != sandbox.StateSuspended && ua.State != sandbox.StateSuspending {
			// Already running (user came back on their own): the in-pod
			// scheduler handles the job; just clear the stale marker.
			_ = w.Lifecycle.SetClaimAnnotations(ctx, ua.UserID, map[string]*string{
				sandbox.AnnotationNextCronWake: nil,
			})
			continue
		}

		slog.Info("cron waker: resuming for scheduled job", "user", ua.UserID, "due", ua.NextCronWake)
		graceUntil := now.Add(w.Grace).UTC().Format(time.RFC3339)
		// Grace goes on FIRST: if the resume is slow, the sweeper must
		// already see the protection when the pod comes up.
		if err := w.Lifecycle.SetClaimAnnotations(ctx, ua.UserID, map[string]*string{
			sandbox.AnnotationCronGraceUntil: &graceUntil,
			sandbox.AnnotationNextCronWake:   nil,
		}); err != nil {
			slog.Error("cron waker: annotate", "user", ua.UserID, "err", err)
			continue
		}
		woken, err := w.Lifecycle.Resume(ctx, ua.UserID, w.WakeTimeout, "cron")
		if err != nil {
			slog.Error("cron waker: resume failed (job will catch up on next wake)",
				"user", ua.UserID, "err", err)
			continue
		}
		// Immediate deterministic fire; the built-in 60s ticker + boot
		// catch-up are the safety net if this exec fails.
		if w.Lifecycle.Exec != nil {
			if pod, perr := w.Resolver.PodName(ctx, woken); perr == nil {
				if _, stderr, xerr := w.Lifecycle.Exec.Exec(ctx, w.Lifecycle.Namespace, pod, "hermes",
					[]string{"hermes", "cron", "tick"}); xerr != nil {
					slog.Warn("cron waker: tick exec failed; 60s ticker will fire",
						"user", ua.UserID, "err", xerr, "stderr", stderr)
				}
			}
		}
	}
}
