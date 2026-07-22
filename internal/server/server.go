// Package server wires the HTTP mux: management API now (M3);
// the user proxy mounts under /u/ in M4.
package server

import (
	"net/http"

	"github.com/aditya-shantanu/ai-agent-service/internal/api"
	"github.com/aditya-shantanu/ai-agent-service/internal/auth"
	"github.com/aditya-shantanu/ai-agent-service/internal/config"
)

func New(cfg *config.Config, h *api.Handlers, proxy http.Handler) http.Handler {
	mux := http.NewServeMux()

	admin := func(fn http.HandlerFunc) http.HandlerFunc {
		return auth.RequireAdmin(cfg.AdminToken, fn)
	}

	mux.HandleFunc("POST /api/v1/users", admin(h.CreateUser))
	mux.HandleFunc("GET /api/v1/users", admin(h.ListUsers))
	mux.HandleFunc("GET /api/v1/users/{id}", admin(h.GetUser))
	mux.HandleFunc("POST /api/v1/users/{id}/suspend", admin(h.Suspend))
	mux.HandleFunc("POST /api/v1/users/{id}/resume", admin(h.Resume))
	mux.HandleFunc("POST /api/v1/users/{id}/token", admin(h.RotateToken))
	mux.HandleFunc("PUT /api/v1/users/{id}/suspend-exempt", admin(h.SetSuspendExempt))
	mux.HandleFunc("PUT /api/v1/users/{id}/telegram-token", admin(h.SetTelegramToken))
	mux.HandleFunc("DELETE /api/v1/users/{id}/telegram-token", admin(h.DeleteTelegramToken))
	mux.HandleFunc("DELETE /api/v1/users/{id}", admin(h.DeleteUser))

	if proxy != nil {
		mux.Handle("/u/", proxy)
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return mux
}
