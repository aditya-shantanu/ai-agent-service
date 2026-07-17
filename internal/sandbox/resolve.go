package sandbox

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

// State is the derived lifecycle state of a user's agent. It is computed
// from claim + sandbox conditions on every read — never stored.
type State string

const (
	StateProvisioning State = "Provisioning" // claim exists, sandbox not adopted/created yet
	StateReady        State = "Ready"
	StateSuspending   State = "Suspending" // operatingMode=Suspended, pod not fully gone
	StateSuspended    State = "Suspended"
	StateWaking       State = "Waking" // operatingMode=Running, not Ready yet
	StateUnknown      State = "Unknown"
)

// UserAgent is the resolved view of one user's agent.
type UserAgent struct {
	UserID       string
	ClaimName    string
	SandboxName  string
	ServiceFQDN  string
	State        State
	SuspendState string // reason detail for debugging
	Exempt       bool   // suspend-exempt (telegram users)
	TokenSHA256  string // hex hash of the user's bearer token
}

// ErrUserNotFound is returned when no claim exists for the user.
var ErrUserNotFound = fmt.Errorf("user not found")

// Resolver reads claims and sandboxes and derives user state.
type Resolver struct {
	Clients   *Clients
	Namespace string
}

// Resolve loads the claim and (if adopted) the sandbox for userID.
func (r *Resolver) Resolve(ctx context.Context, userID string) (*UserAgent, error) {
	claim, err := r.Clients.Ext.ExtensionsV1beta1().SandboxClaims(r.Namespace).
		Get(ctx, ClaimName(userID), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("get claim: %w", err)
	}
	return r.resolveFromClaim(ctx, userID, claim)
}

func (r *Resolver) resolveFromClaim(ctx context.Context, userID string, claim *extv1beta1.SandboxClaim) (*UserAgent, error) {
	ua := &UserAgent{
		UserID:      userID,
		ClaimName:   claim.Name,
		TokenSHA256: claim.Annotations[AnnotationTokenSHA256],
		Exempt:      claim.Annotations[AnnotationSuspendExempt] == "true",
	}

	ua.SandboxName = claim.Status.SandboxStatus.Name
	if ua.SandboxName == "" {
		ua.State = StateProvisioning
		return ua, nil
	}

	sb, err := r.Clients.Core.AgentsV1beta1().Sandboxes(r.Namespace).
		Get(ctx, ua.SandboxName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Claim says adopted but sandbox is gone — broken state.
			ua.State = StateUnknown
			ua.SuspendState = "sandbox object missing"
			return ua, nil
		}
		return nil, fmt.Errorf("get sandbox: %w", err)
	}

	ua.ServiceFQDN = sb.Status.ServiceFQDN
	ua.State = deriveState(sb)
	if c := findCondition(sb, "Suspended"); c != nil {
		ua.SuspendState = c.Reason
	}
	return ua, nil
}

// deriveState maps (operatingMode x conditions) to a State.
// See README decision list and the agent-sandbox KEP for the condition matrix.
func deriveState(sb *sandboxv1beta1.Sandbox) State {
	suspendedDesired := sb.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended
	suspended := isConditionTrue(sb, "Suspended")
	ready := isConditionTrue(sb, "Ready")

	switch {
	case suspendedDesired && suspended:
		return StateSuspended
	case suspendedDesired:
		return StateSuspending
	case ready:
		return StateReady
	default:
		// Running desired but not Ready: brand new or waking from suspension.
		return StateWaking
	}
}

func findCondition(sb *sandboxv1beta1.Sandbox, condType string) *metav1.Condition {
	for i := range sb.Status.Conditions {
		if sb.Status.Conditions[i].Type == condType {
			return &sb.Status.Conditions[i]
		}
	}
	return nil
}

func isConditionTrue(sb *sandboxv1beta1.Sandbox, condType string) bool {
	c := findCondition(sb, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}

// List returns all users (claims) managed by this platform.
func (r *Resolver) List(ctx context.Context) ([]*UserAgent, error) {
	claims, err := r.Clients.Ext.ExtensionsV1beta1().SandboxClaims(r.Namespace).
		List(ctx, metav1.ListOptions{
			LabelSelector: LabelManagedBy + "=" + ManagedByValue,
		})
	if err != nil {
		return nil, fmt.Errorf("list claims: %w", err)
	}
	out := make([]*UserAgent, 0, len(claims.Items))
	for i := range claims.Items {
		claim := &claims.Items[i]
		userID := claim.Labels[LabelUser]
		if userID == "" {
			continue
		}
		ua, err := r.resolveFromClaim(ctx, userID, claim)
		if err != nil {
			return nil, err
		}
		out = append(out, ua)
	}
	return out, nil
}
