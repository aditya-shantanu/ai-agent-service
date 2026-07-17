// Package sandbox is the platform's only interface to the agent-sandbox
// CRDs: provisioning claims, resolving user -> sandbox endpoints, and
// flipping operatingMode for suspend/resume.
package sandbox

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	sbclient "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned"
	extclient "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned"
)

// Labels and annotations owned by this platform. The claim IS the user
// record (README decision 10) — these keys are its schema.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "hermes-gateway"
	LabelUser      = "hermes.ai-agent-service.dev/user"

	// AnnotationTokenSHA256 stores the hex SHA-256 of the user's bearer token.
	AnnotationTokenSHA256 = "hermes.ai-agent-service.dev/token-sha256"
	// AnnotationSuspendExempt marks users excluded from idle suspension.
	AnnotationSuspendExempt = "hermes.ai-agent-service.dev/suspend-exempt"

	// PodLabelDomainUser is the pod label (via additionalPodMetadata) carrying
	// the user ID. Must stay under sandbox.users.io — the only label domain the
	// claim controller allows without custom flags.
	PodLabelDomainUser = "sandbox.users.io/hermes-user"
)

// Clients bundles the three typed clients the gateway needs.
type Clients struct {
	Core sbclient.Interface  // sandboxes (agents.x-k8s.io)
	Ext  extclient.Interface // claims/templates/warmpools (extensions.agents.x-k8s.io)
	K8s  kubernetes.Interface
	Rest *rest.Config
}

// NewClients builds clients from in-cluster config, falling back to the
// given kubeconfig path (dev mode).
func NewClients(kubeconfig string) (*Clients, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("no in-cluster config and kubeconfig failed: %w", err)
		}
	}
	core, err := sbclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	ext, err := extclient.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	k8s, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Clients{Core: core, Ext: ext, K8s: k8s, Rest: cfg}, nil
}

// ClaimName returns the deterministic claim name for a user.
func ClaimName(userID string) string { return "hermes-" + userID }
