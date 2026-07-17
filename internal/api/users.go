// Package api implements the /api/v1 management REST surface.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/adityashantanu/ai-agent-service/internal/auth"
	"github.com/adityashantanu/ai-agent-service/internal/sandbox"
	"github.com/adityashantanu/ai-agent-service/internal/telegram"
)

type Handlers struct {
	Provisioner *sandbox.Provisioner
	Lifecycle   *sandbox.Lifecycle
	Resolver    *sandbox.Resolver
	Telegram    *telegram.Injector // nil disables telegram endpoints

	ProvisionTimeout time.Duration
	WakeTimeout      time.Duration
}

type userResponse struct {
	UserID      string `json:"userId"`
	State       string `json:"state"`
	Claim       string `json:"claim"`
	SandboxName string `json:"sandboxName,omitempty"`
	ServiceFQDN string `json:"serviceFQDN,omitempty"`
	Exempt      bool   `json:"suspendExempt"`
	// Token is only set on initial creation and rotation.
	Token string `json:"token,omitempty"`
	Note  string `json:"note,omitempty"`
}

func toResponse(ua *sandbox.UserAgent) userResponse {
	return userResponse{
		UserID:      ua.UserID,
		State:       string(ua.State),
		Claim:       ua.ClaimName,
		SandboxName: ua.SandboxName,
		ServiceFQDN: ua.ServiceFQDN,
		Exempt:      ua.Exempt,
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (h *Handlers) errStatus(w http.ResponseWriter, err error) {
	if errors.Is(err, sandbox.ErrUserNotFound) {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	slog.Error("api error", "err", err)
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// CreateUser handles POST /api/v1/users {"userId": "..."}.
// 201 with one-time token on creation; 200 without token on idempotent replay.
func (h *Handlers) CreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID string `json:"userId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == "" {
		writeErr(w, http.StatusBadRequest, "body must be {\"userId\": \"...\"}")
		return
	}
	if err := sandbox.ValidateUserID(body.UserID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	token, tokenHash, err := auth.MintToken()
	if err != nil {
		h.errStatus(w, err)
		return
	}

	ua, created, err := h.Provisioner.EnsureUser(r.Context(), body.UserID, tokenHash)
	if err != nil {
		h.errStatus(w, err)
		return
	}
	if !created {
		resp := toResponse(ua)
		resp.Note = "user already exists; rotate the token via POST /api/v1/users/{id}/token if needed"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.ProvisionTimeout)
	defer cancel()
	ready, werr := h.Provisioner.WaitAdopted(ctx, body.UserID, h.ProvisionTimeout)
	if werr != nil {
		// Claim exists; report current state rather than failing the create.
		slog.Warn("user created but not Ready in time", "user", body.UserID, "err", werr)
		if ready == nil {
			ready = ua
		}
	}
	resp := toResponse(ready)
	resp.Token = token
	writeJSON(w, http.StatusCreated, resp)
}

// GetUser handles GET /api/v1/users/{id}.
func (h *Handlers) GetUser(w http.ResponseWriter, r *http.Request) {
	ua, err := h.Resolver.Resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		h.errStatus(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(ua))
}

// ListUsers handles GET /api/v1/users.
func (h *Handlers) ListUsers(w http.ResponseWriter, r *http.Request) {
	uas, err := h.Resolver.List(r.Context())
	if err != nil {
		h.errStatus(w, err)
		return
	}
	out := make([]userResponse, 0, len(uas))
	for _, ua := range uas {
		out = append(out, toResponse(ua))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// Suspend handles POST /api/v1/users/{id}/suspend.
func (h *Handlers) Suspend(w http.ResponseWriter, r *http.Request) {
	ua, err := h.Lifecycle.Suspend(r.Context(), r.PathValue("id"))
	if err != nil {
		h.errStatus(w, err)
		return
	}
	resp := toResponse(ua)
	if ua.Exempt {
		resp.Note = "user is suspend-exempt (telegram); explicit suspend honored but the bot will be offline"
	}
	writeJSON(w, http.StatusOK, resp)
}

// Resume handles POST /api/v1/users/{id}/resume.
func (h *Handlers) Resume(w http.ResponseWriter, r *http.Request) {
	ua, err := h.Lifecycle.Resume(r.Context(), r.PathValue("id"), h.WakeTimeout)
	if err != nil {
		if ua != nil {
			// Timed out but not fatal — report state.
			resp := toResponse(ua)
			resp.Note = "resume requested; not Ready within timeout"
			writeJSON(w, http.StatusAccepted, resp)
			return
		}
		h.errStatus(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(ua))
}

// RotateToken handles POST /api/v1/users/{id}/token.
func (h *Handlers) RotateToken(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	if _, err := h.Resolver.Resolve(r.Context(), userID); err != nil {
		h.errStatus(w, err)
		return
	}
	token, tokenHash, err := auth.MintToken()
	if err != nil {
		h.errStatus(w, err)
		return
	}
	if err := h.Lifecycle.SetTokenHash(r.Context(), userID, tokenHash); err != nil {
		h.errStatus(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"userId": userID, "token": token})
}

// SetTelegramToken handles PUT /api/v1/users/{id}/telegram-token.
func (h *Handlers) SetTelegramToken(w http.ResponseWriter, r *http.Request) {
	if h.Telegram == nil {
		writeErr(w, http.StatusNotImplemented, "telegram support disabled")
		return
	}
	var body struct {
		Token        string `json:"token"`
		AllowedUsers string `json:"allowedUsers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeErr(w, http.StatusBadRequest, "body must be {\"token\": \"...\", \"allowedUsers\": \"id1,id2\"}")
		return
	}
	userID := r.PathValue("id")
	if err := h.Telegram.SetToken(r.Context(), userID, body.Token, body.AllowedUsers); err != nil {
		if errors.Is(err, sandbox.ErrUserNotFound) {
			writeErr(w, http.StatusNotFound, "user not found")
			return
		}
		slog.Error("telegram inject", "user", userID, "err", err)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"userId":        userID,
		"telegram":      true,
		"suspendExempt": true,
		"note":          "bot connected in-pod; user is now exempt from idle suspension",
	})
}

// DeleteTelegramToken handles DELETE /api/v1/users/{id}/telegram-token.
func (h *Handlers) DeleteTelegramToken(w http.ResponseWriter, r *http.Request) {
	if h.Telegram == nil {
		writeErr(w, http.StatusNotImplemented, "telegram support disabled")
		return
	}
	userID := r.PathValue("id")
	if err := h.Telegram.RemoveToken(r.Context(), userID); err != nil {
		if errors.Is(err, sandbox.ErrUserNotFound) {
			writeErr(w, http.StatusNotFound, "user not found")
			return
		}
		h.errStatus(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"userId": userID, "telegram": false, "suspendExempt": false})
}

// DeleteUser handles DELETE /api/v1/users/{id}.
func (h *Handlers) DeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := h.Provisioner.DeleteUser(r.Context(), r.PathValue("id")); err != nil {
		h.errStatus(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"userId": r.PathValue("id"),
		"note":   "claim deleted; sandbox, PVC and owned secrets are garbage-collected",
	})
}
