package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotFound is returned by GetUser when the gateway reports 404.
var ErrNotFound = errors.New("user not found")

// User mirrors the subset of the gateway's user response the bench needs.
type User struct {
	UserID         string `json:"userId"`
	State          string `json:"state"`
	SandboxName    string `json:"sandboxName"`
	SuspendExempt  bool   `json:"suspendExempt"`
	LastWakeReason string `json:"lastWakeReason"`
	Token          string `json:"token"`
	Note           string `json:"note"`
}

// Client talks to the hermes-gateway management API (admin token) and the
// user proxy (per-user tokens). All measurement happens client-side: the
// gateway exposes no timing metadata, and client wall-time is the UX anyway.
type Client struct {
	BaseURL    string
	AdminToken string
	HTTP       *http.Client
}

// NewClient builds a Client whose per-request timeout must exceed the
// longest gateway hold it will measure (provision and wake waits).
func NewClient(baseURL, adminToken string, timeout time.Duration) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		AdminToken: adminToken,
		HTTP:       &http.Client{Timeout: timeout},
	}
}

func (c *Client) admin(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.AdminToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

// CreateUser provisions a user; the gateway blocks until warm adoption +
// Ready (or its provision timeout). 201 and 200 (idempotent replay) both
// succeed; only 201 carries a token.
func (c *Client) CreateUser(ctx context.Context, id string) (*User, error) {
	code, b, err := c.admin(ctx, "POST", "/api/v1/users", map[string]string{"userId": id})
	if err != nil {
		return nil, err
	}
	if code != http.StatusCreated && code != http.StatusOK {
		return nil, fmt.Errorf("create %s: HTTP %d: %s", id, code, trunc(b))
	}
	var u User
	if err := json.Unmarshal(b, &u); err != nil {
		return nil, fmt.Errorf("create %s: bad response: %w", id, err)
	}
	return &u, nil
}

func (c *Client) GetUser(ctx context.Context, id string) (*User, error) {
	code, b, err := c.admin(ctx, "GET", "/api/v1/users/"+id, nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("get %s: HTTP %d: %s", id, code, trunc(b))
	}
	var u User
	if err := json.Unmarshal(b, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (c *Client) Suspend(ctx context.Context, id string) error {
	code, b, err := c.admin(ctx, "POST", "/api/v1/users/"+id+"/suspend", nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("suspend %s: HTTP %d: %s", id, code, trunc(b))
	}
	return nil
}

func (c *Client) SetSuspendExempt(ctx context.Context, id string, exempt bool) error {
	code, b, err := c.admin(ctx, "PUT", "/api/v1/users/"+id+"/suspend-exempt", map[string]bool{"exempt": exempt})
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("suspend-exempt %s: HTTP %d: %s", id, code, trunc(b))
	}
	return nil
}

// WaitState polls until the user reaches the wanted state.
func (c *Client) WaitState(ctx context.Context, id, want string, timeout time.Duration) (*User, error) {
	deadline := time.Now().Add(timeout)
	var last *User
	for {
		u, err := c.GetUser(ctx, id)
		if err == nil {
			last = u
			if u.State == want {
				return u, nil
			}
		} else if errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if time.Now().After(deadline) {
			state := "unknown"
			if last != nil {
				state = last.State
			}
			return last, fmt.Errorf("user %s not %s within %s (last state %s)", id, want, timeout, state)
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return last, err
		}
	}
}

// DeleteUserAndWait deletes the user and polls until the gateway reports
// 404. Deleting a non-existent user is not an error (pre-clean usage).
// Waiting matters: re-creating over a terminating claim replays
// idempotently and returns no token (the simulate-users.sh lesson).
func (c *Client) DeleteUserAndWait(ctx context.Context, id string, timeout time.Duration) error {
	code, b, err := c.admin(ctx, "DELETE", "/api/v1/users/"+id, nil)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		return nil
	}
	if code != http.StatusOK {
		return fmt.Errorf("delete %s: HTTP %d: %s", id, code, trunc(b))
	}
	deadline := time.Now().Add(timeout)
	for {
		if _, err := c.GetUser(ctx, id); errors.Is(err, ErrNotFound) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("user %s still present %s after delete", id, timeout)
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return err
		}
	}
}

// Probe is one timed request against the user proxy.
type Probe struct {
	Status     int // 0 = transport error
	Duration   time.Duration
	RetryAfter string
	Body       string
	Err        error
}

// ProbeModels performs GET /u/{user}/v1/models with the user token, timed
// from request write to full body read. Requests against a suspended agent
// are held by the gateway's wake-on-connect — that hold IS the measurement.
// Connection reuse is disabled so every sample pays the same TCP setup.
func (c *Client) ProbeModels(ctx context.Context, user, token string) Probe {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/u/"+user+"/v1/models", nil)
	if err != nil {
		return Probe{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Close = true
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Probe{Duration: time.Since(start), Err: err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return Probe{
		Status:     resp.StatusCode,
		Duration:   time.Since(start),
		RetryAfter: resp.Header.Get("Retry-After"),
		Body:       trunc(b),
	}
}

// TTFT is one timed streaming chat turn.
type TTFT struct {
	Status   int
	First    time.Duration // to the first non-empty content delta (not first byte)
	Total    time.Duration // to [DONE] / stream end
	GotToken bool
	Body     string
	Err      error
}

// StreamChatTTFT sends a one-token streaming chat completion and measures
// time-to-first-token by incrementally parsing the SSE stream. Costs LLM
// credits; only the --ttft scenarios call it.
func (c *Client) StreamChatTTFT(ctx context.Context, user, token string) TTFT {
	payload := map[string]any{
		"model":      "hermes-agent",
		"stream":     true,
		"max_tokens": 16,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with exactly: ok"}},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/u/"+user+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return TTFT{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Close = true
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return TTFT{Total: time.Since(start), Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return TTFT{Status: resp.StatusCode, Total: time.Since(start), Body: trunc(body)}
	}
	res := TTFT{Status: resp.StatusCode}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		if !res.GotToken && sseHasContent(data) {
			res.First = time.Since(start)
			res.GotToken = true
		}
	}
	res.Total = time.Since(start)
	if err := sc.Err(); err != nil && !res.GotToken {
		res.Err = err
	}
	return res
}

func sseHasContent(data string) bool {
	var ev struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &ev) != nil {
		return false
	}
	for _, ch := range ev.Choices {
		if ch.Delta.Content != "" {
			return true
		}
	}
	return false
}

func trunc(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
