// Package auth implements the two credential kinds of the platform:
// a static admin token for /api/v1 management calls, and per-user bearer
// tokens whose SHA-256 lives as an annotation on the user's claim.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
)

// MintToken returns a new user bearer token and its hex SHA-256.
func MintToken() (token, sha256hex string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken returns the hex SHA-256 of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyToken constant-time-compares a presented token against a stored hash.
func VerifyToken(token, storedSHA256Hex string) bool {
	if token == "" || storedSHA256Hex == "" {
		return false
	}
	presented := HashToken(token)
	return subtle.ConstantTimeCompare([]byte(presented), []byte(storedSHA256Hex)) == 1
}

// SessionCookie is the gateway's own browser-session cookie: the proxy sets
// it (path-scoped to /u/{user}) after a successful ?token= request, because
// browsers drop the query param on every subsequent navigation, redirect,
// asset load and fetch(). It holds the same bearer token and is verified
// the same way; it is never forwarded upstream.
const SessionCookie = "hermes_gw_token"

// BearerFromRequest extracts a bearer token from the Authorization header or,
// as a fallback for browser WebSocket clients, the ?token= query parameter.
func BearerFromRequest(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

// TokenFromRequest is BearerFromRequest plus the gateway session cookie —
// the user-proxy credential order: header, query param, cookie. The admin
// API deliberately keeps the stricter BearerFromRequest.
func TokenFromRequest(r *http.Request) string {
	if tok := BearerFromRequest(r); tok != "" {
		return tok
	}
	if c, err := r.Cookie(SessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// RequireAdmin wraps a handler with static-admin-token auth.
func RequireAdmin(adminToken string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := BearerFromRequest(r)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(adminToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
