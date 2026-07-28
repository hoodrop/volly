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

	// Build the client before the countdown so nothing stands between the
	// precise wake-up and the first request being fired.
	client := &http.Client{
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if scheduled {
		countdownUntil(log, launchAt)
	}

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

	if r.won.Load() == 1 {
		log.logf("done: stopped early on first 200 — fired=%d aborted-inflight=%d skipped-pending=%d",
			fired, r.aborted.Load(), r.skipped.Load())
	} else {
		log.logf("done: all %d requests fired, no 200 seen", fired)
	}
	if fired > 0 {
		log.logf("launch jitter: mean=%v max=%v over %d fired",
			totalJitter/time.Duration(fired), maxJitter, fired)
	}

	log.close()
}
