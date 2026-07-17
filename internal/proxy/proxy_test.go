package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sbfake "sigs.k8s.io/agent-sandbox/clients/k8s/clientset/versioned/fake"
	extfake "sigs.k8s.io/agent-sandbox/clients/k8s/extensions/clientset/versioned/fake"
	extv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"

	"github.com/adityashantanu/ai-agent-service/internal/auth"
	"github.com/adityashantanu/ai-agent-service/internal/idle"
	"github.com/adityashantanu/ai-agent-service/internal/sandbox"
)

const (
	ns        = "hermes-users"
	userToken = "user-token-abc"
	apiKey    = "platform-api-key-1234567890abcdef"
)

// echoUpstream records the last request it served.
type echoUpstream struct {
	lastPath string
	lastAuth string
	lastTok  string
}

func (e *echoUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		e.lastPath = r.URL.Path
		e.lastAuth = r.Header.Get("Authorization")
		e.lastTok = r.URL.Query().Get("token")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	}
}

func newProxyFixture(t *testing.T, state string, upstreamPort int) (*Proxy, *sandbox.Clients) {
	t.Helper()
	core := sbfake.NewSimpleClientset()
	ext := extfake.NewSimpleClientset()
	ctx := context.Background()

	ready := state == "Ready"
	mode := sandboxv1beta1.SandboxOperatingModeRunning
	suspended := false
	if state == "Suspended" {
		mode = sandboxv1beta1.SandboxOperatingModeSuspended
		suspended = true
	}
	boolTo := func(b bool) metav1.ConditionStatus {
		if b {
			return metav1.ConditionTrue
		}
		return metav1.ConditionFalse
	}
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes-pool-p", Namespace: ns},
		Spec:       sandboxv1beta1.SandboxSpec{OperatingMode: mode},
		Status: sandboxv1beta1.SandboxStatus{
			ServiceFQDN: "127.0.0.1", // tests dial loopback:upstreamPort
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: boolTo(ready)},
				{Type: "Suspended", Status: boolTo(suspended)},
			},
		},
	}
	claim := &extv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandbox.ClaimName("alice"),
			Namespace: ns,
			Labels: map[string]string{
				sandbox.LabelManagedBy: sandbox.ManagedByValue,
				sandbox.LabelUser:      "alice",
			},
			Annotations: map[string]string{
				sandbox.AnnotationTokenSHA256: auth.HashToken(userToken),
			},
		},
	}
	claim.Status.SandboxStatus.Name = "hermes-pool-p"
	if _, err := core.AgentsV1beta1().Sandboxes(ns).Create(ctx, sb, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ext.ExtensionsV1beta1().SandboxClaims(ns).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	clients := &sandbox.Clients{Core: core, Ext: ext}
	resolver := &sandbox.Resolver{Clients: clients, Namespace: ns}
	lc := &sandbox.Lifecycle{Clients: clients, Namespace: ns, Resolver: resolver}
	p := &Proxy{
		Resolver:      resolver,
		Lifecycle:     lc,
		Tracker:       idle.NewTracker(),
		DashboardPort: upstreamPort,
		APIPort:       upstreamPort,
		APIKey:        apiKey,
		WakeTimeout:   200 * time.Millisecond,
	}
	return p, clients
}

func upstreamPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return port
}

func TestSplitUserPath(t *testing.T) {
	cases := []struct {
		in         string
		user, rest string
		ok         bool
	}{
		{"/u/alice/api/health", "alice", "api/health", true},
		{"/u/alice", "alice", "", true},
		{"/u/alice/", "alice", "", true},
		{"/u/", "", "", false},
		{"/other", "", "", false},
	}
	for _, tc := range cases {
		user, rest, ok := splitUserPath(tc.in)
		if user != tc.user || rest != tc.rest || ok != tc.ok {
			t.Errorf("splitUserPath(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.in, user, rest, ok, tc.user, tc.rest, tc.ok)
		}
	}
}

func TestProxyAuth(t *testing.T) {
	up := &echoUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()
	p, _ := newProxyFixture(t, "Ready", upstreamPort(t, srv))

	// no token
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "/u/alice/api/health", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
	// wrong token
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/u/alice/api/health", nil)
	r.Header.Set("Authorization", "Bearer nope")
	p.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", w.Code)
	}
	// unknown user — same 401, not 404
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/u/ghost/api/health", nil)
	r.Header.Set("Authorization", "Bearer "+userToken)
	p.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unknown user: %d", w.Code)
	}
}

func TestProxyDashboardPassThrough(t *testing.T) {
	up := &echoUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()
	p, _ := newProxyFixture(t, "Ready", upstreamPort(t, srv))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/u/alice/api/health?token="+userToken+"&x=1", nil)
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	if up.lastPath != "/api/health" {
		t.Errorf("upstream path = %s", up.lastPath)
	}
	if up.lastAuth != "" {
		t.Errorf("client credentials leaked upstream: %q", up.lastAuth)
	}
	if up.lastTok != "" {
		t.Errorf("token query param leaked upstream: %q", up.lastTok)
	}
}

func TestProxyAPIKeyInjection(t *testing.T) {
	up := &echoUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()
	p, _ := newProxyFixture(t, "Ready", upstreamPort(t, srv))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/u/alice/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+userToken)
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	if up.lastPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %s", up.lastPath)
	}
	if up.lastAuth != "Bearer "+apiKey {
		t.Errorf("expected platform key injected, got %q", up.lastAuth)
	}
}

func TestProxyWakeTimeout503(t *testing.T) {
	up := &echoUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()
	// Suspended + no controller in fakes -> Resume patches then times out.
	p, clients := newProxyFixture(t, "Suspended", upstreamPort(t, srv))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/u/alice/api/health", nil)
	r.Header.Set("Authorization", "Bearer "+userToken)
	p.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After")
	}
	// The wake attempt must have flipped operatingMode back to Running.
	sb, _ := clients.Core.AgentsV1beta1().Sandboxes(ns).Get(context.Background(), "hermes-pool-p", metav1.GetOptions{})
	if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Errorf("wake did not patch operatingMode: %s", sb.Spec.OperatingMode)
	}
}
