package sandbox

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sbfake "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	extfake "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

const ns = "hermes-users"

// newFixture seeds objects via Create calls: the generated fakes' constructor
// seeding (NewSimpleClientset(objs...)) silently drops objects for these CRD
// types, but Create-then-Get works.
func newFixture(t *testing.T, objs ...runtime.Object) (*Clients, *Resolver, *Provisioner, *Lifecycle) {
	t.Helper()
	core := sbfake.NewSimpleClientset()
	ext := extfake.NewSimpleClientset()
	ctx := context.Background()
	for _, o := range objs {
		var err error
		switch obj := o.(type) {
		case *sandboxv1beta1.Sandbox:
			_, err = core.AgentsV1beta1().Sandboxes(obj.Namespace).Create(ctx, obj, metav1.CreateOptions{})
		case *extv1beta1.SandboxClaim:
			_, err = ext.ExtensionsV1beta1().SandboxClaims(obj.Namespace).Create(ctx, obj, metav1.CreateOptions{})
		default:
			t.Fatalf("unsupported fixture object %T", o)
		}
		if err != nil {
			t.Fatalf("seed %T: %v", o, err)
		}
	}
	clients := &Clients{Core: core, Ext: ext, K8s: k8sfake.NewSimpleClientset()}
	resolver := &Resolver{Clients: clients, Namespace: ns}
	prov := &Provisioner{Clients: clients, Namespace: ns, WarmPoolName: "hermes-pool", Resolver: resolver}
	lc := &Lifecycle{Clients: clients, Namespace: ns, Resolver: resolver}
	return clients, resolver, prov, lc
}

func claimFor(user, sandboxName string) *extv1beta1.SandboxClaim {
	c := &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClaimName(user),
			Namespace: ns,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelUser:      user,
			},
			Annotations: map[string]string{
				AnnotationTokenSHA256: "deadbeef",
			},
		},
		Spec: extv1beta1.SandboxClaimSpec{
			WarmPoolRef: extv1beta1.SandboxWarmPoolRef{Name: "hermes-pool"},
		},
	}
	if sandboxName != "" {
		c.Status.SandboxStatus.Name = sandboxName
	}
	return c
}

func sandboxFor(name string, mode sandboxv1beta1.SandboxOperatingMode, ready, suspended bool) *sandboxv1beta1.Sandbox {
	boolTo := func(b bool) metav1.ConditionStatus {
		if b {
			return metav1.ConditionTrue
		}
		return metav1.ConditionFalse
	}
	return &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       sandboxv1beta1.SandboxSpec{OperatingMode: mode},
		Status: sandboxv1beta1.SandboxStatus{
			ServiceFQDN: name + "." + ns + ".svc.cluster.local",
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: boolTo(ready)},
				{Type: "Suspended", Status: boolTo(suspended), Reason: "test"},
			},
		},
	}
}

func TestValidateUserID(t *testing.T) {
	for _, ok := range []string{"alice", "a", "user-42", "a1b2"} {
		if err := ValidateUserID(ok); err != nil {
			t.Errorf("expected %q valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Alice", "-alice", "alice-", "a_b", "a.b",
		"averyveryveryveryverylongusernameover40chars!"} {
		if err := ValidateUserID(bad); err == nil {
			t.Errorf("expected %q invalid", bad)
		}
	}
}

func TestStateDerivation(t *testing.T) {
	cases := []struct {
		name      string
		mode      sandboxv1beta1.SandboxOperatingMode
		ready     bool
		suspended bool
		want      State
	}{
		{"ready", sandboxv1beta1.SandboxOperatingModeRunning, true, false, StateReady},
		{"waking", sandboxv1beta1.SandboxOperatingModeRunning, false, true, StateWaking},
		{"provision-ish", sandboxv1beta1.SandboxOperatingModeRunning, false, false, StateWaking},
		{"suspending", sandboxv1beta1.SandboxOperatingModeSuspended, false, false, StateSuspending},
		{"suspended", sandboxv1beta1.SandboxOperatingModeSuspended, false, true, StateSuspended},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := sandboxFor("sb", tc.mode, tc.ready, tc.suspended)
			if got := deriveState(sb); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	_, resolver, _, _ := newFixture(t,
		claimFor("alice", "hermes-pool-abc"),
		sandboxFor("hermes-pool-abc", sandboxv1beta1.SandboxOperatingModeRunning, true, false),
	)
	ua, err := resolver.Resolve(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if ua.State != StateReady {
		t.Errorf("state = %s, want Ready", ua.State)
	}
	if ua.SandboxName != "hermes-pool-abc" {
		t.Errorf("sandbox = %s", ua.SandboxName)
	}
	if ua.ServiceFQDN == "" {
		t.Error("expected FQDN")
	}
	if ua.TokenSHA256 != "deadbeef" {
		t.Errorf("token hash = %s", ua.TokenSHA256)
	}
}

func TestResolveNotFound(t *testing.T) {
	_, resolver, _, _ := newFixture(t)
	if _, err := resolver.Resolve(context.Background(), "ghost"); err != ErrUserNotFound {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestResolveProvisioning(t *testing.T) {
	_, resolver, _, _ := newFixture(t, claimFor("bob", ""))
	ua, err := resolver.Resolve(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if ua.State != StateProvisioning {
		t.Errorf("state = %s, want Provisioning", ua.State)
	}
}

func TestEnsureUserIdempotent(t *testing.T) {
	_, _, prov, _ := newFixture(t)
	ctx := context.Background()

	_, created, err := prov.EnsureUser(ctx, "carol", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected created=true on first call")
	}

	_, created, err = prov.EnsureUser(ctx, "carol", "hash2")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("expected created=false on replay")
	}

	// Replay must NOT overwrite the original token hash.
	claim, err := prov.Clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).
		Get(ctx, ClaimName("carol"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := claim.Annotations[AnnotationTokenSHA256]; got != "hash1" {
		t.Errorf("token hash overwritten on replay: %s", got)
	}
	// Warm-pool safety invariants (README decisions 6+7).
	if len(claim.Spec.Env) != 0 || len(claim.Spec.VolumeClaimTemplates) != 0 || claim.Spec.Lifecycle != nil {
		t.Error("claim must have no env, no VCTs, no lifecycle")
	}
	if claim.Spec.AdditionalPodMetadata.Labels[PodLabelDomainUser] != "carol" {
		t.Error("expected user pod label under sandbox.users.io domain")
	}
}

func TestEnsureUserRejectsBadID(t *testing.T) {
	_, _, prov, _ := newFixture(t)
	if _, _, err := prov.EnsureUser(context.Background(), "Bad_ID", "h"); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestSuspendAndResume(t *testing.T) {
	_, resolver, _, lc := newFixture(t,
		claimFor("dave", "hermes-pool-xyz"),
		sandboxFor("hermes-pool-xyz", sandboxv1beta1.SandboxOperatingModeRunning, true, false),
	)
	ctx := context.Background()

	ua, err := lc.Suspend(ctx, "dave")
	if err != nil {
		t.Fatal(err)
	}
	if ua.State != StateSuspending {
		t.Errorf("state after suspend = %s, want Suspending", ua.State)
	}

	// Fake has no controller: simulate the controller finishing suspension.
	sb, _ := lc.Clients.Core.AgentsV1beta1().Sandboxes(ns).Get(ctx, "hermes-pool-xyz", metav1.GetOptions{})
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatalf("operatingMode not patched: %s", sb.Spec.OperatingMode)
	}

	// Suspend is idempotent.
	if _, err := lc.Suspend(ctx, "dave"); err != nil {
		t.Fatal(err)
	}

	// Resume flips the mode back and waits Ready; simulate controller by
	// marking Ready in the background.
	go func() {
		time.Sleep(150 * time.Millisecond)
		cur, _ := lc.Clients.Core.AgentsV1beta1().Sandboxes(ns).Get(context.Background(), "hermes-pool-xyz", metav1.GetOptions{})
		upd := sandboxFor("hermes-pool-xyz", cur.Spec.OperatingMode, true, false)
		upd.ResourceVersion = cur.ResourceVersion
		_, _ = lc.Clients.Core.AgentsV1beta1().Sandboxes(ns).Update(context.Background(), upd, metav1.UpdateOptions{})
	}()
	ua, err = lc.Resume(ctx, "dave", 5*time.Second, "api")
	if err != nil {
		t.Fatal(err)
	}
	if ua.State != StateReady {
		t.Errorf("state after resume = %s, want Ready", ua.State)
	}
	_ = resolver
}

func TestList(t *testing.T) {
	unmanaged := &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "stray", Namespace: ns},
	}
	_, resolver, _, _ := newFixture(t,
		claimFor("u1", ""),
		claimFor("u2", "hermes-pool-2"),
		sandboxFor("hermes-pool-2", sandboxv1beta1.SandboxOperatingModeRunning, true, false),
		unmanaged,
	)
	uas, err := resolver.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(uas) != 2 {
		t.Fatalf("listed %d users, want 2 (unmanaged claim excluded)", len(uas))
	}
}

func TestDeleteUser(t *testing.T) {
	_, _, prov, _ := newFixture(t, claimFor("erin", ""))
	ctx := context.Background()
	if err := prov.DeleteUser(ctx, "erin"); err != nil {
		t.Fatal(err)
	}
	if err := prov.DeleteUser(ctx, "erin"); err != ErrUserNotFound {
		t.Fatalf("second delete err = %v, want ErrUserNotFound", err)
	}
}
