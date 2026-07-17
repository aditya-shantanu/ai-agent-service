package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMintAndVerify(t *testing.T) {
	token, hash, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 40 {
		t.Errorf("token too short: %d chars", len(token))
	}
	if !VerifyToken(token, hash) {
		t.Error("freshly minted token failed verification")
	}
	if VerifyToken("wrong", hash) {
		t.Error("wrong token verified")
	}
	if VerifyToken("", hash) || VerifyToken(token, "") {
		t.Error("empty inputs must never verify")
	}
	if HashToken(token) != hash {
		t.Error("HashToken mismatch with MintToken hash")
	}
}

func TestTokensAreUnique(t *testing.T) {
	a, _, _ := MintToken()
	b, _, _ := MintToken()
	if a == b {
		t.Fatal("two minted tokens are identical")
	}
}

func TestBearerFromRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer tok123")
	if got := BearerFromRequest(r); got != "tok123" {
		t.Errorf("header bearer = %q", got)
	}
	r = httptest.NewRequest("GET", "/x?token=qtok", nil)
	if got := BearerFromRequest(r); got != "qtok" {
		t.Errorf("query bearer = %q", got)
	}
	r = httptest.NewRequest("GET", "/x", nil)
	if got := BearerFromRequest(r); got != "" {
		t.Errorf("no-auth bearer = %q", got)
	}
}

func TestRequireAdmin(t *testing.T) {
	const admin = "super-secret-admin-token"
	called := false
	h := RequireAdmin(admin, func(w http.ResponseWriter, r *http.Request) { called = true })

	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusUnauthorized || called {
		t.Errorf("no token: code=%d called=%v", w.Code, called)
	}

	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	h(w, r)
	if w.Code != http.StatusUnauthorized || called {
		t.Errorf("bad token: code=%d called=%v", w.Code, called)
	}

	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+admin)
	h(w, r)
	if w.Code != http.StatusOK || !called {
		t.Errorf("good token: code=%d called=%v", w.Code, called)
	}
}
