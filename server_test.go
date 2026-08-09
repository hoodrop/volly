package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	launcherv1 "volly/gen/launcher/v1"
)

// newTestServer returns a launcherServer writing logs to t.TempDir().
func newTestServer(t *testing.T) *launcherServer {
	t.Helper()
	cfg := defaultConfig()
	cfg.LogDir = t.TempDir()
	return &launcherServer{cfg: cfg, loc: time.UTC}
}

func launchReq(target string, numCalls int32, interval time.Duration) *launcherv1.LaunchRequest {
	return &launcherv1.LaunchRequest{
		Method:   "GET",
		Url:      target,
		NumCalls: numCalls,
		Interval: durationpb.New(interval),
	}
}

// A burst against a 200-responding target wins, and the winner's status and
// body make it into the response.
func TestLaunchWins(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "hello")
	}))
	defer target.Close()

	s := newTestServer(t)
	resp, err := s.Launch(context.Background(), launchReq(target.URL, 5, 10*time.Millisecond))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !resp.GetWon() {
		t.Fatal("expected won=true against a 200 target")
	}
	if resp.GetWinningStatus() != http.StatusOK || resp.GetWinningBody() != "hello" {
		t.Errorf("unexpected winner: status=%d body=%q", resp.GetWinningStatus(), resp.GetWinningBody())
	}
	if resp.GetWinningId() < 1 || resp.GetWinningId() > 5 {
		t.Errorf("winning id %d out of range", resp.GetWinningId())
	}
	// Every goroutine either fires (jitter recorded) or skips; aborted is a
	// subset of fired (a request fires first, then may be cancelled in
	// flight), so the invariant is fired + skipped == numCalls.
	if resp.GetFired() < 1 || resp.GetFired()+resp.GetSkippedPending() != 5 {
		t.Errorf("counts don't add up: fired=%d skipped=%d, want fired+skipped=5",
			resp.GetFired(), resp.GetSkippedPending())
	}
	if resp.GetAbortedInflight() > resp.GetFired() {
		t.Errorf("aborted=%d exceeds fired=%d", resp.GetAbortedInflight(), resp.GetFired())
	}
	if resp.GetLogFile() == "" {
		t.Error("expected a log file path")
	}
}

// A target that never returns 200 fires everything and reports won=false.
func TestLaunchNoWin(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer target.Close()

	s := newTestServer(t)
	resp, err := s.Launch(context.Background(), launchReq(target.URL, 3, 5*time.Millisecond))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if resp.GetWon() {
		t.Fatal("expected won=false against a 404 target")
	}
	if resp.GetFired() != 3 {
		t.Errorf("fired = %d, want 3", resp.GetFired())
	}
}

// A second Launch while one is in progress is rejected with
// FailedPrecondition, not queued.
func TestLaunchRejectsConcurrentRun(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release // hold every request until the test lets go
	}))
	defer target.Close()

	s := newTestServer(t)

	first := make(chan error, 1)
	go func() {
		_, err := s.Launch(context.Background(), launchReq(target.URL, 1, 0))
		first <- err
	}()

	// Wait until the first run's request is actually in flight.
	for i := 0; i < 100 && hits.Load() == 0; i++ {
		time.Sleep(time.Millisecond)
	}

	_, err := s.Launch(context.Background(), launchReq(target.URL, 1, 0))
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("concurrent Launch: code = %v, want FailedPrecondition (err=%v)", code, err)
	}

	close(release)
	if err := <-first; err != nil {
		t.Errorf("first Launch: %v", err)
	}
}

// Invalid requests are rejected before anything is fired.
func TestLaunchValidation(t *testing.T) {
	s := newTestServer(t)

	cases := []struct {
		name string
		req  *launcherv1.LaunchRequest
	}{
		{"empty method", &launcherv1.LaunchRequest{Url: "http://example.com", NumCalls: 1}},
		{"relative url", &launcherv1.LaunchRequest{Method: "GET", Url: "/foo", NumCalls: 1}},
		{"zero calls", &launcherv1.LaunchRequest{Method: "GET", Url: "http://example.com", NumCalls: 0}},
		{"negative interval", &launcherv1.LaunchRequest{Method: "GET", Url: "http://example.com", NumCalls: 1, Interval: durationpb.New(-time.Second)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Launch(context.Background(), tc.req)
			if code := status.Code(err); code != codes.InvalidArgument {
				t.Errorf("code = %v, want InvalidArgument (err=%v)", code, err)
			}
		})
	}
}

// Content-Length headers from the caller are dropped, like the file parsers do.
func TestTemplateFromRequestStripsContentLength(t *testing.T) {
	tmpl, err := templateFromRequest(&launcherv1.LaunchRequest{
		Method:   "POST",
		Url:      "http://example.com/x",
		NumCalls: 1,
		Headers: []*launcherv1.Header{
			{Name: "Content-Length", Value: "9999"},
			{Name: "X-Auth", Value: "a"},
			{Name: "X-Auth", Value: "b"},
		},
		Body: "{}",
	})
	if err != nil {
		t.Fatalf("templateFromRequest: %v", err)
	}
	if got := tmpl.header.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length not stripped: %q", got)
	}
	if got := tmpl.header.Values("X-Auth"); len(got) != 2 {
		t.Errorf("multi-value header lost: %v", got)
	}
}
