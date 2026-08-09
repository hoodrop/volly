package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	launcherv1 "volly/gen/launcher/v1"
)

// launcherServer implements the gRPC API (proto/launcher/v1/launcher.proto).
// The request template arrives inline in each LaunchRequest — serve mode
// never reads request.txt; that file stays the interactive CLI's input.
type launcherServer struct {
	launcherv1.UnimplementedLauncherServer

	cfg config
	loc *time.Location

	// runMu serializes runs: SCHED_FIFO pinning, busy-spinning and the
	// first-200-wins semantics are all per-run, so concurrent bursts would
	// only interfere with each other. A second Launch while one is in
	// progress is rejected, not queued.
	runMu sync.Mutex
}

// serve runs the gRPC API until SIGINT/SIGTERM. Scheduling (SCHED_FIFO, CPU
// pinning) is process-wide, so it is applied once here rather than per run.
func serve(cfg config, loc *time.Location) {
	applyScheduling(cfg, func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	})

	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		fmt.Printf("cannot listen on %s: %v\n", cfg.GRPCListen, err)
		return
	}

	srv := grpc.NewServer()
	launcherv1.RegisterLauncherServer(srv, &launcherServer{cfg: cfg, loc: loc})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		fmt.Println("\nshutting down, waiting for the active run to finish...")
		srv.GracefulStop()
	}()

	fmt.Printf("gRPC API listening on %s (plaintext, intended for localhost)\n", lis.Addr())
	if err := srv.Serve(lis); err != nil {
		fmt.Printf("gRPC server: %v\n", err)
	}
}

// Launch runs one burst and blocks until it ends. The RPC's context drives
// the whole run: a client disconnect cancels the wait and every in-flight
// request, same as the first 200 does.
func (s *launcherServer) Launch(ctx context.Context, req *launcherv1.LaunchRequest) (*launcherv1.LaunchResponse, error) {
	if !s.runMu.TryLock() {
		return nil, status.Error(codes.FailedPrecondition, "a run is already in progress")
	}
	defer s.runMu.Unlock()

	tmpl, err := templateFromRequest(req)
	if err != nil {
		return nil, err
	}

	// One fresh timestamped log file per run, exactly like a CLI run.
	log, err := newLogger(s.cfg.LogDir, s.loc)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cannot set up logging: %v", err)
	}
	defer log.close()
	log.logf("config: %s %s calls=%d interval=%v timezone=%s format=grpc body=%s",
		tmpl.method, tmpl.url, req.GetNumCalls(), req.GetInterval().AsDuration(), zoneLabel(s.loc), tmpl.body)

	// Build the client before the precise wait so nothing stands between
	// wake-up and the first request.
	client := newClient(int(req.GetNumCalls()))

	if launchAt := req.GetLaunchAt(); launchAt != nil {
		if err := waitUntil(ctx, log, launchAt.AsTime()); err != nil {
			return nil, err
		}
	}

	summary := runBurst(ctx, log, client, tmpl, int(req.GetNumCalls()), req.GetInterval().AsDuration())

	resp := &launcherv1.LaunchResponse{
		Won:             summary.won,
		WinningId:       int32(summary.winID),
		WinningStatus:   int32(summary.winStatus),
		WinningBody:     summary.winBody,
		Fired:           int32(summary.fired),
		AbortedInflight: int32(summary.aborted),
		SkippedPending:  int32(summary.skipped),
		MeanJitter:      durationpb.New(summary.meanJitter),
		MaxJitter:       durationpb.New(summary.maxJitter),
		LogFile:         log.path,
	}
	return resp, nil
}

// templateFromRequest validates a LaunchRequest and converts it into the
// rawRequest the runner consumes. Same conventions as the file parsers:
// Content-Length is dropped (Go sets it from the body) and Host is left to
// the URL.
func templateFromRequest(req *launcherv1.LaunchRequest) (*rawRequest, error) {
	method := strings.TrimSpace(req.GetMethod())
	if method == "" {
		return nil, status.Error(codes.InvalidArgument, "method must not be empty")
	}
	u, err := url.Parse(req.GetUrl())
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, status.Errorf(codes.InvalidArgument, "url %q is not a valid absolute URL", req.GetUrl())
	}
	if req.GetNumCalls() <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "num_calls must be > 0, got %d", req.GetNumCalls())
	}
	if d := req.GetInterval(); d != nil {
		if err := d.CheckValid(); err != nil || d.AsDuration() < 0 {
			return nil, status.Errorf(codes.InvalidArgument, "interval must be >= 0, got %v", d.AsDuration())
		}
	}

	header := make(http.Header)
	for _, h := range req.GetHeaders() {
		// Go's http.Client sets Content-Length from the body; a stale value
		// would break the request (same rule as the parsers).
		if strings.EqualFold(h.GetName(), "Content-Length") {
			continue
		}
		header.Add(h.GetName(), h.GetValue())
	}

	return &rawRequest{method: method, url: req.GetUrl(), header: header, body: req.GetBody()}, nil
}

// waitUntil blocks until the absolute launch instant, with the same
// two-phase sleep + busy-spin precision as the CLI countdown but driven by
// the RPC context instead of a console display: a client disconnect while
// waiting aborts the run before it starts. A target in the past returns
// immediately (catch-up, never drop).
func waitUntil(ctx context.Context, log *logger, target time.Time) error {
	log.logf("scheduled launch: %s (%s UTC)",
		target.In(log.loc).Format("2006-01-02 15:04:05 MST"),
		target.UTC().Format("2006-01-02 15:04:05"))

	if remaining := time.Until(target); remaining > spinMargin {
		t := time.NewTimer(remaining - spinMargin)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return status.Error(codes.Canceled, "run cancelled while waiting for the launch time")
		}
	}
	for time.Now().Before(target) {
	}

	log.logf("launching now, fired at %s", log.stamp(time.Now()))
	return nil
}
