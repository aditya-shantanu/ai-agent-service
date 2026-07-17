package sandbox

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEarliestCronRun(t *testing.T) {
	boolp := func(b bool) *bool { return &b }
	_ = boolp
	cases := []struct {
		name    string
		json    string
		want    string // RFC3339 or "" for ok=false
		wantErr bool
	}{
		{"empty input", "", "", false},
		{"no jobs dict", `{"jobs":[]}`, "", false},
		{"bare list accepted", `[{"id":"a","enabled":true,"next_run_at":"2026-07-18T09:00:00+00:00"}]`,
			"2026-07-18T09:00:00Z", false},
		{"earliest of several", `{"jobs":[
			{"id":"late","next_run_at":"2026-07-18T12:00:00+00:00"},
			{"id":"early","next_run_at":"2026-07-18T09:00:00+00:00"}]}`,
			"2026-07-18T09:00:00Z", false},
		{"disabled jobs skipped", `{"jobs":[
			{"id":"off","enabled":false,"next_run_at":"2026-07-18T01:00:00+00:00"},
			{"id":"on","enabled":true,"next_run_at":"2026-07-18T09:00:00+00:00"}]}`,
			"2026-07-18T09:00:00Z", false},
		{"enabled-absent means enabled", `{"jobs":[{"id":"a","next_run_at":"2026-07-18T09:00:00+00:00"}]}`,
			"2026-07-18T09:00:00Z", false},
		{"naive timestamp treated as UTC", `{"jobs":[{"id":"a","next_run_at":"2026-07-18T09:00:00"}]}`,
			"2026-07-18T09:00:00Z", false},
		{"fractional seconds", `{"jobs":[{"id":"a","next_run_at":"2026-07-18T09:00:00.123456+00:00"}]}`,
			"2026-07-18T09:00:00Z", false}, // truncation not required; compare via .Truncate below
		{"bad timestamp skipped, good survives", `{"jobs":[
			{"id":"bad","next_run_at":"not-a-time"},
			{"id":"good","next_run_at":"2026-07-18T09:00:00+00:00"}]}`,
			"2026-07-18T09:00:00Z", false},
		{"only bad timestamps -> none", `{"jobs":[{"id":"bad","next_run_at":"nope"}]}`, "", false},
		{"no next_run_at -> none", `{"jobs":[{"id":"a"}]}`, "", false},
		{"garbage document", `"what"`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := EarliestCronRun([]byte(tc.json))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if ok {
					t.Fatalf("expected no result, got %v", got)
				}
				return
			}
			want, _ := time.Parse(time.RFC3339, tc.want)
			if !ok || !got.Truncate(time.Second).Equal(want) {
				t.Fatalf("got (%v, %v), want %v", got, ok, want)
			}
		})
	}
}

type recordingExec struct {
	stdout string
	err    error
	calls  [][]string
}

func (f *recordingExec) Exec(_ context.Context, _, pod, container string, command []string) (string, string, error) {
	f.calls = append(f.calls, append([]string{pod, container}, command...))
	return f.stdout, "", f.err
}

func TestSuspendCapturesCronWake(t *testing.T) {
	_, resolver, _, lc := newFixture(t,
		claimFor("carl", "hermes-pool-c"),
		sandboxFor("hermes-pool-c", "Running", true, false),
	)
	exec := &recordingExec{stdout: `{"jobs":[{"id":"j","next_run_at":"2026-07-18T09:00:00+00:00"}]}`}
	lc.Exec = exec
	ctx := context.Background()

	if _, err := lc.Suspend(ctx, "carl"); err != nil {
		t.Fatal(err)
	}
	claim, _ := lc.Clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Get(ctx, ClaimName("carl"), metav1.GetOptions{})
	if got := claim.Annotations[AnnotationNextCronWake]; got != "2026-07-18T09:00:00Z" {
		t.Errorf("next-cron-wake = %q", got)
	}
	if len(exec.calls) != 1 {
		t.Errorf("exec calls = %d", len(exec.calls))
	}
	_ = resolver
}

func TestSuspendWithoutJobsClearsAnnotation(t *testing.T) {
	c := claimFor("nia", "hermes-pool-n")
	c.Annotations[AnnotationNextCronWake] = "2026-01-01T00:00:00Z" // stale from a prior suspend
	_, _, _, lc := newFixture(t, c,
		sandboxFor("hermes-pool-n", "Running", true, false),
	)
	lc.Exec = &recordingExec{stdout: `{"jobs":[]}`}
	ctx := context.Background()

	if _, err := lc.Suspend(ctx, "nia"); err != nil {
		t.Fatal(err)
	}
	claim, _ := lc.Clients.Ext.ExtensionsV1beta1().SandboxClaims(ns).Get(ctx, ClaimName("nia"), metav1.GetOptions{})
	if got, present := claim.Annotations[AnnotationNextCronWake]; present && got != "" {
		t.Errorf("stale next-cron-wake not cleared: %q", got)
	}
}

func TestSuspendProceedsWhenCaptureFails(t *testing.T) {
	_, _, _, lc := newFixture(t,
		claimFor("erin", "hermes-pool-e"),
		sandboxFor("hermes-pool-e", "Running", true, false),
	)
	lc.Exec = &recordingExec{err: context.DeadlineExceeded}
	ua, err := lc.Suspend(context.Background(), "erin")
	if err != nil {
		t.Fatalf("suspend must proceed despite capture failure: %v", err)
	}
	if ua.State != StateSuspending {
		t.Errorf("state = %s", ua.State)
	}
}
