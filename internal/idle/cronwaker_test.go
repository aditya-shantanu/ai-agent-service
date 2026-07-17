package idle

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	"github.com/adityashantanu/ai-agent-service/internal/sandbox"
)

func suspendUser(t *testing.T, clients *sandbox.Clients, user string, nextWake time.Time) {
	t.Helper()
	ctx := context.Background()
	sb, err := clients.Core.AgentsV1beta1().Sandboxes(ns).Get(ctx, "hermes-pool-"+user, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	sb.Status.Conditions = []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse},
		{Type: "Suspended", Status: metav1.ConditionTrue, Reason: "PodTerminated"},
	}
	if _, err := clients.Core.AgentsV1beta1().Sandboxes(ns).Update(ctx, sb, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	claim, err := clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Get(ctx, sandbox.ClaimName(user), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	claim.Annotations[sandbox.AnnotationNextCronWake] = nextWake.UTC().Format(time.RFC3339)
	if _, err := clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Update(ctx, claim, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func newWakerFixture(t *testing.T) (*CronWaker, *sandbox.Clients, *time.Time) {
	t.Helper()
	s, _, clients, now := newSuspenderFixture(t)
	w := &CronWaker{
		Resolver:    s.Resolver,
		Lifecycle:   s.Lifecycle,
		Grace:       2 * time.Minute,
		WakeTimeout: 100 * time.Millisecond,
		Interval:    30 * time.Second,
		now:         func() time.Time { return *now },
	}
	return w, clients, now
}

func TestWakerResumesDueUser(t *testing.T) {
	w, clients, now := newWakerFixture(t)
	seedReadyUser(t, clients, "cronny", false)
	ctx := context.Background()
	suspendUser(t, clients, "cronny", now.Add(20*time.Second)) // due within interval

	w.SweepOnce(ctx)

	sb, _ := clients.Core.AgentsV1beta1().Sandboxes(ns).Get(ctx, "hermes-pool-cronny", metav1.GetOptions{})
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatalf("waker did not resume: %s", sb.Spec.OperatingMode)
	}
	claim, _ := clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Get(ctx, sandbox.ClaimName("cronny"), metav1.GetOptions{})
	if v := claim.Annotations[sandbox.AnnotationNextCronWake]; v != "" {
		t.Errorf("next-cron-wake not cleared: %q", v)
	}
	graceStr := claim.Annotations[sandbox.AnnotationCronGraceUntil]
	grace, err := time.Parse(time.RFC3339, graceStr)
	if err != nil || !grace.After(*now) {
		t.Errorf("cron-grace-until bad: %q", graceStr)
	}
	if claim.Annotations[sandbox.AnnotationLastWakeReason] != "cron" {
		t.Errorf("lastWakeReason = %q", claim.Annotations[sandbox.AnnotationLastWakeReason])
	}
}

func TestWakerIgnoresNotYetDue(t *testing.T) {
	w, clients, now := newWakerFixture(t)
	seedReadyUser(t, clients, "later", false)
	ctx := context.Background()
	suspendUser(t, clients, "later", now.Add(10*time.Minute))

	w.SweepOnce(ctx)

	sb, _ := clients.Core.AgentsV1beta1().Sandboxes(ns).Get(ctx, "hermes-pool-later", metav1.GetOptions{})
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatalf("waker resumed a not-yet-due user")
	}
}

func TestWakerClearsMarkerForRunningUser(t *testing.T) {
	w, clients, now := newWakerFixture(t)
	seedReadyUser(t, clients, "awake", false)
	ctx := context.Background()
	// Running user with a stale due marker (came back on their own).
	claim, _ := clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Get(ctx, sandbox.ClaimName("awake"), metav1.GetOptions{})
	claim.Annotations[sandbox.AnnotationNextCronWake] = now.Add(-time.Minute).UTC().Format(time.RFC3339)
	_, _ = clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Update(ctx, claim, metav1.UpdateOptions{})

	w.SweepOnce(ctx)

	claim, _ = clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Get(ctx, sandbox.ClaimName("awake"), metav1.GetOptions{})
	if v := claim.Annotations[sandbox.AnnotationNextCronWake]; v != "" {
		t.Errorf("stale marker not cleared for running user: %q", v)
	}
	if v := claim.Annotations[sandbox.AnnotationCronGraceUntil]; v != "" {
		t.Errorf("running user must not get a grace window: %q", v)
	}
}

func TestSweeperHonorsCronGrace(t *testing.T) {
	s, _, clients, now := newSuspenderFixture(t)
	seedReadyUser(t, clients, "graced", false)
	ctx := context.Background()

	claim, _ := clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Get(ctx, sandbox.ClaimName("graced"), metav1.GetOptions{})
	claim.Annotations[sandbox.AnnotationCronGraceUntil] = now.Add(2 * time.Minute).UTC().Format(time.RFC3339)
	_, _ = clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Update(ctx, claim, metav1.UpdateOptions{})

	// Way past the idle timeout, but inside the cron grace: must NOT suspend.
	s.SweepOnce(ctx)
	*now = now.Add(time.Minute)
	s.SweepOnce(ctx)
	if m := modeOf(t, clients, "graced"); m != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatalf("suspended inside cron grace: %s", m)
	}

	// Past the grace: the first sweep starts the idle clock (the user was
	// never tracked while grace-skipped), the next sweep past the idle
	// timeout suspends normally.
	*now = now.Add(15 * time.Minute)
	s.SweepOnce(ctx)
	*now = now.Add(11 * time.Minute)
	s.SweepOnce(ctx)
	if m := modeOf(t, clients, "graced"); m != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatalf("not suspended after grace expired: %s", m)
	}
}
