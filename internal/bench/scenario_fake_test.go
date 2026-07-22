package bench_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/aditya-shantanu/ai-agent-service/internal/bench"
)

// fakeGateway is an in-memory hermes-gateway good enough to exercise the
// scenario state machines: create/suspend/exempt/delete plus a models
// probe that emulates the wake-on-connect hold with a configurable delay.
type fakeGateway struct {
	mu    sync.Mutex
	users map[string]*fakeUser

	wakeDelay   time.Duration
	sandboxName string // assigned to new users ("hermes-pool-x" = warm adoption)
	// fail503 makes the next N suspended-probe attempts return 503 with
	// this Retry-After value (empty = the correctness bug case).
	fail503    int
	retryAfter string

	onExempt func(exempt bool) // observation hook
}

type fakeUser struct {
	state          string
	exempt         bool
	lastWakeReason string
	token          string
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{
		users:       map[string]*fakeUser{},
		sandboxName: "hermes-pool-fake",
		retryAfter:  "1",
	}
}

func (f *fakeGateway) userJSON(id string, u *fakeUser, withToken bool) map[string]any {
	resp := map[string]any{
		"userId": id, "state": u.state, "sandboxName": f.sandboxName,
		"suspendExempt": u.exempt, "lastWakeReason": u.lastWakeReason,
	}
	if withToken {
		resp["token"] = u.token
	}
	return resp
}

func (f *fakeGateway) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UserID string `json:"userId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		u := &fakeUser{state: "Ready", token: "tok-" + body.UserID}
		f.users[body.UserID] = u
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(f.userJSON(body.UserID, u, true))
	})
	mux.HandleFunc("GET /api/v1/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		u, ok := f.users[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(f.userJSON(r.PathValue("id"), u, false))
	})
	mux.HandleFunc("DELETE /api/v1/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.users[r.PathValue("id")]; !ok {
			http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
			return
		}
		delete(f.users, r.PathValue("id"))
		_ = json.NewEncoder(w).Encode(map[string]string{"userId": r.PathValue("id")})
	})
	mux.HandleFunc("POST /api/v1/users/{id}/suspend", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		u, ok := f.users[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
			return
		}
		u.state = "Suspended"
		_ = json.NewEncoder(w).Encode(f.userJSON(r.PathValue("id"), u, false))
	})
	mux.HandleFunc("PUT /api/v1/users/{id}/suspend-exempt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Exempt *bool `json:"exempt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		u, ok := f.users[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
			return
		}
		u.exempt = *body.Exempt
		if f.onExempt != nil {
			f.onExempt(u.exempt)
		}
		_ = json.NewEncoder(w).Encode(f.userJSON(r.PathValue("id"), u, false))
	})
	mux.HandleFunc("GET /u/{user}/v1/models", func(w http.ResponseWriter, r *http.Request) {
		user := r.PathValue("user")
		f.mu.Lock()
		u, ok := f.users[user]
		if !ok || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
			f.mu.Unlock()
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		suspended := u.state == "Suspended"
		if suspended && f.fail503 > 0 {
			f.fail503--
			ra := f.retryAfter
			f.mu.Unlock()
			if ra != "" {
				w.Header().Set("Retry-After", ra)
			}
			http.Error(w, `{"error":"agent is waking up, retry shortly"}`, http.StatusServiceUnavailable)
			return
		}
		f.mu.Unlock()
		if suspended {
			time.Sleep(f.wakeDelay) // the wake-on-connect hold
			f.mu.Lock()
			u.state = "Ready"
			u.lastWakeReason = "connect"
			f.mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{}})
	})

	return mux
}

func newFakeEnv(f *fakeGateway) (*bench.Env, *httptest.Server) {
	srv := httptest.NewServer(f.handler())
	return &bench.Env{
		Client:     bench.NewClient(srv.URL, "admin", 10*time.Second),
		EnvName:    "kind",
		UserPrefix: "bench",
	}, srv
}
