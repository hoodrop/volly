package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// spinMargin is how long before an absolute deadline we stop sleeping
// and start busy-waiting. Go's time.Sleep guarantees *at least* the
// requested duration and can wake a few ms late, so the last stretch
// before any precise deadline is handled by spinning on the wall clock.
const spinMargin = 2 * time.Millisecond

// runner holds the state shared by all request goroutines.
type runner struct {
	ctx     context.Context
	cancel  context.CancelFunc
	client  *http.Client
	tmpl    *rawRequest
	log     *logger
	jitters chan<- time.Duration

	won     atomic.Int32 // set to 1 by the first goroutine that sees a 200
	aborted atomic.Int32 // in-flight requests killed by the cancel
	skipped atomic.Int32 // pending launches that never fired due to the cancel
}

// doRequest waits for its absolute target time, fires, and logs the full
// record (target time, actual fire time, status, latency, body). The first
// 200 response wins: it cancels the context, which aborts every in-flight
// request and makes every pending launch return without firing.
func (r *runner) doRequest(id int, target time.Time) {
	// Phase 1 — sleep until just before target, waking early if the run
	// was already won while we slept.
	if remaining := time.Until(target); remaining > spinMargin {
		t := time.NewTimer(remaining - spinMargin)
		select {
		case <-t.C:
		case <-r.ctx.Done():
			t.Stop()
			r.skipped.Add(1)
			return
		}
	}

	// Phase 2 — busy-spin the final stretch, bailing out early on a win
	// (an atomic load per iteration costs ~1ns and doesn't loosen the spin).
	for time.Now().Before(target) {
		if r.won.Load() == 1 {
			r.skipped.Add(1)
			return
		}
	}

	fire := time.Now()
	r.jitters <- fire.Sub(target)

	req, err := http.NewRequestWithContext(r.ctx, r.tmpl.method, r.tmpl.url, bytes.NewBufferString(r.tmpl.body))
	if err != nil {
		r.log.logf("[%03d] build error: %v", id, err)
		return
	}
	// Clone headers so each goroutine has its own independent set.
	for name, values := range r.tmpl.header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}

	resp, err := r.client.Do(req)
	if err != nil {
		if r.ctx.Err() != nil {
			// Aborted by the winner — expected, keep the log clean.
			r.aborted.Add(1)
			return
		}
		r.log.logf("[%03d] target=%s fire=%s error=%v took=%v",
			id, r.log.stamp(target), r.log.stamp(fire), err, time.Since(fire))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	r.log.logf("[%03d] target=%s fire=%s status=%d took=%v body=%s",
		id, r.log.stamp(target), r.log.stamp(fire), resp.StatusCode, time.Since(fire), string(body))

	if resp.StatusCode == http.StatusOK && r.won.CompareAndSwap(0, 1) {
		r.log.logf("[%03d] first 200 — cancelling all remaining requests", id)
		r.cancel()
	}
}

// sleepUntilPrecise blocks until the absolute target instant. It sleeps in
// one shot until spinMargin before the target, then busy-spins the rest of
// the way against the wall clock — landing within microseconds of target
// instead of the ~1ms overshoot a bare time.Sleep can incur. A target in
// the past returns immediately (catch-up, never drop).
func sleepUntilPrecise(target time.Time) {
	if remaining := time.Until(target); remaining > spinMargin {
		time.Sleep(remaining - spinMargin)
	}
	for time.Now().Before(target) {
	}
}

// countdownUntil blocks, printing a live countdown, and returns as close to
// the target instant as possible. The wait is done in two phases:
//
//  1. Coarse: refresh the display every ~250ms until ~1.5s remain.
//  2. Precise: sleepUntilPrecise (sleep + final busy-spin) for the rest.
func countdownUntil(log *logger, target time.Time) {
	log.logf("scheduled launch: %s (%s UTC)",
		target.In(log.loc).Format("2006-01-02 15:04:05 MST"),
		target.UTC().Format("2006-01-02 15:04:05"))

	// Phase 1 — coarse display loop.
	for {
		remaining := time.Until(target)
		if remaining <= 1500*time.Millisecond {
			break
		}
		fmt.Printf("\rLaunching in %s...   ", remaining.Round(time.Second))
		time.Sleep(250 * time.Millisecond)
	}

	// Phase 2 — sleep + busy-spin for sub-millisecond precision.
	sleepUntilPrecise(target)

	log.logf("launching now, fired at %s", log.stamp(time.Now()))
}
