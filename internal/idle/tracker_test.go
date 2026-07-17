package idle

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sbfake "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	extfake "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"

	"github.com/adityashantanu/ai-agent-service/internal/sandbox"
)

const ns = "hermes-users"

func seedReadyUser(t *testing.T, clients *sandbox.Clients, user string, exempt bool) {
	t.Helper()
	ctx := context.Background()
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes-pool-" + user, Namespace: ns},
		Spec:       sandboxv1beta1.SandboxSpec{OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning},
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
		},
	}
	claim := &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandbox.ClaimName(user),
			Namespace: ns,
			Labels: map[string]string{
				sandbox.LabelManagedBy: sandbox.ManagedByValue,
				sandbox.LabelUser:      user,
			},
			Annotations: map[string]string{},
		},
	}
	if exempt {
		claim.Annotations[sandbox.AnnotationSuspendExempt] = "true"
	}
	claim.Status.SandboxStatus.Name = sb.Name
	if _, err := clients.Core.AgentsV1beta1().Sandboxes(ns).Create(ctx, sb, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func modeOf(t *testing.T, clients *sandbox.Clients, user string) sandboxv1beta1.SandboxOperatingMode {
	t.Helper()
	sb, err := clients.Core.AgentsV1beta1().Sandboxes(ns).Get(context.Background(), "hermes-pool-"+user, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return sb.Spec.OperatingMode
}

func newSuspenderFixture(t *testing.T) (*Suspender, *Tracker, *sandbox.Clients, *time.Time) {
	t.Helper()
	clients := &sandbox.Clients{Core: sbfake.NewSimpleClientset(), Ext: extfake.NewSimpleClientset()}
	resolver := &sandbox.Resolver{Clients: clients, Namespace: ns}
	lc := &sandbox.Lifecycle{Clients: clients, Namespace: ns, Resolver: resolver}
	tracker := NewTracker()
	now := time.Now()
	tracker.SetNowFunc(func() time.Time { return now })
	s := &Suspender{
		Tracker:     tracker,
		Resolver:    resolver,
		Lifecycle:   lc,
		IdleTimeout: 10 * time.Minute,
	}
	return s, tracker, clients, &now
}

func TestSweepSuspendsIdleUser(t *testing.T) {
	s, tracker, clients, now := newSuspenderFixture(t)
	seedReadyUser(t, clients, "alice", false)
	ctx := context.Background()

	// First sweep only starts the idle clock (fresh gateway restart).
	s.SweepOnce(ctx)
	if m := modeOf(t, clients, "alice"); m != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatalf("suspended on first sight: %s", m)
	}

	// Under the timeout: untouched.
	*now = now.Add(5 * time.Minute)
	s.SweepOnce(ctx)
	if m := modeOf(t, clients, "alice"); m != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatalf("suspended too early: %s", m)
	}

	// Past the timeout: suspended.
	*now = now.Add(6 * time.Minute)
	s.SweepOnce(ctx)
	if m := modeOf(t, clients, "alice"); m != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatalf("not suspended after idle timeout: %s", m)
	}
	_ = tracker
}

func TestSweepSkipsActiveAndInflight(t *testing.T) {
	s, tracker, clients, now := newSuspenderFixture(t)
	seedReadyUser(t, clients, "bob", false)
	ctx := context.Background()
	s.SweepOnce(ctx) // start clock

	// In-flight connection (e.g. open WebSocket): never suspended.
	done := tracker.Begin("bob")
	*now = now.Add(time.Hour)
	s.SweepOnce(ctx)
	if m := modeOf(t, clients, "bob"); m != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatalf("suspended with in-flight connection: %s", m)
	}
	done()

	// Activity resets the clock.
	tracker.Touch("bob")
	*now = now.Add(5 * time.Minute)
	s.SweepOnce(ctx)
	if m := modeOf(t, clients, "bob"); m != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatalf("suspended despite recent activity: %s", m)
	}

	*now = now.Add(6 * time.Minute)
	s.SweepOnce(ctx)
	if m := modeOf(t, clients, "bob"); m != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatalf("not suspended after idle: %s", m)
	}
}

func TestSweepSkipsExemptUsers(t *testing.T) {
	s, _, clients, now := newSuspenderFixture(t)
	seedReadyUser(t, clients, "tg", true)
	ctx := context.Background()
	s.SweepOnce(ctx)
	*now = now.Add(2 * time.Hour)
	s.SweepOnce(ctx)
	if m := modeOf(t, clients, "tg"); m != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatalf("exempt user suspended: %s", m)
	}

	// With SuspendTelegramUsers=true the exemption is ignored. The user was
	// never tracked while exempt, so the first eligible sweep starts the
	// clock; the next sweep past the timeout suspends.
	s.SuspendTelegramUsers = true
	s.SweepOnce(ctx)
	*now = now.Add(2 * time.Hour)
	s.SweepOnce(ctx)
	if m := modeOf(t, clients, "tg"); m != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatalf("exemption not overridden: %s", m)
	}
}
