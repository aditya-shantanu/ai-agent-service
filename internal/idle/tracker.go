// Package idle tracks per-user activity and suspends idle agents.
package idle

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/aditya-shantanu/ai-agent-service/internal/sandbox"
)

type entry struct {
	watchSince   time.Time // when tracking began (bookkeeping, not an activity)
	lastActivity time.Time // real activities only
	prevActivity time.Time // the activity before last: gap(prev,last) detects conversations
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
		e = &entry{watchSince: t.now()}
		t.entries[user] = e
	}
	return e
}

// Observe starts the idle clock for a user WITHOUT recording an activity
// pair — used at first sight (gateway restart, sweep discovery) so the
// bookkeeping itself can't masquerade as a conversation.
func (t *Tracker) Observe(user string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.get(user) // get() initializes watchSince=now; no activity recorded
}

// Touch marks activity for the user.
func (t *Tracker) Touch(user string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.get(user)
	e.prevActivity, e.lastActivity = e.lastActivity, t.now()
}

// Begin marks a request in flight; the returned func ends it (defer it).
func (t *Tracker) Begin(user string) func() {
	t.mu.Lock()
	e := t.get(user)
	e.prevActivity, e.lastActivity = e.lastActivity, t.now()
	e.inflight++
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		e.inflight--
		e.prevActivity, e.lastActivity = e.lastActivity, t.now()
		t.mu.Unlock()
	}
}

// Forget drops tracking state (user deleted).
func (t *Tracker) Forget(user string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, user)
}

// snapshot reports (idleFor, activityGap, inflight, seen) for a user.
// activityGap is the spacing between the user's last two activities — small
// gaps mean an active conversation.
func (t *Tracker) snapshot(user string) (time.Duration, time.Duration, int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[user]
	if !ok {
		return 0, 0, 0, false
	}
	gap := time.Duration(1<<62 - 1) // no prior activity pair = not a conversation
	if !e.prevActivity.IsZero() {
		gap = e.lastActivity.Sub(e.prevActivity)
	}
	ref := e.lastActivity
	if ref.IsZero() {
		ref = e.watchSince // never active: idle since we started watching
	}
	return t.now().Sub(ref), gap, e.inflight, true
}

// Suspender periodically suspends Ready users whose idle time exceeds their
// CURRENT window: BaseTimeout normally, ActiveTimeout while "in a
// conversation" (last two activities within ActiveTimeout of each other).
// Level-1 adaptive suspension: conversations pay the idle tail once, not
// per message (costcalc/COST-REDUCTION.md #5).
type Suspender struct {
	Tracker   *Tracker
	Resolver  *sandbox.Resolver
	Lifecycle *sandbox.Lifecycle
	// IdleTimeout is the BASE window after an isolated interaction.
	IdleTimeout time.Duration
	// ActiveTimeout is the extended window during a conversation.
	// Zero disables adaptivity (base window always — legacy behavior).
	ActiveTimeout        time.Duration
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
		idleFor, gap, inflight, seen := s.Tracker.snapshot(ua.UserID)
		if !seen {
			// First time we see this user (e.g. gateway restart): start the
			// idle clock now rather than suspending immediately. Observe, not
			// Touch: bookkeeping must not pair with the user's next request
			// into a phantom "conversation".
			s.Tracker.Observe(ua.UserID)
			continue
		}
		window := s.IdleTimeout
		if s.ActiveTimeout > 0 && gap <= s.ActiveTimeout {
			window = s.ActiveTimeout // conversation in progress: generous tail
		}
		if inflight > 0 || idleFor < window {
			continue
		}
		slog.Info("idle sweep: suspending", "user", ua.UserID,
			"idle", idleFor.Round(time.Second), "window", window)
		if _, err := s.Lifecycle.Suspend(ctx, ua.UserID); err != nil {
			slog.Error("idle sweep: suspend failed", "user", ua.UserID, "err", err)
		}
	}
}

// SweepOnce is a test hook running a single sweep synchronously.
func (s *Suspender) SweepOnce(ctx context.Context) { s.sweep(ctx) }

// SetNowFunc is a test hook to control the tracker clock.
func (t *Tracker) SetNowFunc(f func() time.Time) { t.now = f }
