// Package idle tracks per-user activity and suspends idle agents.
package idle

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/adityashantanu/ai-agent-service/internal/sandbox"
)

type entry struct {
	lastActivity time.Time
	inflight     int
}

// Tracker records last activity + in-flight requests per user. In-flight
// covers WebSockets too: an upgraded connection keeps its handler (and thus
// the counter) held until the socket closes.
type Tracker struct {
	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time // injectable for tests
}

func NewTracker() *Tracker {
	return &Tracker{entries: map[string]*entry{}, now: time.Now}
}

func (t *Tracker) get(user string) *entry {
	e, ok := t.entries[user]
	if !ok {
		e = &entry{lastActivity: t.now()}
		t.entries[user] = e
	}
	return e
}

// Touch marks activity for the user.
func (t *Tracker) Touch(user string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.get(user).lastActivity = t.now()
}

// Begin marks a request in flight; the returned func ends it (defer it).
func (t *Tracker) Begin(user string) func() {
	t.mu.Lock()
	e := t.get(user)
	e.lastActivity = t.now()
	e.inflight++
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		e.inflight--
		e.lastActivity = t.now()
		t.mu.Unlock()
	}
}

// Forget drops tracking state (user deleted).
func (t *Tracker) Forget(user string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, user)
}

// IdleSince reports (idleFor, inflight) for a user. Unknown users are treated
// as idle since the tracker started following them (i.e., never seen: 0).
func (t *Tracker) snapshot(user string) (time.Duration, int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[user]
	if !ok {
		return 0, 0, false
	}
	return t.now().Sub(e.lastActivity), e.inflight, true
}

// Suspender periodically suspends Ready users idle beyond IdleTimeout.
type Suspender struct {
	Tracker              *Tracker
	Resolver             *sandbox.Resolver
	Lifecycle            *sandbox.Lifecycle
	IdleTimeout          time.Duration
	SuspendTelegramUsers bool // if false, suspend-exempt users are skipped
	Interval             time.Duration
}

// Run blocks until ctx is done, ticking every Interval.
func (s *Suspender) Run(ctx context.Context) {
	interval := s.Interval
	if interval == 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// sweep runs one idle-check pass. Exported-ish for tests via SweepOnce.
func (s *Suspender) sweep(ctx context.Context) {
	users, err := s.Resolver.List(ctx)
	if err != nil {
		slog.Error("idle sweep: list users", "err", err)
		return
	}
	for _, ua := range users {
		if ua.State != sandbox.StateReady {
			continue
		}
		if ua.Exempt && !s.SuspendTelegramUsers {
			continue
		}
		// Cron-woken sandboxes get a grace window to run their job
		// (docs/cron-wake-design.md); the sweeper must not race it.
		if !ua.CronGraceUntil.IsZero() && s.Tracker.now().Before(ua.CronGraceUntil) {
			continue
		}
		idleFor, inflight, seen := s.Tracker.snapshot(ua.UserID)
		if !seen {
			// First time we see this user (e.g. gateway restart): start the
			// idle clock now rather than suspending immediately.
			s.Tracker.Touch(ua.UserID)
			continue
		}
		if inflight > 0 || idleFor < s.IdleTimeout {
			continue
		}
		slog.Info("idle sweep: suspending", "user", ua.UserID, "idle", idleFor.Round(time.Second))
		if _, err := s.Lifecycle.Suspend(ctx, ua.UserID); err != nil {
			slog.Error("idle sweep: suspend failed", "user", ua.UserID, "err", err)
		}
	}
}

// SweepOnce is a test hook running a single sweep synchronously.
func (s *Suspender) SweepOnce(ctx context.Context) { s.sweep(ctx) }

// SetNowFunc is a test hook to control the tracker clock.
func (t *Tracker) SetNowFunc(f func() time.Time) { t.now = f }
