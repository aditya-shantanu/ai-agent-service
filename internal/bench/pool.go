package bench

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sbclient "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned"
	extclient "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned"
)

// PoolControl manipulates the SandboxWarmPool directly: drain for the
// new-agent-cold scenario, restock waits for new-agent-warm. It is the only
// part of the bench that talks to the Kubernetes API; everything else goes
// through the gateway like a real client.
type PoolControl struct {
	Core      sbclient.Interface
	Ext       extclient.Interface
	Namespace string
	PoolName  string
}

// NewPoolControl builds clients from the default kubeconfig chain with an
// optional context override (kind-hermes-svc vs the GKE context).
func NewPoolControl(kubeContext, namespace, poolName string) (*PoolControl, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	core, err := sbclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	ext, err := extclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &PoolControl{Core: core, Ext: ext, Namespace: namespace, PoolName: poolName}, nil
}

func (p *PoolControl) desiredReplicas(ctx context.Context) (int32, error) {
	pool, err := p.Ext.ExtensionsV1beta1().SandboxWarmPools(p.Namespace).Get(ctx, p.PoolName, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	if pool.Spec.Replicas == nil {
		return 1, nil // API default
	}
	return *pool.Spec.Replicas, nil
}

func (p *PoolControl) readySpares(ctx context.Context) (int32, error) {
	pool, err := p.Ext.ExtensionsV1beta1().SandboxWarmPools(p.Namespace).Get(ctx, p.PoolName, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	return pool.Status.ReadyReplicas, nil
}

func (p *PoolControl) setReplicas(ctx context.Context, n int32) error {
	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, n)
	_, err := p.Ext.ExtensionsV1beta1().SandboxWarmPools(p.Namespace).
		Patch(ctx, p.PoolName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch warmpool %s replicas=%d: %w", p.PoolName, n, err)
	}
	return nil
}

// Drain scales the pool to 0 and deletes the existing spares (adopted
// sandboxes lose the warm-pool label, so this never touches live users).
// The returned restore puts replicas back and waits for restock; callers
// MUST invoke it on a cancellation-immune context.
func (p *PoolControl) Drain(ctx context.Context) (restore func(context.Context) error, err error) {
	saved, err := p.desiredReplicas(ctx)
	if err != nil {
		return nil, err
	}
	if err := p.setReplicas(ctx, 0); err != nil {
		return nil, err
	}
	restore = func(rctx context.Context) error {
		if err := p.setReplicas(rctx, saved); err != nil {
			return err
		}
		return p.WaitRestocked(rctx, 5*time.Minute)
	}

	spares, err := p.Core.AgentsV1beta1().Sandboxes(p.Namespace).
		List(ctx, metav1.ListOptions{LabelSelector: sandboxv1beta1.SandboxWarmPoolLabel})
	if err != nil {
		return restore, fmt.Errorf("list warm spares: %w", err)
	}
	for _, sb := range spares.Items {
		err := p.Core.AgentsV1beta1().Sandboxes(p.Namespace).
			Delete(ctx, sb.Name, metav1.DeleteOptions{})
		// The pool controller reaps spares itself after replicas=0 — losing
		// the race to it is success, not failure.
		if err != nil && !apierrors.IsNotFound(err) {
			return restore, fmt.Errorf("delete spare %s: %w", sb.Name, err)
		}
	}

	// Wait until nothing labeled remains — a terminating spare could still
	// be adopted mid-teardown and turn a "cold" sample warm.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		left, err := p.Core.AgentsV1beta1().Sandboxes(p.Namespace).
			List(ctx, metav1.ListOptions{LabelSelector: sandboxv1beta1.SandboxWarmPoolLabel})
		if err == nil && len(left.Items) == 0 {
			return restore, nil
		}
		if time.Now().After(deadline) {
			return restore, fmt.Errorf("warm spares still present after drain")
		}
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			return restore, err
		}
	}
}

// WaitRestocked polls until the pool reports its desired number of Ready
// spares again.
func (p *PoolControl) WaitRestocked(ctx context.Context, timeout time.Duration) error {
	want, err := p.desiredReplicas(ctx)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		ready, err := p.readySpares(ctx)
		if err == nil && ready >= want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pool %s not restocked to %d within %s (ready %d)", p.PoolName, want, timeout, ready)
		}
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}
