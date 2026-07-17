// Package telegram wires a user's Telegram bot token into their running
// Hermes sandbox at runtime — the only injection path compatible with warm
// pools (claim env kills warm adoption; template env is shared).
//
// Flow: durable copy in a claim-owned Secret -> ensure the sandbox runs ->
// exec into the pod to rewrite $HERMES_HOME/.env -> restart the supervised
// gateway (s6) -> mark the user suspend-exempt (long-polling bot dies while
// suspended).
package telegram

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/adityashantanu/ai-agent-service/internal/sandbox"
)

// tokenRe matches Telegram bot tokens: numeric bot ID, colon, secret.
var tokenRe = regexp.MustCompile(`^[0-9]{5,12}:[A-Za-z0-9_-]{30,64}$`)

func ValidateToken(tok string) error {
	if !tokenRe.MatchString(tok) {
		return fmt.Errorf("token does not look like a Telegram bot token (expected <botid>:<secret>)")
	}
	return nil
}

// allowedUsersRe: comma-separated telegram user IDs or @usernames.
var allowedUsersRe = regexp.MustCompile(`^[@A-Za-z0-9_,]*$`)

// ExecRunner abstracts pod exec for tests.
type ExecRunner interface {
	Exec(ctx context.Context, namespace, pod, container string, command []string) (stdout, stderr string, err error)
}

// SecretName returns the per-user Telegram secret name.
func SecretName(userID string) string { return "hermes-user-" + userID + "-telegram" }

type Injector struct {
	Clients   *sandbox.Clients
	Namespace string
	Resolver  *sandbox.Resolver
	Lifecycle *sandbox.Lifecycle
	Exec      ExecRunner
	// WakeTimeout bounds the implicit resume when the user is suspended.
	WakeTimeout time.Duration
}

// SetToken installs (or replaces) the user's bot token.
func (i *Injector) SetToken(ctx context.Context, userID, token, allowedUsers string) error {
	if err := ValidateToken(token); err != nil {
		return err
	}
	if !allowedUsersRe.MatchString(allowedUsers) {
		return fmt.Errorf("allowedUsers must be comma-separated Telegram IDs or usernames")
	}

	ua, err := i.Resolver.Resolve(ctx, userID)
	if err != nil {
		return err
	}

	// 1. Durable copy: claim-owned Secret (cascade-deleted with the user).
	if err := i.upsertSecret(ctx, userID, token, allowedUsers); err != nil {
		return err
	}

	// 2. Agent must be running to receive the injection.
	if ua.State != sandbox.StateReady {
		if ua, err = i.Lifecycle.Resume(ctx, userID, i.WakeTimeout); err != nil {
			return fmt.Errorf("resume before injection: %w", err)
		}
	}

	// 3. Rewrite the TELEGRAM_* lines in $HERMES_HOME/.env idempotently and
	// restart the supervised gateway so it reconnects with the new token.
	script := fmt.Sprintf(
		`grep -v '^TELEGRAM_BOT_TOKEN=' /opt/data/.env 2>/dev/null | grep -v '^TELEGRAM_ALLOWED_USERS=' > /opt/data/.env.tmp || true
printf 'TELEGRAM_BOT_TOKEN=%%s\nTELEGRAM_ALLOWED_USERS=%%s\n' %q %q >> /opt/data/.env.tmp
mv /opt/data/.env.tmp /opt/data/.env
chmod 600 /opt/data/.env`, token, allowedUsers)
	pod, err := i.podName(ctx, ua)
	if err != nil {
		return err
	}
	if _, stderr, err := i.Exec.Exec(ctx, i.Namespace, pod, "hermes", []string{"sh", "-c", script}); err != nil {
		return fmt.Errorf("write .env: %w (stderr: %s)", err, stderr)
	}
	if _, stderr, err := i.Exec.Exec(ctx, i.Namespace, pod, "hermes",
		[]string{"/command/s6-svc", "-r", "/run/service/gateway-default"}); err != nil {
		return fmt.Errorf("restart gateway service: %w (stderr: %s)", err, stderr)
	}

	// 4. Long-polling bot must stay up: exempt from idle suspension.
	return i.Lifecycle.SetSuspendExempt(ctx, userID, true)
}

// RemoveToken deletes the token from Secret + .env and re-enables idle suspend.
func (i *Injector) RemoveToken(ctx context.Context, userID string) error {
	ua, err := i.Resolver.Resolve(ctx, userID)
	if err != nil {
		return err
	}
	err = i.Clients.K8s.CoreV1().Secrets(i.Namespace).Delete(ctx, SecretName(userID), metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete secret: %w", err)
	}
	if ua.State == sandbox.StateReady {
		pod, perr := i.podName(ctx, ua)
		if perr == nil {
			script := `grep -v '^TELEGRAM_BOT_TOKEN=' /opt/data/.env 2>/dev/null | grep -v '^TELEGRAM_ALLOWED_USERS=' > /opt/data/.env.tmp || true
mv /opt/data/.env.tmp /opt/data/.env`
			if _, stderr, xerr := i.Exec.Exec(ctx, i.Namespace, pod, "hermes", []string{"sh", "-c", script}); xerr != nil {
				return fmt.Errorf("scrub .env: %w (stderr: %s)", xerr, stderr)
			}
			if _, _, xerr := i.Exec.Exec(ctx, i.Namespace, pod, "hermes",
				[]string{"/command/s6-svc", "-r", "/run/service/gateway-default"}); xerr != nil {
				return fmt.Errorf("restart gateway service: %w", xerr)
			}
		}
	}
	return i.Lifecycle.SetSuspendExempt(ctx, userID, false)
}

// HasToken reports whether the durable Secret exists (for status displays).
func (i *Injector) HasToken(ctx context.Context, userID string) (bool, error) {
	_, err := i.Clients.K8s.CoreV1().Secrets(i.Namespace).Get(ctx, SecretName(userID), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (i *Injector) upsertSecret(ctx context.Context, userID, token, allowedUsers string) error {
	claim, err := i.Clients.Ext.ExtensionsV1beta1().SandboxClaims(i.Namespace).
		Get(ctx, sandbox.ClaimName(userID), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get claim for ownerRef: %w", err)
	}
	controller := true
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(userID),
			Namespace: i.Namespace,
			Labels: map[string]string{
				sandbox.LabelManagedBy: sandbox.ManagedByValue,
				sandbox.LabelUser:      userID,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1beta1",
				Kind:       "SandboxClaim",
				Name:       claim.Name,
				UID:        claim.UID,
				Controller: &controller,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"TELEGRAM_BOT_TOKEN":     token,
			"TELEGRAM_ALLOWED_USERS": allowedUsers,
		},
	}
	_, err = i.Clients.K8s.CoreV1().Secrets(i.Namespace).Create(ctx, sec, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		_, err = i.Clients.K8s.CoreV1().Secrets(i.Namespace).Update(ctx, sec, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("upsert secret: %w", err)
	}
	return nil
}

// podName resolves the actual pod: the sandbox tracks warm-adopted pods via
// annotation; recreated (post-resume) pods are named after the sandbox.
func (i *Injector) podName(ctx context.Context, ua *sandbox.UserAgent) (string, error) {
	sb, err := i.Clients.Core.AgentsV1beta1().Sandboxes(i.Namespace).Get(ctx, ua.SandboxName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get sandbox: %w", err)
	}
	if name := sb.Annotations["agents.x-k8s.io/pod-name"]; name != "" {
		return name, nil
	}
	return ua.SandboxName, nil
}

// SPDYExecRunner is the real pod-exec implementation.
type SPDYExecRunner struct {
	Clients *sandbox.Clients
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
