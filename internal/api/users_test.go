package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sbfake "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	extfake "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"

	"github.com/aditya-shantanu/ai-agent-service/internal/api"
	"github.com/aditya-shantanu/ai-agent-service/internal/auth"
	"github.com/aditya-shantanu/ai-agent-service/internal/config"
	"github.com/aditya-shantanu/ai-agent-service/internal/sandbox"
	"github.com/aditya-shantanu/ai-agent-service/internal/server"
)

const (
	ns         = "hermes-users"
	adminToken = "test-admin-token-16chars"
)

func newServer(t *testing.T, seedReadyUser string) (http.Handler, *sandbox.Clients) {
	t.Helper()
	core := sbfake.NewSimpleClientset()
	ext := extfake.NewSimpleClientset()
	clients := &sandbox.Clients{Core: core, Ext: ext, K8s: k8sfake.NewSimpleClientset()}

	if seedReadyUser != "" {
		sb := &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "hermes-pool-seed", Namespace: ns},
			Spec:       sandboxv1beta1.SandboxSpec{OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning},
			Status: sandboxv1beta1.SandboxStatus{
				ServiceFQDN: "hermes-pool-seed." + ns + ".svc.cluster.local",
				Conditions:  []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
			},
		}
		claim := &extv1beta1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sandbox.ClaimName(seedReadyUser),
				Namespace: ns,
				Labels: map[string]string{
					sandbox.LabelManagedBy: sandbox.ManagedByValue,
					sandbox.LabelUser:      seedReadyUser,
				},
				Annotations: map[string]string{sandbox.AnnotationTokenSHA256: auth.HashToken("seed-token")},
			},
		}
		claim.Status.SandboxStatus.Name = "hermes-pool-seed"
		if _, err := core.AgentsV1beta1().Sandboxes(ns).Create(t.Context(), sb, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := ext.ExtensionsV1beta1().SandboxClaims(ns).Create(t.Context(), claim, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	resolver := &sandbox.Resolver{Clients: clients, Namespace: ns}
	prov := &sandbox.Provisioner{Clients: clients, Namespace: ns, WarmPoolName: "hermes-pool", Resolver: resolver}
	lc := &sandbox.Lifecycle{Clients: clients, Namespace: ns, Resolver: resolver}
	h := &api.Handlers{
		Provisioner: prov, Lifecycle: lc, Resolver: resolver,
		// Tiny: the fake has no controllers, so Ready-waits always time out.
		ProvisionTimeout: 100 * time.Millisecond,
		WakeTimeout:      100 * time.Millisecond,
	}
	cfg := &config.Config{AdminToken: adminToken}
	return server.New(cfg, h, nil), clients
}

func doReq(t *testing.T, h http.Handler, method, path, body string, admin bool) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if admin {
		r.Header.Set("Authorization", "Bearer "+adminToken)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAdminAuthRequired(t *testing.T) {
	h, _ := newServer(t, "")
	if w := doReq(t, h, "GET", "/api/v1/users", "", false); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated list: %d", w.Code)
	}
}

func TestCreateUserAndIdempotentReplay(t *testing.T) {
	h, _ := newServer(t, "")

	w := doReq(t, h, "POST", "/api/v1/users", `{"userId":"alice"}`, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["token"] == nil || resp["token"] == "" {
		t.Error("create must return a one-time token")
	}
	// No controller in fakes -> stays Provisioning; creation still succeeds.
	if resp["state"] != "Provisioning" {
		t.Errorf("state = %v", resp["state"])
	}

	w = doReq(t, h, "POST", "/api/v1/users", `{"userId":"alice"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("replay: %d", w.Code)
	}
	resp = map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if tok, ok := resp["token"]; ok && tok != "" {
		t.Error("replay must NOT return a token")
	}
}

func TestCreateUserValidation(t *testing.T) {
	h, _ := newServer(t, "")
	if w := doReq(t, h, "POST", "/api/v1/users", `{"userId":"Bad_ID"}`, true); w.Code != http.StatusBadRequest {
		t.Errorf("invalid id: %d", w.Code)
	}
	if w := doReq(t, h, "POST", "/api/v1/users", `{}`, true); w.Code != http.StatusBadRequest {
		t.Errorf("missing id: %d", w.Code)
	}
}

func TestGetUser(t *testing.T) {
	h, _ := newServer(t, "seeduser")
	w := doReq(t, h, "GET", "/api/v1/users/seeduser", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["state"] != "Ready" || resp["sandboxName"] != "hermes-pool-seed" {
		t.Errorf("resp = %v", resp)
	}
	if w := doReq(t, h, "GET", "/api/v1/users/ghost", "", true); w.Code != http.StatusNotFound {
		t.Errorf("ghost: %d", w.Code)
	}
}

func TestSuspendResumeAndDelete(t *testing.T) {
	h, clients := newServer(t, "seeduser")

	w := doReq(t, h, "POST", "/api/v1/users/seeduser/suspend", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("suspend: %d %s", w.Code, w.Body)
	}
	sb, _ := clients.Core.AgentsV1beta1().Sandboxes(ns).Get(t.Context(), "hermes-pool-seed", metav1.GetOptions{})
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Errorf("operatingMode after suspend = %s", sb.Spec.OperatingMode)
	}

	// Simulate the controller mid-termination: Ready=False. With no fake
	// controller to bring it back, resume must time out -> 202 Accepted.
	sb.Status.Conditions = []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse},
		{Type: "Suspended", Status: metav1.ConditionTrue, Reason: "PodTerminated"},
	}
	if _, err := clients.Core.AgentsV1beta1().Sandboxes(ns).Update(t.Context(), sb, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	w = doReq(t, h, "POST", "/api/v1/users/seeduser/resume", "", true)
	if w.Code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	sb, _ = clients.Core.AgentsV1beta1().Sandboxes(ns).Get(t.Context(), "hermes-pool-seed", metav1.GetOptions{})
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Errorf("operatingMode after resume = %s", sb.Spec.OperatingMode)
	}

	w = doReq(t, h, "DELETE", "/api/v1/users/seeduser", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := doReq(t, h, "DELETE", "/api/v1/users/seeduser", "", true); w.Code != http.StatusNotFound {
		t.Errorf("double delete: %d", w.Code)
	}
}

func TestSetSuspendExempt(t *testing.T) {
	h, clients := newServer(t, "seeduser")

	w := doReq(t, h, "PUT", "/api/v1/users/seeduser/suspend-exempt", `{"exempt":true}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("set exempt: %d %s", w.Code, w.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["suspendExempt"] != true {
		t.Errorf("response suspendExempt = %v", resp["suspendExempt"])
	}
	claim, _ := clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Get(t.Context(), sandbox.ClaimName("seeduser"), metav1.GetOptions{})
	if claim.Annotations[sandbox.AnnotationSuspendExempt] != "true" {
		t.Errorf("claim annotation = %q", claim.Annotations[sandbox.AnnotationSuspendExempt])
	}

	w = doReq(t, h, "PUT", "/api/v1/users/seeduser/suspend-exempt", `{"exempt":false}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("clear exempt: %d %s", w.Code, w.Body)
	}
	resp = map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["suspendExempt"] != false {
		t.Errorf("response suspendExempt after clear = %v", resp["suspendExempt"])
	}

	if w := doReq(t, h, "PUT", "/api/v1/users/seeduser/suspend-exempt", `{}`, true); w.Code != http.StatusBadRequest {
		t.Errorf("missing exempt field: %d", w.Code)
	}
	if w := doReq(t, h, "PUT", "/api/v1/users/ghost/suspend-exempt", `{"exempt":true}`, true); w.Code != http.StatusNotFound {
		t.Errorf("ghost: %d", w.Code)
	}
	if w := doReq(t, h, "PUT", "/api/v1/users/seeduser/suspend-exempt", `{"exempt":true}`, false); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", w.Code)
	}
}

func TestRotateToken(t *testing.T) {
	h, clients := newServer(t, "seeduser")
	w := doReq(t, h, "POST", "/api/v1/users/seeduser/token", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate: %d", w.Code)
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["token"] == "" {
		t.Fatal("rotate must return a token")
	}
	claim, _ := clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Get(t.Context(), sandbox.ClaimName("seeduser"), metav1.GetOptions{})
	if claim.Annotations[sandbox.AnnotationTokenSHA256] != auth.HashToken(resp["token"]) {
		t.Error("stored hash does not match rotated token")
	}
}

func TestHealthz(t *testing.T) {
	h, _ := newServer(t, "")
	if w := doReq(t, h, "GET", "/healthz", "", false); w.Code != http.StatusOK {
		t.Errorf("healthz: %d", w.Code)
	}
}
