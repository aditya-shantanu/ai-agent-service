package sandbox

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecRunner abstracts pod exec so cron-peek and telegram injection are
// testable without a cluster.
type ExecRunner interface {
	Exec(ctx context.Context, namespace, pod, container string, command []string) (stdout, stderr string, err error)
}

// SPDYExecRunner is the real pod-exec implementation.
type SPDYExecRunner struct {
	Clients *Clients
}

func (s *SPDYExecRunner) Exec(ctx context.Context, namespace, pod, container string, command []string) (string, string, error) {
	req := s.Clients.K8s.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(s.Clients.Rest, "POST", req.URL())
	if err != nil {
		return "", "", err
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	return stdout.String(), stderr.String(), err
}

// PodName resolves the actual pod for a user's sandbox: warm-adopted pods are
// tracked via the sandbox's pod-name annotation; recreated (post-resume) pods
// are named after the sandbox itself.
func (r *Resolver) PodName(ctx context.Context, ua *UserAgent) (string, error) {
	sb, err := r.Clients.Core.AgentsV1beta1().Sandboxes(r.Namespace).Get(ctx, ua.SandboxName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get sandbox: %w", err)
	}
	if name := sb.Annotations["agents.x-k8s.io/pod-name"]; name != "" {
		return name, nil
	}
	return ua.SandboxName, nil
}
