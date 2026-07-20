package bench_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/adityashantanu/ai-agent-service/internal/bench"
)

// TestStreamChatTTFT verifies TTFT means first *token*, not first byte:
// an early heartbeat with an empty content delta must not stop the clock.
func TestStreamChatTTFT(t *testing.T) {
	const tokenDelay = 80 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/u/alice/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n") // heartbeat, no token
		fl.Flush()
		time.Sleep(tokenDelay)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := bench.NewClient(srv.URL, "admin", 5*time.Second)
	res := c.StreamChatTTFT(t.Context(), "alice", "tok")
	if res.Err != nil || res.Status != 200 {
		t.Fatalf("stream: status %d err %v", res.Status, res.Err)
	}
	if !res.GotToken {
		t.Fatal("no token detected")
	}
	if res.First < tokenDelay {
		t.Errorf("TTFT %v < token delay %v — clock stopped at first byte, not first token", res.First, tokenDelay)
	}
	if res.Total < res.First {
		t.Errorf("total %v < first %v", res.Total, res.First)
	}
}

func TestStreamChatTTFTUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no provider key"}`, http.StatusBadGateway)
	}))
	defer srv.Close()

	c := bench.NewClient(srv.URL, "admin", 5*time.Second)
	res := c.StreamChatTTFT(t.Context(), "alice", "tok")
	if res.Status != http.StatusBadGateway || res.GotToken {
		t.Errorf("res = %+v", res)
	}
}

func TestProbeModelsCapturesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10")
		http.Error(w, `{"error":"agent is waking up, retry shortly"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := bench.NewClient(srv.URL, "admin", 5*time.Second)
	p := c.ProbeModels(t.Context(), "alice", "tok")
	if p.Status != 503 || p.RetryAfter != "10" {
		t.Errorf("probe = %+v", p)
	}
}
