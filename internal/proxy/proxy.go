// Package proxy is the user-facing data plane: it authenticates per-user
// bearer tokens, wakes suspended sandboxes on connect, and reverse-proxies
// HTTP + WebSocket traffic to the user's Hermes pod.
//
// Path scheme (README decision 2):
//
//	/u/{user}/v1/...   -> sandbox :8642 (OpenAI-compatible API; platform key injected)
//	/u/{user}/...      -> sandbox :9119 (dashboard; cookie login flows through)
package proxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/aditya-shantanu/ai-agent-service/internal/auth"
	"github.com/aditya-shantanu/ai-agent-service/internal/idle"
	"github.com/aditya-shantanu/ai-agent-service/internal/sandbox"
)

type Proxy struct {
	Resolver  *sandbox.Resolver
	Lifecycle *sandbox.Lifecycle
	Tracker   *idle.Tracker

	DashboardPort int
	APIPort       int
	// APIKey is the shared API_SERVER_KEY injected for /v1 requests.
	APIKey      string
	WakeTimeout time.Duration

	// transport is shared across upstreams; dial retries absorb the
	// just-woke window where the pod is Ready but uvicorn is still binding.
	transport http.RoundTripper
}

func (p *Proxy) init() {
	if p.transport == nil {
		p.transport = &retryTransport{
			inner: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
				ForceAttemptHTTP2:     false,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 5 * time.Minute, // agent responses can be slow
			},
			attempts: 5,
			backoff:  500 * time.Millisecond,
		}
	}
}

// ServeHTTP handles /u/{user}/rest...
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.init()

	userID, rest, ok := splitUserPath(r.URL.Path)
	if !ok {
		http.Error(w, `{"error":"path must be /u/{user}/..."}`, http.StatusNotFound)
		return
	}

	// Authenticate against the token hash stored on the user's claim.
	ua, err := p.Resolver.Resolve(r.Context(), userID)
	if err != nil {
		if err == sandbox.ErrUserNotFound {
			// Same status for unknown user and bad token: don't leak user IDs.
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		slog.Error("proxy resolve", "user", userID, "err", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	tok := auth.TokenFromRequest(r)
	if !auth.VerifyToken(tok, ua.TokenSHA256) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	// Browser bootstrap: a valid ?token= is promoted to a path-scoped
	// session cookie, because the browser drops the query param on every
	// redirect, asset load and SPA fetch that follows. Same token, same
	// constant-time verification on every request; rotation invalidates it.
	if r.URL.Query().Get("token") == tok {
		if c, err := r.Cookie(auth.SessionCookie); err != nil || c.Value != tok {
			http.SetCookie(w, &http.Cookie{
				Name:     auth.SessionCookie,
				Value:    tok,
				Path:     "/u/" + userID,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
			})
		}
	}

	// Wake-on-connect: hold the request while the sandbox resumes.
	if ua.State != sandbox.StateReady {
		ua, err = p.Lifecycle.Resume(r.Context(), userID, p.WakeTimeout, "connect")
		if err != nil || ua == nil || ua.State != sandbox.StateReady {
			slog.Warn("wake-on-connect timeout", "user", userID, "err", err)
			w.Header().Set("Retry-After", "10")
			http.Error(w, `{"error":"agent is waking up, retry shortly"}`, http.StatusServiceUnavailable)
			return
		}
	}
	if ua.ServiceFQDN == "" {
		http.Error(w, `{"error":"agent has no endpoint"}`, http.StatusBadGateway)
		return
	}

	// Track activity for the idle suspender. Held for the full request —
	// including upgraded WebSockets, whose handlers block until close.
	done := p.Tracker.Begin(userID)
	defer done()

	port := p.DashboardPort
	injectAPIKey := false
	if rest == "v1" || strings.HasPrefix(rest, "v1/") {
		port = p.APIPort
		injectAPIKey = true
	}

	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", ua.ServiceFQDN, port)}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/" + rest
			pr.Out.URL.RawPath = ""
			pr.SetXForwarded()
			// Hermes honors X-Forwarded-Prefix natively (redirect Locations,
			// cookie Path scoping, OAuth redirect_uri, SPA asset URLs) — tell
			// it where its subtree is mounted.
			pr.Out.Header.Set("X-Forwarded-Prefix", "/u/"+userID)

			// Never forward platform credentials upstream...
			pr.Out.Header.Del("Authorization")
			q := pr.Out.URL.Query()
			q.Del("token")
			pr.Out.URL.RawQuery = q.Encode()
			// ...including the gateway session cookie (Hermes' own
			// cookies — sso_attempt, login session — must flow).
			if cookies := pr.In.Cookies(); len(cookies) > 0 {
				pr.Out.Header.Del("Cookie")
				for _, c := range cookies {
					if c.Name != auth.SessionCookie {
						pr.Out.AddCookie(c)
					}
				}
			}
			// ...but inject the shared key for the OpenAI-compatible API.
			if injectAPIKey {
				pr.Out.Header.Set("Authorization", "Bearer "+p.APIKey)
			}

			// Strip Origin on upgrades: the dashboard's CSRF/origin checks
			// see the internal host otherwise (same trick as sandbox-router).
			if isUpgrade(pr.In) {
				pr.Out.Header.Del("Origin")
			}
		},
		// Hermes issues host-relative redirects (e.g. the dashboard's
		// unauthenticated 302 -> /auth/login). Un-prefixed they escape the
		// user's /u/{id}/ subtree and 404 on the gateway mux, so re-anchor
		// them. Absolute redirects to external hosts pass through untouched.
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("Location", rewriteRedirect(resp.Header.Get("Location"), userID))
			if resp.Header.Get("Location") == "" {
				resp.Header.Del("Location")
			}
			return nil
		},
		Transport:     p.transport,
		FlushInterval: -1, // immediate flush: SSE + streaming chat
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("proxy upstream error", "user", userID, "err", err)
			w.Header().Set("Retry-After", "5")
			http.Error(w, `{"error":"upstream unavailable"}`, http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// rewriteRedirect re-anchors an upstream redirect under /u/{user}. Only
// host-relative Locations (path-absolute, no host) are rewritten; empty,
// external, and unparseable values are returned as-is.
//
// It also defuses an upstream trap (Hermes v2026.7.7.2): the dashboard's
// auto-SSO middleware 302s first-time unauthenticated visits to
// /auth/login?provider=basic — but BasicAuthProvider has no OAuth redirect
// flow, so that route raises NotImplementedError (500). The working page is
// the /login interstitial (which POSTs /auth/password-login), so we send
// browsers there directly. OAuth providers (provider != basic) are
// untouched.
func rewriteRedirect(loc, userID string) string {
	if loc == "" {
		return loc
	}
	u, err := url.Parse(loc)
	if err != nil || u.Host != "" || u.Scheme != "" || !strings.HasPrefix(u.Path, "/") {
		return loc
	}
	prefix := "/u/" + userID
	if u.Path != prefix && !strings.HasPrefix(u.Path, prefix+"/") {
		u.Path = prefix + u.Path
	}
	if u.Path == prefix+"/auth/login" {
		if q := u.Query(); q.Get("provider") == "basic" {
			q.Del("provider")
			u.Path = prefix + "/login"
			u.RawQuery = q.Encode()
		}
	}
	return u.String()
}

// splitUserPath parses /u/{user}/rest... -> (user, "rest...", true).
func splitUserPath(path string) (string, string, bool) {
	trimmed := strings.TrimPrefix(path, "/u/")
	if trimmed == path || trimmed == "" {
		return "", "", false
	}
	parts := strings.SplitN(trimmed, "/", 2)
	user := parts[0]
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}
	if user == "" {
		return "", "", false
	}
	return user, rest, true
}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// retryTransport retries dial-level failures (connection refused, DNS not yet
// propagated) with backoff. Dial errors happen before any bytes are sent, so
// retrying is safe for all methods.
type retryTransport struct {
	inner    http.RoundTripper
	attempts int
	backoff  time.Duration
}

func (t *retryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var lastErr error
	backoff := t.backoff
	for i := 0; i < t.attempts; i++ {
		resp, err := t.inner.RoundTrip(r)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isDialError(err) {
			return nil, err
		}
		select {
		case <-r.Context().Done():
			return nil, lastErr
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return nil, lastErr
}

func isDialError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}
