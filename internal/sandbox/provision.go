package sandbox

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

// userIDRe: DNS-1123 label, max 40 chars (leaves headroom for prefixes).
var userIDRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ValidateUserID rejects IDs that can't be embedded in resource names/labels.
func ValidateUserID(id string) error {
	if !userIDRe.MatchString(id) {
		return fmt.Errorf("invalid user id %q: must match %s", id, userIDRe)
	}
	return nil
}

// Provisioner creates and deletes per-user claims.
type Provisioner struct {
	Clients      *Clients
	Namespace    string
	WarmPoolName string
	Resolver     *Resolver
}

// EnsureUser creates the user's claim if absent. Returns (agent, created).
//
// The claim deliberately has NO env, NO volumeClaimTemplates and NO lifecycle:
// the first two would be rejected by the template's Disallowed policies (and
// would bypass the warm pool otherwise); lifecycle expiry would delete the
// Sandbox and garbage-collect the user's PVC (README decisions 6+7).
func (p *Provisioner) EnsureUser(ctx context.Context, userID, tokenSHA256 string) (*UserAgent, bool, error) {
	if err := ValidateUserID(userID); err != nil {
		return nil, false, err
	}
	claim := &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClaimName(userID),
			Namespace: p.Namespace,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelUser:      userID,
			},
			Annotations: map[string]string{
				AnnotationTokenSHA256: tokenSHA256,
			},
		},
		Spec: extv1beta1.SandboxClaimSpec{
			WarmPoolRef: extv1beta1.SandboxWarmPoolRef{Name: p.WarmPoolName},
			AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{
					PodLabelDomainUser: userID,
				},
			},
		},
	}

	created := true
	_, err := p.Clients.Ext.ExtensionsV1beta1().SandboxClaims(p.Namespace).
		Create(ctx, claim, metav1.CreateOptions{})
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return nil, false, fmt.Errorf("create claim: %w", err)
		}
		created = false // idempotent replay
	}

	ua, err := p.Resolver.Resolve(ctx, userID)
	if err != nil {
		return nil, created, err
	}
	return ua, created, nil
}

// WaitAdopted polls until the claim has a sandbox and it reports Ready,
// returning the final resolved state. Warm path: seconds. Cold path: slower,
// same code.
func (p *Provisioner) WaitAdopted(ctx context.Context, userID string, timeout time.Duration) (*UserAgent, error) {
	var last *UserAgent
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			ua, err := p.Resolver.Resolve(ctx, userID)
			if err != nil {
				if err == ErrUserNotFound {
					return false, err // deleted underneath us — stop
				}
				return false, nil // transient API error — keep polling
			}
			last = ua
			return ua.State == StateReady, nil
		})
	if err != nil {
		state := StateUnknown
		if last != nil {
			state = last.State
		}
		return last, fmt.Errorf("waiting for user %s to become Ready (last state %s): %w", userID, state, err)
	}
	return last, nil
}

// DeleteUser removes the claim. This cascades: claim -> sandbox -> PVC
// (deliberate, decision 7) and any claim-owned Secrets (telegram token).
func (p *Provisioner) DeleteUser(ctx context.Context, userID string) error {
	err := p.Clients.Ext.ExtensionsV1beta1().SandboxClaims(p.Namespace).
		Delete(ctx, ClaimName(userID), metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return ErrUserNotFound
		}
		return fmt.Errorf("delete claim: %w", err)
	}
	return nil
}
