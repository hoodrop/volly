package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
	// Embed the IANA tz database so zone names like "Asia/Singapore" resolve
	// even where the host has no system tzdata (static binary, minimal container).
	_ "time/tzdata"
)

func main() {
	cfg, err := loadConfig(configFile)
	if err != nil {
		fmt.Printf("config: %v\n", err)
		return
	}
	if err := cfg.validate(); err != nil {
		fmt.Printf("config: %v\n", err)
		return
	}

	loc, err := cfg.location()
	if err != nil {
		fmt.Printf("config: %v\n", err)
		return
	}

	// Two modes: no argument is the interactive CLI below; "serve" runs the
	// gRPC API for programmatic callers (see server.go).
	if len(os.Args) > 1 {
		if os.Args[1] == "serve" {
			serve(cfg, loc)
		} else {
			fmt.Printf("unknown argument %q (the only supported argument is \"serve\")\n", os.Args[1])
		}
		return
	}

	tmpl, format, err := loadRequest(cfg.RequestFile, cfg.RequestFormat)
	if err != nil {
		fmt.Printf("failed to load/parse %s: %v\n", cfg.RequestFile, err)
		return
	}

	fmt.Printf("Parsed request (%s): %s %s\n", format, tmpl.method, tmpl.url)
	fmt.Printf("Body: %s\n", tmpl.body)
	fmt.Printf("Headers: %d\n\n", len(tmpl.header))

	reader := bufio.NewReader(os.Stdin)

	launchAt, scheduled := promptLaunchTime(reader, loc)
	numCalls := promptInt(reader, "Total request amount", cfg.NumCalls)
	interval := promptDuration(reader, "Interval between launches (ms)", time.Duration(cfg.IntervalMs)*time.Millisecond)

	// From here on, every noteworthy record goes to stdout AND a fresh
	// timestamped file under the log dir.
	log, err := newLogger(cfg.LogDir, loc)
	if err != nil {
		fmt.Printf("cannot set up logging: %v\n", err)
		return
	}
	fmt.Printf("logging to %s\n", log.path)
	log.logf("config: %s %s calls=%d interval=%v timezone=%s format=%s body=%s",
		tmpl.method, tmpl.url, numCalls, interval, zoneLabel(loc), format, tmpl.body)

	// In-code `chrt -f … taskset -c …`: pin and go real-time before the
	// countdown, well ahead of the first launch. Linux-only, best-effort.
	applyScheduling(cfg, log.logf)

	// Build the client before the countdown so nothing stands between the
	// precise wake-up and the first request being fired.
	client := newClient(numCalls)

	if scheduled {
		countdownUntil(log, launchAt)
	}

	runBurst(context.Background(), log, client, tmpl, numCalls, interval)

	log.close()
}

// newClient builds the HTTP client for one burst, sized so every request in
// the burst can have its own keep-alive connection.
func newClient(numCalls int) *http.Client {
	return &http.Client{
		Timeout: 180 * time.Second,
		Transport: &http.Transport{
			// Proxy:               http.ProxyFromEnvironment, // HTTPS_PROXY=http://127.0.0.1:9090 to route via a local proxy
			MaxIdleConns:        numCalls,
			MaxIdleConnsPerHost: numCalls,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     90 * time.Second,
			DisableKeepAlives:   false,
		},
	}
}

// burstSummary is the outcome of one runBurst call, shared by the CLI (which
// only logs it) and the gRPC API (which maps it onto LaunchResponse).
type burstSummary struct {
	won       bool
	winID     int    // 1-based index of the request that got the first 200
	winStatus int    // its status code
	winBody   string // its response body

	fired   int // requests actually launched
	aborted int // in-flight requests killed by the winner's cancel
	skipped int // pending launches that never fired due to the cancel

	meanJitter time.Duration // fire time minus target time, over fired requests
	maxJitter  time.Duration
}

// runBurst fires numCalls copies of tmpl at absolute times
// start + (i-1)*interval and blocks until the first 200 cancels the rest or
// every request has completed. It logs the per-run summary lines and returns
// them in structured form. Cancelling ctx cancels the run.
func runBurst(ctx context.Context, log *logger, client *http.Client, tmpl *rawRequest, numCalls int, interval time.Duration) burstSummary {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jitters := make(chan time.Duration, numCalls)
	r := &runner{
		ctx:     ctx,
		cancel:  cancel,
		client:  client,
		tmpl:    tmpl,
		log:     log,
		jitters: jitters,
	}

	// Absolute-time scheduling: request i is due at start + (i-1)*interval.
	// Every launch is aimed at the wall clock rather than at "interval after
	// the previous launch", so per-iteration overhead and timer overshoot
	// can't accumulate — one late wake-up doesn't push everything later.
	//
	// Each goroutine performs the precise wait itself, so the moment the
	// busy-spin exits, client.Do runs with no scheduler handoff in between.
	start := time.Now()

	var wg sync.WaitGroup
	for i := 1; i <= numCalls; i++ {
		target := start.Add(time.Duration(i-1) * interval)
		wg.Add(1)
		go func(id int, target time.Time) {
			defer wg.Done()
			r.doRequest(id, target)
		}(i, target)
	}
	wg.Wait()
	close(jitters)

	fired := 0
	var maxJitter, totalJitter time.Duration
	for j := range jitters {
		fired++
		if j > maxJitter {
			maxJitter = j
		}
		totalJitter += j
	}

	summary := burstSummary{
		won:       r.won.Load() == 1,
		fired:     fired,
		aborted:   int(r.aborted.Load()),
		skipped:   int(r.skipped.Load()),
		maxJitter: maxJitter,
	}
	if fired > 0 {
		summary.meanJitter = totalJitter / time.Duration(fired)
	}
	if summary.won {
		summary.winID = r.winID
		summary.winStatus = r.winStatus
		summary.winBody = r.winBody
	}

	if summary.won {
		log.logf("done: stopped early on first 200 — fired=%d aborted-inflight=%d skipped-pending=%d",
			fired, summary.aborted, summary.skipped)
	} else {
		log.logf("done: all %d requests fired, no 200 seen", fired)
	}
	if fired > 0 {
		log.logf("launch jitter: mean=%v max=%v over %d fired",
			summary.meanJitter, maxJitter, fired)
	}
	return summary
}
