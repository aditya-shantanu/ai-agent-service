package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sbfake "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	extfake "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"

	"github.com/aditya-shantanu/ai-agent-service/internal/sandbox"
)

const (
	ns       = "hermes-users"
	goodTok  = "1234567890:AAFakeTokenForTesting_0123456789abc"
	userName = "alice"
)

type fakeExec struct {
	calls [][]string
	fail  bool
}

func (f *fakeExec) Exec(_ context.Context, _, pod, container string, command []string) (string, string, error) {
	f.calls = append(f.calls, append([]string{pod, container}, command...))
	if f.fail {
		return "", "boom", context.DeadlineExceeded
	}
	return "", "", nil
}

func fixture(t *testing.T) (*Injector, *fakeExec, *sandbox.Clients) {
	t.Helper()
	core := sbfake.NewSimpleClientset()
	ext := extfake.NewSimpleClientset()
	k8s := k8sfake.NewSimpleClientset()
	clients := &sandbox.Clients{Core: core, Ext: ext, K8s: k8s}
	ctx := context.Background()

	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "hermes-pool-w",
			Namespace:   ns,
			Annotations: map[string]string{"agents.x-k8s.io/pod-name": "hermes-pool-w-pod"},
		},
		Spec: sandboxv1beta1.SandboxSpec{OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning},
		Status: sandboxv1beta1.SandboxStatus{
			ServiceFQDN: "hermes-pool-w.hermes-users.svc.cluster.local",
			Conditions:  []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
		},
	}
	claim := &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandbox.ClaimName(userName),
			Namespace: ns,
			UID:       "claim-uid-1",
			Labels: map[string]string{
				sandbox.LabelManagedBy: sandbox.ManagedByValue,
				sandbox.LabelUser:      userName,
			},
			Annotations: map[string]string{},
		},
	}
	claim.Status.SandboxStatus.Name = sb.Name
	if _, err := core.AgentsV1beta1().Sandboxes(ns).Create(ctx, sb, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ext.ExtensionsV1beta1().SandboxClaims(ns).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	resolver := &sandbox.Resolver{Clients: clients, Namespace: ns}
	lc := &sandbox.Lifecycle{Clients: clients, Namespace: ns, Resolver: resolver}
	exec := &fakeExec{}
	inj := &Injector{
		Clients: clients, Namespace: ns, Resolver: resolver, Lifecycle: lc,
		Exec: exec, WakeTimeout: 100 * time.Millisecond,
	}
	return inj, exec, clients
}

func TestValidateToken(t *testing.T) {
	if err := ValidateToken(goodTok); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
	for _, bad := range []string{"", "notatoken", "abc:def", "12345", goodTok + " ; rm -rf /"} {
		if err := ValidateToken(bad); err == nil {
			t.Errorf("expected %q rejected", bad)
		}
	}
}

func TestSetToken(t *testing.T) {
	inj, exec, clients := fixture(t)
	ctx := context.Background()

	if err := inj.SetToken(ctx, userName, goodTok, "42,@bob"); err != nil {
		t.Fatal(err)
	}

	// Durable Secret with claim ownerRef.
	sec, err := clients.K8s.CoreV1().Secrets(ns).Get(ctx, SecretName(userName), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sec.StringData["TELEGRAM_BOT_TOKEN"] != goodTok {
		t.Error("secret token mismatch")
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "SandboxClaim" {
		t.Errorf("ownerRef = %+v", sec.OwnerReferences)
	}

	// Exec: .env rewrite against the annotated pod, then s6 restart.
	if len(exec.calls) != 2 {
		t.Fatalf("exec calls = %d, want 2", len(exec.calls))
	}
	if exec.calls[0][0] != "hermes-pool-w-pod" {
		t.Errorf("exec targeted pod %s (want annotation value)", exec.calls[0][0])
	}
	if !strings.Contains(strings.Join(exec.calls[0], " "), "TELEGRAM_BOT_TOKEN") {
		t.Error("first exec should write .env")
	}
	if exec.calls[1][2] != "/command/s6-svc" {
		t.Errorf("second exec = %v, want s6-svc restart", exec.calls[1])
	}

	// Suspend-exempt annotation set on claim.
	claim, _ := clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Get(ctx, sandbox.ClaimName(userName), metav1.GetOptions{})
	if claim.Annotations[sandbox.AnnotationSuspendExempt] != "true" {
		t.Error("suspend-exempt not set")
	}

	// Replace (upsert path must not fail on AlreadyExists).
	if err := inj.SetToken(ctx, userName, goodTok, "99"); err != nil {
		t.Fatalf("replace: %v", err)
	}
}

func TestSetTokenRejectsBadInput(t *testing.T) {
	inj, exec, _ := fixture(t)
	ctx := context.Background()
	if err := inj.SetToken(ctx, userName, "garbage", ""); err == nil {
		t.Error("bad token accepted")
	}
	if err := inj.SetToken(ctx, userName, goodTok, "1; rm -rf /"); err == nil {
		t.Error("shell metacharacters in allowedUsers accepted")
	}
	if len(exec.calls) != 0 {
		t.Error("exec ran despite validation failure")
	}
}

func TestRemoveToken(t *testing.T) {
	inj, exec, clients := fixture(t)
	ctx := context.Background()
	if err := inj.SetToken(ctx, userName, goodTok, ""); err != nil {
		t.Fatal(err)
	}
	exec.calls = nil

	if err := inj.RemoveToken(ctx, userName); err != nil {
		t.Fatal(err)
	}
	if _, err := clients.K8s.CoreV1().Secrets(ns).Get(ctx, SecretName(userName), metav1.GetOptions{}); err == nil {
		t.Error("secret still exists after removal")
	}
	if len(exec.calls) != 2 {
		t.Errorf("exec calls = %d, want 2 (scrub + restart)", len(exec.calls))
	}
	claim, _ := clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Get(ctx, sandbox.ClaimName(userName), metav1.GetOptions{})
	if claim.Annotations[sandbox.AnnotationSuspendExempt] != "false" {
		t.Error("suspend-exempt not cleared")
	}
}

func TestHasToken(t *testing.T) {
	inj, _, _ := fixture(t)
	ctx := context.Background()
	has, err := inj.HasToken(ctx, userName)
	if err != nil || has {
		t.Fatalf("has=%v err=%v before set", has, err)
	}
	_ = inj.SetToken(ctx, userName, goodTok, "")
	has, err = inj.HasToken(ctx, userName)
	if err != nil || !has {
		t.Fatalf("has=%v err=%v after set", has, err)
	}
}
