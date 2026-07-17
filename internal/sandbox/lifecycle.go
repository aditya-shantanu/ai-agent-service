package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// Lifecycle flips Sandbox.spec.operatingMode and waits for the transitions.
// This is the heart of the cost-saving story: suspend deletes only the pod
// (PVC + Service survive); resume recreates the pod with the same PVCs.
type Lifecycle struct {
	Clients   *Clients
	Namespace string
	Resolver  *Resolver

	// wakeMu serializes wake attempts per user so a thundering herd of
	// requests produces exactly one resume patch + wait.
	wakeMu sync.Map // userID -> *sync.Mutex
}

func (l *Lifecycle) userMutex(userID string) *sync.Mutex {
	mu, _ := l.wakeMu.LoadOrStore(userID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func (l *Lifecycle) setOperatingMode(ctx context.Context, sandboxName string, mode sandboxv1beta1.SandboxOperatingMode) error {
	patch := fmt.Sprintf(`{"spec":{"operatingMode":%q}}`, mode)
	_, err := l.Clients.Core.AgentsV1beta1().Sandboxes(l.Namespace).
		Patch(ctx, sandboxName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch sandbox %s operatingMode=%s: %w", sandboxName, mode, err)
	}
	return nil
}

// Suspend requests suspension for the user's sandbox. Idempotent; returns the
// state after the patch (usually Suspending — full termination is async).
func (l *Lifecycle) Suspend(ctx context.Context, userID string) (*UserAgent, error) {
	ua, err := l.Resolver.Resolve(ctx, userID)
	if err != nil {
		return nil, err
	}
	if ua.SandboxName == "" {
		return nil, fmt.Errorf("user %s has no sandbox yet (state %s)", userID, ua.State)
	}
	if ua.State == StateSuspended || ua.State == StateSuspending {
		return ua, nil
	}
	if err := l.setOperatingMode(ctx, ua.SandboxName, sandboxv1beta1.SandboxOperatingModeSuspended); err != nil {
		return nil, err
	}
	return l.Resolver.Resolve(ctx, userID)
}

// Resume requests the user's sandbox to run and waits (up to timeout) for
// Ready. Safe to call in any state — flipping operatingMode back to Running
// mid-suspend is supported by the controller (never wait for Suspended=True
// before resuming; README decision 13).
func (l *Lifecycle) Resume(ctx context.Context, userID string, timeout time.Duration) (*UserAgent, error) {
	mu := l.userMutex(userID)
	mu.Lock()
	defer mu.Unlock()

	ua, err := l.Resolver.Resolve(ctx, userID)
	if err != nil {
		return nil, err
	}
	if ua.SandboxName == "" {
		return nil, fmt.Errorf("user %s has no sandbox yet (state %s)", userID, ua.State)
	}
	if ua.State == StateReady {
		return ua, nil
	}
	if ua.State == StateSuspended || ua.State == StateSuspending {
		if err := l.setOperatingMode(ctx, ua.SandboxName, sandboxv1beta1.SandboxOperatingModeRunning); err != nil {
			return nil, err
		}
	}
	return l.WaitReady(ctx, userID, timeout)
}

// WaitReady polls until the user's agent is Ready.
func (l *Lifecycle) WaitReady(ctx context.Context, userID string, timeout time.Duration) (*UserAgent, error) {
	var last *UserAgent
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			ua, err := l.Resolver.Resolve(ctx, userID)
			if err != nil {
				if err == ErrUserNotFound {
					return false, err
				}
				return false, nil
			}
			last = ua
			return ua.State == StateReady, nil
		})
	if err != nil {
		state := StateUnknown
		if last != nil {
			state = last.State
		}
		return last, fmt.Errorf("waiting for user %s Ready (last state %s): %w", userID, state, err)
	}
	return last, nil
}

// SetSuspendExempt toggles the idle-suspension exemption annotation on the claim.
func (l *Lifecycle) SetSuspendExempt(ctx context.Context, userID string, exempt bool) error {
	val := "false"
	if exempt {
		val = "true"
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, AnnotationSuspendExempt, val)
	_, err := l.Clients.Ext.ExtensionsV1beta1().SandboxClaims(l.Namespace).
		Patch(ctx, ClaimName(userID), types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch claim suspend-exempt: %w", err)
	}
	return nil
}

// SetTokenHash updates the stored bearer-token hash (token rotation).
func (l *Lifecycle) SetTokenHash(ctx context.Context, userID, sha256hex string) error {
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, AnnotationTokenSHA256, sha256hex)
	_, err := l.Clients.Ext.ExtensionsV1beta1().SandboxClaims(l.Namespace).
		Patch(ctx, ClaimName(userID), types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch claim token hash: %w", err)
	}
	return nil
}
